package server

import (
	"context"
	"sync"
	"time"
	"zcode2api/internal/server/fault"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

type outboundProxyManager struct {
	mu            sync.Mutex
	generation    uint64
	selected      string
	claimCursor   int
	endpoints     []routing.ProxyEndpoint
	key           string
	current       *managedProxyRuntime
	runtimes      map[*managedProxyRuntime]bool
	closing       bool
	retryAt       map[string]time.Time
	blocked       map[string]time.Time
	probeCancel   context.CancelFunc
	persistQueued bool
}

// Call with mu held; the zero-value manager and configuration resets are valid.
func (manager *outboundProxyManager) initStateLocked() {
	if manager.retryAt == nil {
		manager.retryAt = map[string]time.Time{}
	}
	if manager.blocked == nil {
		manager.blocked = map[string]time.Time{}
	}
}

func (manager *outboundProxyManager) configureLocked(options shared.M, endpoints []routing.ProxyEndpoint) {
	manager.initStateLocked()
	key := shared.Hash(shared.JSON(options))
	if key != manager.key {
		manager.key, manager.selected = key, ""
		manager.generation++
		manager.claimCursor = 0
		for _, endpoint := range endpoints {
			if endpoint.SelectionID() == shared.Str(options["preferred_node"]) {
				manager.selected = endpoint.Identity()
				break
			}
		}
	}
	manager.endpoints = append([]routing.ProxyEndpoint(nil), endpoints...)
	manager.pruneStateLocked(time.Now())
}

func (manager *outboundProxyManager) pruneStateLocked(now time.Time) {
	manager.initStateLocked()
	present := map[string]bool{}
	for _, endpoint := range manager.endpoints {
		present[endpoint.Identity()] = true
	}
	for identity, until := range manager.retryAt {
		if !present[identity] || !now.Before(until) {
			delete(manager.retryAt, identity)
		}
	}
	for identity, until := range manager.blocked {
		if !present[identity] || !now.Before(until) {
			delete(manager.blocked, identity)
		}
	}
}

// Pin scene lookup, SDK verification and inference to the same exit.
func (a *App) pinOutboundProxy(parent context.Context, account shared.M) (context.Context, func(), error) {
	return a.pinOutboundProxyMode(parent, account, false)
}

// Rotate per claim, keeping scene lookup, SDK verification and submission on the same exit.
func (a *App) pinClaimOutboundProxy(parent context.Context, account shared.M) (context.Context, func(), error) {
	return a.pinOutboundProxyMode(parent, account, true)
}

func (a *App) pinOutboundProxyMode(parent context.Context, account shared.M, rotatePerClaim bool) (context.Context, func(), error) {
	if _, present := parent.Value(routing.ProxyContextKey{}).(routing.ProxyRoute); present {
		return parent, func() {}, nil
	}
	ctx, cleanup := a.operationContext(parent, 0)
	address := shared.Str(account["proxy_url"])
	if address != "" {
		if !routing.ValidProxy(address) {
			cleanup()
			return parent, func() {}, routing.ProxyFailure("proxy_config_invalid", "账号代理地址无效")
		}
		return routing.PinnedProxyContext(ctx, address), cleanup, nil
	}
	options := shared.Obj(a.configValue("outbound_proxy"))
	if err := routing.ValidateOutboundProxy(options); err != nil {
		cleanup()
		return parent, func() {}, routing.ProxyFailure("proxy_config_invalid", err.Error())
	}
	options = shared.Merge(routing.ProxyDefaults(), options)
	if !shared.Truth(options["enabled"]) {
		return routing.PinnedProxyContext(ctx, ""), cleanup, nil
	}
	var endpoints []routing.ProxyEndpoint
	var runtime *managedProxyRuntime
	var err error
	if options["source"] == "subscription" {
		runtime, err = a.acquireManagedProxy(ctx, options)
		if err != nil {
			cleanup()
			return parent, func() {}, err
		}
		endpoints = runtime.endpoints
	} else {
		endpoints = routing.ManualProxyEndpoints(shared.Arr(options["urls"]))
	}
	var index int
	var generation uint64
	if rotatePerClaim && options["source"] == "manual" {
		index, generation = a.proxies.selectRotatingEndpoint(options, endpoints)
	} else {
		index, generation = a.proxies.selectEndpoint(options, endpoints)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if runtime != nil {
				a.releaseManagedProxy(runtime)
			}
			cleanup()
		})
	}
	if index < 0 {
		release()
		a.proxies.mu.Lock()
		blocked := len(a.proxies.blocked) > 0 && len(a.proxies.retryAt) == 0
		a.proxies.mu.Unlock()
		if blocked {
			return parent, func() {}, routing.ProxyFailure("proxy_upstream_blocked", "代理连接可达，但出口暂被上游安全网关拦截")
		}
		return parent, func() {}, routing.ProxyFailure("proxy_pool_exhausted", "无可用代理节点，请检查代理配置")
	}
	ctx = context.WithValue(ctx, routing.ProxySelectionKey{}, routing.ProxySelection{generation, endpoints[index].Identity()})
	return routing.PinnedProxyContext(ctx, endpoints[index].URL), release, nil
}

