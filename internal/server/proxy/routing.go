package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"zcode2api/internal/server/fault"
	"zcode2api/internal/server/shared"
)

func ProxyDefaults() shared.M {
	return shared.M{"enabled": false, "rotation_enabled": false, "rotation_interval_sec": 0, "source": "manual", "urls": []any{}, "subscription_url": "", "subscription_filter": "", "preferred_node": "", "mihomo_path": "mihomo", "refresh_interval_sec": 3600}
}

func ValidateOutboundProxyFields(value any) error {
	options, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("outbound_proxy 必须为配置对象")
	}
	for key, value := range options {
		switch key {
		case "enabled", "rotation_enabled":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("outbound_proxy.%s 必须为布尔值", key)
			}
		case "source":
			if value != "manual" && value != "subscription" {
				return fmt.Errorf("代理来源须为 manual 或 subscription")
			}
		case "urls":
			list, ok := value.([]any)
			if !ok || len(list) > 64 {
				return fmt.Errorf("代理列表须为数组，最多 64 个地址")
			}
			for i, item := range list {
				address, ok := item.(string)
				if !ok || len(address) > 4096 || !ValidProxy(address) {
					return fmt.Errorf("第 %d 个代理地址无效，仅支持 http(s)://[用户名:密码@]主机:端口；订阅链接请使用订阅模式", i+1)
				}
			}
		case "subscription_url":
			address, ok := value.(string)
			if !ok || len(address) > 8192 || strings.ContainsAny(address, "\r\n\x00") {
				return fmt.Errorf("订阅地址无效")
			}
			if address != "" {
				u, err := url.Parse(address)
				if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
					return fmt.Errorf("订阅地址必须为 HTTPS URL，访问参数可放在查询字符串中")
				}
			}
		case "subscription_filter":
			pattern, ok := value.(string)
			if !ok || len(pattern) > 512 {
				return fmt.Errorf("节点筛选表达式过长或格式无效")
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("节点筛选须为有效的正则表达式")
			}
		case "preferred_node":
			identity, ok := value.(string)
			if !ok || identity != "" && !HexIDPattern.MatchString(identity) {
				return fmt.Errorf("指定代理节点标识无效，请重新选择")
			}
		case "mihomo_path":
			path, ok := value.(string)
			if !ok || strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\r\n\x00") {
				return fmt.Errorf("Mihomo 可执行文件路径无效")
			}
		case "refresh_interval_sec", "rotation_interval_sec":
			minimum, label := 60.0, "订阅刷新间隔"
			if key == "rotation_interval_sec" {
				minimum, label = 0, "代理轮换间隔"
			}
			if !shared.IntegerInRange(value, minimum, 86400) {
				return fmt.Errorf("%s须为 %.0f 到 86400 的整数秒", label, minimum)
			}
		default:
			return fmt.Errorf("未知代理设置 outbound_proxy.%s", key)
		}
	}
	return nil
}

func ValidateOutboundProxy(value any) error {
	if err := ValidateOutboundProxyFields(value); err != nil {
		return err
	}
	options := shared.Merge(ProxyDefaults(), shared.Obj(value))
	if shared.Truth(options["enabled"]) {
		if options["source"] == "manual" && len(shared.Arr(options["urls"])) == 0 {
			return fmt.Errorf("启用代理时至少填写一个 HTTP/HTTPS 代理地址")
		}
		if options["source"] == "subscription" && shared.Str(options["subscription_url"]) == "" {
			return fmt.Errorf("启用订阅代理时须填写订阅地址")
		}
	}
	return nil
}

type ProxyContextKey struct{}

type ProxyRoute struct{ URL string }

type ProxyEndpoint struct{ URL, Name, ID string }

func ManualProxyEndpoints(urls []any) []ProxyEndpoint {
	endpoints := []ProxyEndpoint{}
	seen := map[string]bool{}
	for _, value := range urls {
		address := shared.Str(value)
		if !seen[address] {
			endpoints = append(endpoints, ProxyEndpoint{URL: address, Name: maskedProxy(address)})
			seen[address] = true
		}
	}
	return endpoints
}

