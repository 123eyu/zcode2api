package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
	"zcode2api/internal/server/fault"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

type proxyProbeScope struct {
	key        string
	generation uint64
}

type proxyProbeResult struct {
	verdict       string
	retryAfter    time.Duration
	observedRetry time.Time
}

func (a *App) startProxyHealthProbes() {
	a.proxyHealthOnce.Do(func() {
		a.background(func() {
			for a.ctx.Err() == nil {
				// Install cancellation before settings can replace this configuration.
				a.configMu.RLock()
				options := shared.Merge(routing.ProxyDefaults(), shared.Obj(a.cfg["outbound_proxy"]))
				active := shared.Truth(options["enabled"]) && options["source"] == "manual"
				ctx, cancel := context.WithCancel(a.ctx)
				a.proxies.mu.Lock()
				if active {
					a.proxies.configureLocked(options, routing.ManualProxyEndpoints(shared.Arr(options["urls"])))
				}
				scope := proxyProbeScope{a.proxies.key, a.proxies.generation}
				a.proxies.probeCancel = cancel
				a.proxies.mu.Unlock()
				a.configMu.RUnlock()
				if active {
					a.probeManualExits(ctx, options, scope)
				}
				cancel()
				timer := time.NewTimer(5 * time.Minute)
				select {
				case <-a.ctx.Done():
					timer.Stop()
					return
				case <-a.proxyHealthReschedule:
					timer.Stop()
				case <-timer.C:
				}
			}
		})
	})
}

func (a *App) probeManualExits(ctx context.Context, options shared.M, scope proxyProbeScope) {
	endpoints := routing.ManualProxyEndpoints(shared.Arr(options["urls"]))
	shared.ParallelFor(len(endpoints), 4, func(i int) {
		endpoint := endpoints[i]
		if ctx.Err() == nil && routing.ValidProxy(endpoint.URL) {
			result := a.probeManualExit(ctx, endpoint.URL)
			a.applyExitVerdict(ctx, scope, endpoint.Identity(), result)
		}
	})
}

func (a *App) probeManualExit(parent context.Context, address string) proxyProbeResult {
	a.proxies.mu.Lock()
	observedRetry := a.proxies.retryAt[address]
	a.proxies.mu.Unlock()
	status, failure, err := a.probeProxy(parent, address)
	if err != nil {
		if status == 0 && failure != nil && routing.ProxyExitFailure(failure) {
			return proxyProbeResult{verdict: "quarantine"}
		}
		return proxyProbeResult{}
	}
	switch {
	case failure.Code == "upstream_waf_blocked":
		return proxyProbeResult{verdict: "blocked", retryAfter: failure.RetryAfter}
	case routing.ProxyExitFailure(failure):
		return proxyProbeResult{verdict: "quarantine", retryAfter: failure.RetryAfter}
	case status == http.StatusUnauthorized:
		return proxyProbeResult{verdict: "reachable", observedRetry: observedRetry}
	default:
		return proxyProbeResult{}
	}
}

// Share anonymous probes without changing proxy state or sending credentials.
func (a *App) probeProxy(parent context.Context, address string) (int, *fault.Failure, error) {
	ctx, cancel := context.WithTimeout(routing.PinnedProxyContext(parent, address), 8*time.Second)
	defer cancel()
	client, err := a.client(ctx)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", a.origin+"/api/v1/zcode-plan/anthropic/v1/messages", strings.NewReader("{}"))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, routing.TransportFailure(ctx, err), err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	return response.StatusCode, fault.UpstreamHTTPFailure(response, body), err
}

func (a *App) applyExitVerdict(ctx context.Context, scope proxyProbeScope, identity string, result proxyProbeResult) {
	if identity == "" || result.verdict == "" || ctx.Err() != nil {
		return
	}
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	options := shared.Merge(routing.ProxyDefaults(), shared.Obj(a.cfg["outbound_proxy"]))
	if !shared.Truth(options["enabled"]) || options["source"] != "manual" || shared.Hash(shared.JSON(options)) != scope.key {
		return
	}
	a.proxies.mu.Lock()
	defer a.proxies.mu.Unlock()
	if a.proxies.closing || ctx.Err() != nil || a.proxies.key != scope.key || a.proxies.generation != scope.generation {
		return
	}
	a.proxies.initStateLocked()
	present := false
	for _, endpoint := range a.proxies.endpoints {
		present = present || endpoint.Identity() == identity
	}
	if !present {
		return
	}
	now := time.Now()
	changed := false
	switch result.verdict {
	case "reachable":
		if a.proxies.retryAt[identity] != result.observedRetry {
			return // A newer connection failure supersedes this probe.
		}
		changed = !a.proxies.retryAt[identity].IsZero()
		delete(a.proxies.retryAt, identity)
	case "blocked":
		until := now.Add(max(10*time.Minute, result.retryAfter))
		if until.After(a.proxies.blocked[identity]) {
			a.proxies.blocked[identity] = until
			changed = true
		}
	case "quarantine":
		until := now.Add(max(time.Minute, result.retryAfter))
		if until.After(a.proxies.retryAt[identity]) {
			a.proxies.retryAt[identity] = until
			changed = true
		}
	}
	if changed {
		a.persistProxyStateLocked()
	}
}