func (manager *outboundProxyManager) selectEndpoint(options shared.M, endpoints []routing.ProxyEndpoint) (int, uint64) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.configureLocked(options, endpoints)
	return manager.selectAvailableEndpointLocked(time.Now()), manager.generation
}

// Call with mu held. Expired cooldowns are eligible on selection and failover.
func (manager *outboundProxyManager) selectAvailableEndpointLocked(now time.Time) int {
	manager.pruneStateLocked(now)
	start := 0
	for index, endpoint := range manager.endpoints {
		if endpoint.Identity() == manager.selected {
			start = index
			break
		}
	}
	for offset := 0; offset < len(manager.endpoints); offset++ {
		index := (start + offset) % len(manager.endpoints)
		identity := manager.endpoints[index].Identity()
		if manager.retryAt[identity].IsZero() && manager.blocked[identity].IsZero() {
			manager.selected = identity
			return index
		}
	}
	return -1
}

// A route failure changes the shared selection, never an in-flight pinned exit.
func (a *App) prepareProxyFailover(ctx context.Context, failure *fault.Failure) {
	if failure == nil || failure.LocalOnly || ctx.Err() != nil {
		return
	}
	blocked := failure.Code == "upstream_waf_blocked"
	if !blocked && !routing.ProxyExitFailure(failure) {
		return
	}
	if _, pooled := ctx.Value(routing.ProxySelectionKey{}).(routing.ProxySelection); !pooled {
		return
	}
	failure.Retryable = a.markProxyRoute(ctx, blocked, failure.RetryAfter)
	failure.ProxyFailover = failure.Retryable
}

// Claim selection advances its own cursor without moving inference's sticky exit.
func (manager *outboundProxyManager) selectRotatingEndpoint(options shared.M, endpoints []routing.ProxyEndpoint) (int, uint64) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.configureLocked(options, endpoints)
	if len(endpoints) == 0 {
		return -1, manager.generation
	}
	manager.claimCursor %= len(endpoints)
	for offset := 0; offset < len(endpoints); offset++ {
		index := (manager.claimCursor + offset) % len(endpoints)
		identity := endpoints[index].Identity()
		if manager.retryAt[identity].IsZero() && manager.blocked[identity].IsZero() {
			manager.claimCursor = (index + 1) % len(endpoints)
			return index, manager.generation
		}
	}
	return -1, manager.generation
}

// Only current pooled routes may retire nodes; canceled and override routes cannot.
func (a *App) markProxyRoute(ctx context.Context, blocked bool, retryAfter time.Duration) bool {
	selection, ok := ctx.Value(routing.ProxySelectionKey{}).(routing.ProxySelection)
	if !ok || ctx.Err() != nil {
		return false
	}
	a.proxies.mu.Lock()
	a.proxies.initStateLocked()
	if a.proxies.closing || ctx.Err() != nil || selection.Generation != a.proxies.generation {
		a.proxies.mu.Unlock()
		return false
	}
	now := time.Now()
	reason := "代理连接失败"
	if blocked {
		reason = "上游安全网关拦截"
		// Shared admission owns the 1..64 minute backoff. Applying another
		// exponential here would compound simultaneous refusals a second time.
		a.proxies.blocked[selection.Identity] = shared.MaxTime(a.proxies.blocked[selection.Identity], now.Add(max(time.Minute, retryAfter)))
	} else {
		a.proxies.retryAt[selection.Identity] = routing.ProxyCooldownUntil(a.proxies.retryAt[selection.Identity], now, 30*time.Second, 5*time.Minute, retryAfter)
	}
	a.persistProxyStateLocked()
	previous := a.proxies.selected
	index := a.proxies.selectAvailableEndpointLocked(now)
	var previousName, nextName string
	if index >= 0 && previous != a.proxies.selected {
		nextName = a.proxies.endpoints[index].Name
		for _, endpoint := range a.proxies.endpoints {
			if endpoint.Identity() == previous {
				previousName = endpoint.Name
				break
			}
		}
	}
	a.proxies.mu.Unlock()
	if nextName != "" {
		a.log.Printf("代理节点切换（%s）: %q -> %q", reason, previousName, nextName)
	}
	return index >= 0
}