func (endpoint ProxyEndpoint) Identity() string {
	if endpoint.ID != "" {
		return endpoint.ID
	}
	return endpoint.URL
}

func (endpoint ProxyEndpoint) SelectionID() string {
	if endpoint.ID != "" {
		return endpoint.ID
	}
	return shared.Hash(endpoint.URL)
}

type ProxySelectionKey struct{}

type ProxySelection struct {
	Generation uint64 `json:"-"`
	Identity   string `json:"-"`
}

func SelectedProxy(ctx context.Context) string {
	if route, ok := ctx.Value(ProxyContextKey{}).(ProxyRoute); ok {
		return route.URL
	}
	return ""
}

func AsProxyFailure(err error) *fault.Failure {
	var failure *fault.Failure
	if errors.As(err, &failure) {
		return failure
	}
	return &fault.Failure{Kind: "server", Code: "proxy_unavailable", Message: "出站代理不可用，请检查代理设置或运行环境"}
}

func ProxyFailure(code, message string) error {
	return &fault.Failure{Kind: "server", Code: code, Message: message}
}

// Escalate repeated failures without shortening an existing cooldown or Retry-After.
func ProxyCooldownUntil(previous, now time.Time, minimum, maximum, retryAfter time.Duration) time.Time {
	delay := max(minimum, retryAfter)
	if previous.After(now) {
		delay = max(retryAfter, min(maximum, max(delay, previous.Sub(now)*2)))
	}
	return shared.MaxTime(previous, now.Add(delay))
}

// Account refusals, ticket and runtime failures do not identify a broken exit.
func ProxyExitFailure(failure *fault.Failure) bool {
	switch failure.Code {
	case "proxy_auth_failed", "proxy_connection_failed":
		return true
	}
	return false
}

// A failed request through a proxy does not by itself identify a failed proxy.
// Only connection errors naming the proxy hop can quarantine the route.
type ProxyDialError struct{ error }

func NewDialError(err error) error { return &ProxyDialError{err} }

func (e *ProxyDialError) Unwrap() error { return e.error }

func TransportFailure(ctx context.Context, err error) *fault.Failure {
	f := &fault.Failure{Kind: "network", Code: "network", Message: "上游连接失败", Retryable: true, Detail: shared.M{"message": fault.BoundedFailureText(err.Error())}}
	address := SelectedProxy(ctx)
	if address == "" || ctx.Err() != nil {
		return f
	}
	var dialError *ProxyDialError
	if errors.As(err, &dialError) {
		f.Code, f.Message = "proxy_connection_failed", "代理连接失败"
		return f
	}
	proxy, parseErr := url.Parse(address)
	if parseErr != nil {
		return f
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if cause.Error() == "Proxy Authentication Required" {
			f.Code, f.Message = "proxy_auth_failed", "代理认证失败"
			return f
		}
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		port := proxy.Port()
		if port == "" {
			port = "80"
			if proxy.Scheme == "https" {
				port = "443"
			}
		}
		proxyHop := operation.Op == "proxyconnect"
		if operation.Op == "dial" && operation.Addr != nil {
			host, targetPort, splitErr := net.SplitHostPort(operation.Addr.String())
			proxyHop = splitErr == nil && strings.EqualFold(host, proxy.Hostname()) && targetPort == port
		}
		var dns *net.DNSError
		if operation.Op == "dial" && errors.As(operation, &dns) && strings.EqualFold(dns.Name, proxy.Hostname()) {
			proxyHop = true
		}
		if proxyHop {
			f.Code, f.Message = "proxy_connection_failed", "代理连接失败"
		}
	}
	return f
}

func maskedProxy(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return "代理地址"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

func ValidProxy(s string) bool {
	u, e := url.Parse(s)
	if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, "\r\n\x00") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		return err == nil && n > 0 && n <= 65535
	}
	return !strings.HasSuffix(u.Host, ":")
}

var HexIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Background work must use the original request's exit.
func PinnedProxyContext(parent context.Context, proxyURL string) context.Context {
	return context.WithValue(parent, ProxyContextKey{}, ProxyRoute{URL: proxyURL})
}