func (a *App) testOutboundProxies(parent context.Context, options shared.M) (shared.M, error) {
	if err := routing.ValidateOutboundProxy(options); err != nil {
		return nil, err
	}
	options = shared.Merge(routing.ProxyDefaults(), options)
	ctx, cancel := a.operationContext(parent, 45*time.Second)
	defer cancel()
	endpoints := []routing.ProxyEndpoint{{URL: "", Name: "直连"}}
	if shared.Truth(options["enabled"]) {
		endpoints = nil
		if options["source"] == "subscription" {
			runtime, err := a.startManagedProxy(ctx, options)
			if err != nil {
				return nil, err
			}
			defer runtime.stop()
			endpoints = runtime.endpoints
		} else {
			endpoints = routing.ManualProxyEndpoints(shared.Arr(options["urls"]))
		}
	}
	results := make([]any, len(endpoints))
	shared.ParallelFor(len(endpoints), 4, func(index int) {
		endpoint := endpoints[index]
		started := time.Now()
		result := shared.M{"name": endpoint.Name, "reachable": false, "transport_reachable": false, "failure_kind": "network", "message": "连接失败或超时"}
		status, failure, _ := a.probeProxy(ctx, endpoint.URL)
		if status != 0 {
			result["transport_reachable"] = true
			result["status"] = status
			fault.Classify(failure)
			blocked := failure.Code == "upstream_waf_blocked" || failure.Kind == "account_risk"
			reachable := status == 401 && !blocked
			result["reachable"] = reachable
			result["blocked"] = blocked
			result["code"], result["failure_kind"] = failure.Code, failure.Kind
			if reachable {
				result["failure_kind"], result["code"] = "", ""
			}
			if failure.Kind == "account_risk" {
				// Anonymous probes cannot identify an affected account or exit.
				result["failure_kind"] = "risk"
				result["message"] = "上游风控拒绝"
				if failure.Code == "3012" {
					result["message"] = "上游风控拒绝（3012）"
				}
			} else if blocked {
				result["message"] = "上游安全网关拦截"
			} else if status == 401 {
				result["message"] = "出口可达；仍需用真实推理确认账号与模型可用"
			} else {
				result["message"] = "已连接，但上游未返回预期的鉴权响应"
			}
		} else if failure != nil {
			result["code"] = failure.Code
		}
		result["elapsed_ms"] = time.Since(started).Milliseconds()
		results[index] = result
	})
	return shared.M{"ok": true, "results": results, "note": "检测不保存设置、不发送账号令牌、不计模型用量；连通性通过不代表真实推理一定通过风控"}, nil
}

// Listing a draft does not start Mihomo, select a route or save configuration.
func (a *App) outboundProxyNodes(ctx context.Context, options shared.M) (shared.M, error) {
	if err := routing.ValidateOutboundProxy(options); err != nil {
		return nil, err
	}
	options = shared.Merge(routing.ProxyDefaults(), options)
	var endpoints []routing.ProxyEndpoint
	protocols := map[string]string{}
	if options["source"] == "subscription" {
		nodes, err := a.subscriptionNodes(ctx, options)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			endpoint := routing.ProxyEndpoint{Name: node.Name, ID: node.Identity()}
			endpoints = append(endpoints, endpoint)
			protocols[endpoint.SelectionID()] = strings.ToUpper(shared.Str(node.Config["type"]))
		}
	} else {
		endpoints = routing.ManualProxyEndpoints(shared.Arr(options["urls"]))
	}
	a.proxies.mu.Lock()
	defer a.proxies.mu.Unlock()
	active := a.proxies.key == shared.Hash(shared.JSON(options))
	rows := []any{}
	for _, endpoint := range endpoints {
		blocked := active && time.Now().Before(a.proxies.blocked[endpoint.Identity()])
		unavailable := active && time.Now().Before(a.proxies.retryAt[endpoint.Identity()])
		rows = append(rows, shared.M{
			"id": endpoint.SelectionID(), "name": endpoint.Name, "protocol": protocols[endpoint.SelectionID()],
			"current":     active && endpoint.Identity() == a.proxies.selected && !unavailable && !blocked,
			"unavailable": unavailable,
			"blocked":     blocked,
		})
	}
	return shared.M{"ok": true, "nodes": rows}, nil
}
