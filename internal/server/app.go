package server

import (
	"context"
	"database/sql"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	accountdata "zcode2api/internal/server/account"
	settings "zcode2api/internal/server/config"
	"zcode2api/internal/server/fault"
	"zcode2api/internal/server/shared"
	"zcode2api/internal/server/storage"
)

type accountState struct {
	Inflight        int
	LastUsed        int64
	BusyUntil       int64
	BusyGeneration  int64
	BusyProbe       bool
	BusyBackoffStep int
	QuotaExhausted  bool
}

type stickyEntry struct {
	Account string
	At      int64
}

type responseEntry struct {
	Key      string
	Affinity string
	Input    []any
	Response shared.M
	Bytes    int
}

type profileEntry struct {
	Key   string
	Name  any
	Until int64
	Done  chan struct{}
}

type App struct {
	db                     *sql.DB
	data, secret           string
	root                   string
	version                string
	log                    *log.Logger
	logfile                *os.File
	web                    fs.FS
	configMu               sync.RWMutex
	cfg                    shared.M
	mu                     sync.Mutex
	states                 map[string]*accountState
	upstreamCooldowns      map[string]*upstreamCooldown
	cooldownGeneration     uint64
	available              chan struct{}
	scheduleMu             sync.Mutex
	waiters                []*accountWaiter
	sticky                 map[string]stickyEntry
	sessions               map[string]int64
	responses              map[string]responseEntry
	responseOrder          []string
	responseBytes          int
	clients                map[string]*http.Client
	jars                   map[string]*diskJar
	claimLocks             map[string]bool
	claimBatchMu           sync.Mutex
	claimCandidatesMu      sync.Mutex
	manualClaimSlots       chan int
	claimReschedule        chan struct{}
	claimScheduleCancel    context.CancelFunc
	flowLocks              map[string]bool
	localImportMu          sync.Mutex
	credentialMu           sync.Mutex
	businessRefreshes      map[string]*businessTokenFlight
	reauth                 map[string]int64
	pollDue                map[string]int64
	polling                map[string]bool
	pollWake               chan struct{}
	balances               balanceManager
	profiles               map[string]*profileEntry
	ctx                    context.Context
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	webPort, gatewayPort   int
	origin                 string
	captcha                captchaManager
	claimBrowser           claimBrowserManager
	manualClaimBrowsers    [manualClaimConcurrency]claimBrowserManager
	scheduledClaimBrowsers [dailyClaimConcurrency]claimBrowserManager
	proxies                outboundProxyManager
	proxyHealthOnce        sync.Once
	proxyHealthReschedule  chan struct{}
	mihomoInstallMu        sync.Mutex
	updates                updateManager
	zcodeVersionMu         sync.Mutex
	zcodeVersionReschedule chan struct{}
}