// Persist cooldowns across restarts. Hold proxies.mu; App.Close joins these serialized writes.
func (a *App) persistProxyStateLocked() {
	if a.proxies.persistQueued {
		return
	}
	a.proxies.persistQueued = true
	a.background(func() {
		a.proxies.mu.Lock()
		defer a.proxies.mu.Unlock()
		a.proxies.persistQueued = false
		if err := a.writeProxyStateLocked(); err != nil {
			a.log.Printf("proxy state persistence: %v", err)
		}
	})
}

func (a *App) writeProxyStateLocked() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM proxy_state"); err != nil {
		return err
	}
	now := time.Now()
	identities := map[string]bool{}
	for identity := range a.proxies.blocked {
		identities[identity] = true
	}
	for identity := range a.proxies.retryAt {
		identities[identity] = true
	}
	for identity := range identities {
		var blocked, retry int64
		if until := a.proxies.blocked[identity]; until.After(now) {
			blocked = until.UnixMilli()
		}
		if until := a.proxies.retryAt[identity]; until.After(now) {
			retry = until.UnixMilli()
		}
		if blocked == 0 && retry == 0 {
			continue
		}
		if _, err = tx.Exec("INSERT INTO proxy_state(identity,blocked_until,retry_at) VALUES(?,?,?)", identity, blocked, retry); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) restoreProxyState() {
	now := time.Now()
	a.proxies.mu.Lock()
	defer a.proxies.mu.Unlock()
	a.proxies.initStateLocked()
	for _, v := range a.rows("SELECT identity,blocked_until,retry_at FROM proxy_state") {
		row := shared.Obj(v)
		identity := shared.Str(row["identity"])
		if identity == "" {
			continue
		}
		if blocked := time.UnixMilli(shared.Integer(row["blocked_until"])); blocked.After(now) {
			a.proxies.blocked[identity] = blocked
		}
		if retry := time.UnixMilli(shared.Integer(row["retry_at"])); retry.After(now) {
			a.proxies.retryAt[identity] = retry
		}
	}
}

func (a *App) forgetProxyClients(addresses []routing.ProxyEndpoint) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if addresses == nil {
		for key, client := range a.clients {
			client.CloseIdleConnections()
			delete(a.clients, key)
		}
		return
	}
	for _, endpoint := range addresses {
		if client := a.clients[endpoint.URL]; client != nil {
			client.CloseIdleConnections()
			delete(a.clients, endpoint.URL)
		}
	}
}

func (a *App) resetOutboundProxies() {
	a.proxies.mu.Lock()
	if a.proxies.probeCancel != nil {
		a.proxies.probeCancel()
	}
	old := a.proxies.current
	a.proxies.current = nil
	a.proxies.key, a.proxies.selected = "", ""
	a.proxies.generation++
	a.proxies.endpoints = nil
	a.proxies.claimCursor = 0
	a.proxies.initStateLocked()
	stop := old != nil && old.references == 0
	if old != nil {
		old.retired = true
		if stop {
			delete(a.proxies.runtimes, old)
		}
	}
	a.proxies.mu.Unlock()
	if stop {
		old.stop()
	}
	a.forgetProxyClients(nil)
	shared.Wake(a.proxyHealthReschedule)
}

func (a *App) closeOutboundProxies() {
	a.proxies.mu.Lock()
	a.proxies.closing = true
	all := a.proxies.runtimes
	a.proxies.runtimes, a.proxies.current = nil, nil
	a.proxies.mu.Unlock()
	for runtime := range all {
		runtime.stop()
	}
}
