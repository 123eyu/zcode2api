package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"zcode2api/internal/server/fault"
	childproc "zcode2api/internal/server/process"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

type managedProxyRuntime struct {
	key        string
	endpoints  []routing.ProxyEndpoint
	expires    time.Time
	references int
	retired    bool
	done       chan struct{}
	stopOnce   sync.Once
	stopFn     func()
}

func (runtime *managedProxyRuntime) stop() { runtime.stopOnce.Do(runtime.stopFn) }

func (runtime *managedProxyRuntime) alive() bool {
	select {
	case <-runtime.done:
		return false
	default:
		return true
	}
}

func (a *App) acquireManagedProxy(ctx context.Context, options shared.M) (*managedProxyRuntime, error) {
	manager := &a.proxies
	manager.mu.Lock()
	if manager.closing || ctx.Err() != nil {
		manager.mu.Unlock()
		return nil, routing.ProxyFailure("proxy_canceled", "代理准备已取消")
	}
	key := shared.Hash(shared.JSON(options))
	current := manager.current
	if current != nil && current.key == key && current.alive() && time.Now().Before(current.expires) {
		current.references++
		manager.mu.Unlock()
		return current, nil
	}
	next, err := a.startManagedProxy(ctx, options)
	if err != nil {
		// A transient refresh failure must not discard a working configuration.
		// Changing the URL/filter/path never silently falls back to an old source.
		if current != nil && current.key == key && current.alive() {
			current.expires = time.Now().Add(time.Minute)
			current.references++
			manager.mu.Unlock()
			return current, nil
		}
		manager.mu.Unlock()
		return nil, err
	}
	next.key, next.references = key, 1
	manager.current = next
	if manager.runtimes == nil {
		manager.runtimes = map[*managedProxyRuntime]bool{}
	}
	manager.runtimes[next] = true
	stop := current != nil && current.references == 0
	if current != nil {
		current.retired = true
		if stop {
			delete(manager.runtimes, current)
		}
	}
	manager.mu.Unlock()
	if stop {
		current.stop()
	}
	return next, nil
}

func (a *App) releaseManagedProxy(runtime *managedProxyRuntime) {
	a.proxies.mu.Lock()
	runtime.references--
	stop := runtime.retired && runtime.references == 0
	if stop {
		delete(a.proxies.runtimes, runtime)
	}
	a.proxies.mu.Unlock()
	if stop {
		runtime.stop()
	}
}

func (a *App) subscriptionNodes(ctx context.Context, options shared.M) ([]routing.SubscriptionNode, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Subscription bootstrap never recursively uses this pool.
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 || request.URL.Scheme != "https" || request.URL.User != nil {
			return fmt.Errorf("subscription redirect rejected")
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, "GET", shared.Str(options["subscription_url"]), nil)
	if err != nil {
		return nil, routing.ProxyFailure("proxy_subscription_invalid", "订阅地址无效")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, routing.ProxyFailure("proxy_subscription_unavailable", "拉取订阅失败，请检查服务器网络及订阅地址")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		failure := &fault.Failure{Kind: "server", Code: "proxy_subscription_unavailable", Status: response.StatusCode,
			Message: fmt.Sprintf("代理订阅获取失败（HTTP %d）", response.StatusCode)}
		// Recognize only known authentication errors; never expose response bodies
		// or request errors, which can contain the private subscription URL.
		text := strings.ToLower(string(body))
		if response.StatusCode == http.StatusUnauthorized || (response.StatusCode == http.StatusForbidden &&
			(strings.Contains(text, "token is error") || strings.Contains(text, "invalid token") || strings.Contains(text, "token expired"))) {
			failure.Code = "proxy_subscription_auth_failed"
			failure.Message = fmt.Sprintf("订阅令牌错误或已失效（HTTP %d），请重新复制有效订阅链接", response.StatusCode)
		}
		return nil, failure
	}
	if readErr != nil {
		return nil, routing.ProxyFailure("proxy_subscription_unavailable", "读取代理订阅响应失败，请稍后重试")
	}
	if len(body) > 2<<20 {
		return nil, routing.ProxyFailure("proxy_subscription_response_too_large", "代理订阅响应超过 2 MB")
	}
	return routing.DecodeSubscription(body, shared.Str(options["subscription_filter"]))
}