func New(root, data string, web fs.FS, buildVersion string) (*App, error) {
	cfg, e := settings.Load(root, data)
	if e != nil {
		return nil, e
	}
	secret, e := accountdata.LoadSecret(data)
	if e != nil {
		return nil, e
	}
	d, e := storage.Open(data)
	if e != nil {
		return nil, e
	}
	_ = os.MkdirAll(filepath.Join(data, "logs"), 0700)
	lf, _ := os.OpenFile(filepath.Join(data, "logs", "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	var out io.Writer = os.Stdout
	if lf != nil {
		out = io.MultiWriter(os.Stdout, lf)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{db: d, data: data, secret: secret, log: log.New(out, "", log.LstdFlags), logfile: lf, web: web, cfg: cfg, ctx: ctx, cancel: cancel, states: map[string]*accountState{}, sticky: map[string]stickyEntry{}, sessions: map[string]int64{}, responses: map[string]responseEntry{}, clients: map[string]*http.Client{}, jars: map[string]*diskJar{}, claimLocks: map[string]bool{}, claimReschedule: make(chan struct{}, 1), flowLocks: map[string]bool{}, reauth: map[string]int64{}, pollDue: map[string]int64{}, polling: map[string]bool{}, origin: "https://zcode.z.ai"}
	a.root = root
	a.version = buildVersion
	a.zcodeVersionReschedule = make(chan struct{}, 1)
	a.proxyHealthReschedule = make(chan struct{}, 1)
	a.pollWake = make(chan struct{}, 1)
	saved := shared.Obj(a.setting("config_overrides", shared.M{}))
	migrateHTTPVersion := !shared.Truth(a.setting("http_client_version_3_17_2_migrated", false))
	migrateBalancePoll := !shared.Truth(a.setting("balance_poll_30s_migrated", false))
	clean := settings.KnownConfig(cfg, saved)
	for k, v := range clean {
		if k == "http" || k == "retry" || k == "outbound_proxy" {
			v = settings.DeepMerge(shared.Obj(cfg[k]), shared.Obj(v), false)
		}
		a.cfg[k] = v
	}
	a.cfg["inference_driver"] = "http"
	if clean["inference_driver"] != nil {
		clean["inference_driver"] = "http"
	}
	// Upgrade the old default once, preserving later edits and other custom versions.
	if migrateHTTPVersion && shared.Str(shared.Obj(a.cfg["http"])["client_version"]) == "0.17.0" {
		shared.Obj(a.cfg["http"])["client_version"] = settings.DefaultHTTPClientVersion
		httpOverrides := shared.Obj(clean["http"])
		httpOverrides["client_version"] = settings.DefaultHTTPClientVersion
		clean["http"] = httpOverrides
	}
	// Migrate the previous five-second default once. Preserve custom intervals
	// and allow later explicit edits, including opting back into five seconds.
	if migrateBalancePoll && shared.Num(a.cfg["poll_interval_sec"]) == 5 {
		a.cfg["poll_interval_sec"] = settings.DefaultBalancePollInterval
		clean["poll_interval_sec"] = settings.DefaultBalancePollInterval
	}
	if shared.JSON(clean) != shared.JSON(saved) {
		if e = a.setSetting("config_overrides", clean); e != nil {
			a.Close()
			return nil, e
		}
	}
	if migrateHTTPVersion {
		if e = a.setSetting("http_client_version_3_17_2_migrated", true); e != nil {
			a.Close()
			return nil, e
		}
	}
	if migrateBalancePoll {
		if e = a.setSetting("balance_poll_30s_migrated", true); e != nil {
			a.Close()
			return nil, e
		}
	}
	a.ensureGroups()
	a.restoreProxyState()
	a.background(a.runAccountQueue)
	return a, nil
}

func (a *App) config(keys ...string) shared.M {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	if len(keys) == 0 {
		return shared.Clone(a.cfg)
	}
	out := make(shared.M, len(keys))
	for _, key := range keys {
		out[key] = shared.CloneValue(a.cfg[key])
	}
	return out
}

func (a *App) configValue(path ...string) any {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	var value any = a.cfg
	for _, key := range path {
		value = shared.Obj(value)[key]
	}
	return shared.CloneValue(value)
}

func (a *App) Close() {
	a.cancel()
	a.closeClaimBrowser()
	a.closeOutboundProxies()
	a.wg.Wait()
	a.mu.Lock()
	for _, c := range a.clients {
		c.CloseIdleConnections()
	}
	a.mu.Unlock()
	_ = a.db.Close()
	if a.logfile != nil {
		a.logfile.Close()
	}
}

func (a *App) background(f func()) { a.wg.Go(f) }

func (a *App) ensureGroups() {
	if a.row("SELECT id FROM groups WHERE id='g_main'") == nil {
		legacy := a.rows("SELECT id FROM groups WHERE id IN ('g_domestic','g_intl','g_backup')")
		if len(legacy) > 0 {
			_ = a.exec("INSERT INTO groups(id,name,created_at) VALUES('g_main','默认池',?)", shared.NowMS())
			_ = a.exec("UPDATE accounts SET group_id='g_main' WHERE group_id IN ('g_domestic','g_intl','g_backup')")
			_ = a.exec("DELETE FROM groups WHERE id IN ('g_domestic','g_intl','g_backup')")
		}
	}
	if len(a.rows("SELECT id FROM groups")) == 0 {
		_ = a.exec("INSERT INTO groups(id,name,created_at) VALUES('g_main','默认池',?)", shared.NowMS())
	}
}

func (a *App) Run(ctx context.Context) error {
	cfg := a.config()
	host := shared.Str(cfg["host"])
	gwHost, webHost := host, host
	if shared.Truth(cfg["bind_public"]) {
		gwHost = "0.0.0.0"
		if shared.Truth(cfg["admin_bind_public"]) {
			webHost = gwHost
		}
	}
	wl, e := net.Listen("tcp", net.JoinHostPort(webHost, shared.Str(cfg["web_port"])))
	if e != nil {
		return e
	}
	gl, e := net.Listen("tcp", net.JoinHostPort(gwHost, shared.Str(cfg["gateway_port"])))
	if e != nil {
		wl.Close()
		return e
	}
	a.webPort = wl.Addr().(*net.TCPAddr).Port
	a.gatewayPort = gl.Addr().(*net.TCPAddr).Port
	w := &http.Server{Handler: a.AdminHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	g := &http.Server{Handler: a.GatewayHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	info := shared.M{"web_port": a.webPort, "gateway_port": a.gatewayPort, "pid": os.Getpid(), "backend": "go", "updated_at": time.Now().UTC().Format(time.RFC3339)}
	if e = os.WriteFile(filepath.Join(a.data, "ports.json"), []byte(shared.JSON(info)), 0600); e != nil {
		wl.Close()
		gl.Close()
		return e
	}
	a.log.Printf("Go HTTP 服务已启动 Web: http://%s:%d 网关: http://%s:%d/v1 管理密码文件: %s", host, a.webPort, host, a.gatewayPort, filepath.Join(a.data, "admin-password.txt"))
	a.audit("system", "startup", info, "")
	a.startWorkers()
	errs := make(chan error, 2)
	go func() { errs <- w.Serve(wl) }()
	go func() { errs <- g.Serve(gl) }()
	select {
	case <-ctx.Done():
	case e = <-errs:
		if e == http.ErrServerClosed {
			e = nil
		}
	}
	a.cancel()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = w.Shutdown(shutdown)
	_ = g.Shutdown(shutdown)
	_ = w.Close()
	_ = g.Close()
	return e
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, shared.JSON(v))
}

func readBody(w http.ResponseWriter, r *http.Request) (shared.M, error) {
	defer r.Body.Close()
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if e != nil {
		return nil, &fault.Failure{Status: 413, Kind: "invalid", Code: "body_too_large", Message: "请求体超过限制"}
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return shared.M{}, nil
	}
	v := shared.Parse(string(b), nil)
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fault.Invalid("请求体必须为 JSON 对象")
	}
	return m, nil
}

func (a *App) ip(r *http.Request) string {
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	if shared.Truth(a.configValue("trust_proxy")) {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	return ip
}

func (a *App) health(w http.ResponseWriter, r *http.Request, scope string) bool {
	if r.Method != "GET" {
		return false
	}
	switch r.URL.Path {
	case "/healthz":
		writeJSON(w, 200, shared.M{"ok": true, "scope": scope, "version": a.version, "inference_driver": "http", "backend": "go", "ts": shared.NowMS()})
		return true
	case "/readyz":
		n := shared.Integer(a.row("SELECT COUNT(*) n FROM accounts WHERE status='active'")["n"])
		status := 200
		if n == 0 {
			status = 503
		}
		writeJSON(w, status, shared.M{"ok": n > 0, "scope": scope, "active_accounts": n, "inference_driver": "http"})
		return true
	}
	return false
}

func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				a.log.Printf("request panic: %v", v)
				writeJSON(w, 500, shared.M{"ok": false, "error": "内部服务错误"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) scheduleNext(id string, delay time.Duration) {
	a.mu.Lock()
	due := shared.NowMS() + delay.Milliseconds()
	if pending, exists := a.pollDue[id]; !exists || due < pending {
		a.pollDue[id] = due
		shared.Wake(a.pollWake)
	}
	a.mu.Unlock()
}

// Operations stop when either their caller or the service is canceled; zero disables the timeout.
func (a *App) operationContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	stop := context.AfterFunc(a.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (a *App) account(id string) shared.M { return a.row("SELECT * FROM accounts WHERE id=?", id) }

func (a *App) groups() []any { return a.rows("SELECT * FROM groups ORDER BY created_at") }

func (a *App) defaultChain() []any {
	out := []any{}
	groups := a.groups()
	for _, role := range []string{"primary", "backup"} {
		for _, v := range groups {
			g := shared.Obj(v)
			if shared.Truth(g["enabled"]) && shared.Str(g["role"]) == role {
				out = append(out, g["id"])
				break
			}
		}
	}
	return out
}