func (a *App) startManagedProxy(ctx context.Context, options shared.M) (*managedProxyRuntime, error) {
	binary, err := a.resolveMihomo(shared.Str(options["mihomo_path"]))
	if err != nil {
		return nil, routing.ProxyFailure("proxy_runtime_missing", "未找到 Mihomo；可在设置中点击安装，或填写已有可执行文件路径")
	}
	nodes, err := a.subscriptionNodes(ctx, options)
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(a.data, "runtime")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return nil, routing.ProxyFailure("proxy_runtime_error", "无法创建代理运行目录")
	}
	directory, err := os.MkdirTemp(parent, "proxy-")
	if err != nil {
		return nil, routing.ProxyFailure("proxy_runtime_error", "无法创建代理运行目录")
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()
	proxies, listeners := []any{}, []any{}
	endpoints := []routing.ProxyEndpoint{}
	reserved := []net.Listener{}
	for i, node := range nodes {
		listener, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		if listenErr != nil {
			for _, open := range reserved {
				open.Close()
			}
			err = routing.ProxyFailure("proxy_runtime_error", "无法分配本机代理端口")
			return nil, err
		}
		reserved = append(reserved, listener)
		address := listener.Addr().(*net.TCPAddr)
		proxies = append(proxies, node.Config)
		// Each listener has one fixed outbound. A global load-balance listener
		// would let the SDK and Go's separate CONNECTs pick different exits.
		listeners = append(listeners, shared.M{"name": fmt.Sprintf("in-%d", i), "type": "http", "listen": "127.0.0.1", "port": address.Port, "proxy": node.Config["name"]})
		// Local ports change on refresh; node identity must survive that change.
		endpoints = append(endpoints, routing.ProxyEndpoint{URL: fmt.Sprintf("http://127.0.0.1:%d", address.Port), Name: node.Name, ID: node.Identity()})
	}
	config := shared.M{"allow-lan": false, "mode": "rule", "log-level": "silent", "ipv6": false, "listeners": listeners, "proxies": proxies, "rules": []any{"MATCH,REJECT"}}
	filename := filepath.Join(directory, "config.json")
	encoded, _ := json.Marshal(config)
	if err = os.WriteFile(filename, encoded, 0600); err != nil {
		for _, open := range reserved {
			open.Close()
		}
		err = routing.ProxyFailure("proxy_runtime_error", "无法写入代理运行配置")
		return nil, err
	}
	for _, open := range reserved {
		open.Close()
	}
	processCtx, cancel := context.WithCancel(a.ctx)
	command := exec.CommandContext(processCtx, binary, "-d", directory, "-f", filename)
	childproc.HideChildWindow(command)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.WaitDelay = 2 * time.Second
	if err = command.Start(); err != nil {
		cancel()
		err = routing.ProxyFailure("proxy_runtime_error", "无法启动 Mihomo，请检查可执行文件及平台架构")
		return nil, err
	}
	runtime := &managedProxyRuntime{endpoints: endpoints, expires: time.Now().Add(time.Duration(shared.Num(options["refresh_interval_sec"])) * time.Second), done: make(chan struct{})}
	go func() { command.Wait(); close(runtime.done) }()
	runtime.stopFn = func() {
		cancel()
		select {
		case <-runtime.done:
		case <-time.After(3 * time.Second):
			command.Process.Kill()
			<-runtime.done
		}
		a.forgetProxyClients(endpoints)
		os.RemoveAll(directory)
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	for {
		if !runtime.alive() {
			runtime.stop()
			return nil, routing.ProxyFailure("proxy_runtime_error", "Mihomo 启动失败，请检查订阅节点是否受当前内核支持")
		}
		ready := true
		for _, endpoint := range endpoints {
			u, _ := url.Parse(endpoint.URL)
			connection, dialErr := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(readyCtx, "tcp4", u.Host)
			if dialErr != nil {
				ready = false
				break
			}
			connection.Close()
		}
		if ready {
			return runtime, nil
		}
		select {
		case <-readyCtx.Done():
			runtime.stop()
			return nil, routing.ProxyFailure("proxy_runtime_error", "Mihomo 代理端口未能就绪")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (a *App) managedMihomoPath() string {
	name := "mihomo"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(a.data, "tools", "mihomo", routing.MihomoVersion, runtime.GOOS+"-"+runtime.GOARCH, name)
	if absolute, err := filepath.Abs(binary); err == nil {
		return absolute
	}
	return binary
}

func (a *App) resolveMihomo(configured string) (string, error) {
	if configured == "" || configured == "mihomo" {
		// Portable deployments may place Mihomo beside config.json.
		return childproc.ResolveExecutable(a.root, "mihomo", a.managedMihomoPath(), filepath.Join(a.root, "mihomo"))
	}
	return childproc.ResolveExecutable(a.root, configured)
}

func (a *App) mihomoStatus() shared.M {
	return a.mihomoStatusForPath(shared.Str(shared.Obj(a.configValue("outbound_proxy"))["mihomo_path"]))
}

func (a *App) mihomoStatusForPath(configured string) shared.M {
	_, supported := routing.LookupMihomoAsset(runtime.GOOS + "/" + runtime.GOARCH)
	binary, err := a.resolveMihomo(configured)
	return shared.M{"installed": err == nil, "path": binary, "managed": binary == a.managedMihomoPath(), "version": routing.MihomoVersion, "platform": runtime.GOOS + "/" + runtime.GOARCH, "supported": supported}
}

func (a *App) installMihomo(parent context.Context) (shared.M, error) {
	asset, supported := routing.LookupMihomoAsset(runtime.GOOS + "/" + runtime.GOARCH)
	if !supported {
		return nil, fmt.Errorf("当前平台暂不支持在线安装，请手动安装 Mihomo")
	}
	if !a.mihomoInstallMu.TryLock() {
		return nil, fmt.Errorf("Mihomo 正在安装，请稍后检查安装状态")
	}
	defer a.mihomoInstallMu.Unlock()
	ctx, cancel := a.operationContext(parent, 3*time.Minute)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Minute, CheckRedirect: routing.MihomoRedirect}
	binary := a.managedMihomoPath()
	if _, err := exec.LookPath(binary); err != nil {
		if err = routing.InstallMihomoAsset(ctx, client, asset, binary); err != nil {
			return nil, err
		}
	}
	return shared.M{"ok": true, "installed": true, "path": binary, "version": routing.MihomoVersion}, nil
}
