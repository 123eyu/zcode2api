package server

import (
	"context"
	"net/http"
	"time"
	captchasdk "zcode2api/internal/server/captcha"
	"zcode2api/internal/server/fault"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

// Background SDK work only takes an idle slot after foreground waiters. The
// notification is a broadcast so canceling one waiter cannot strand others.
func (a *App) acquireCaptchaSlot(ctx context.Context, background bool) bool {
	slot := a.captchaSlot()
	a.captcha.mu.Lock()
	if !background {
		a.captcha.waiters++
		defer func() {
			a.captcha.waiters--
			a.notifyCaptchaSlotLocked()
			a.captcha.mu.Unlock()
		}()
	} else {
		defer a.captcha.mu.Unlock()
	}
	for {
		if ctx.Err() != nil {
			return false
		}
		if !background || a.captcha.waiters == 0 {
			select {
			case slot <- struct{}{}:
				return true
			default:
			}
		}
		changed := a.captcha.available
		a.captcha.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		a.captcha.mu.Lock()
	}
}

func (a *App) notifyCaptchaSlotLocked() {
	close(a.captcha.available)
	a.captcha.available = make(chan struct{})
}

func (a *App) releaseCaptchaSlot() {
	a.captcha.mu.Lock()
	<-a.captcha.slot
	a.notifyCaptchaSlotLocked()
	a.captcha.mu.Unlock()
}

// Retain the current runtime so subscription refreshes cannot stop a proxy still in use.
func (a *App) retainCaptchaProxy(proxyURL string) (func(), bool) {
	a.proxies.mu.Lock()
	defer a.proxies.mu.Unlock()
	if a.proxies.closing {
		return func() {}, false
	}
	for runtime := range a.proxies.runtimes {
		for _, endpoint := range runtime.endpoints {
			if endpoint.URL == proxyURL {
				if !runtime.alive() {
					return func() {}, false
				}
				runtime.references++
				return func() { a.releaseManagedProxy(runtime) }, true
			}
		}
	}
	return func() {}, true
}

type captchaTicketScope struct {
	sceneKey, accountID, accountVersion, purpose string
	accountDevice                                string
	generation                                   uint64
	guard                                        upstreamRequestScope
	route                                        routing.ProxySelection
	pooled                                       bool
}

func (s captchaTicketScope) key() string {
	return shared.Hash(shared.JSON([]any{s.sceneKey, s.accountID, s.accountVersion, s.purpose, s.generation, s.pooled, s.route.Generation, s.route.Identity}))
}

func (s captchaTicketScope) matchesCooldown(key string) bool {
	return key != "" && (key == s.guard.account || key == s.guard.exit || key == s.guard.device || key == s.guard.verification || key == s.accountDevice)
}

type captchaPrefetchJob struct{ cancel context.CancelFunc }

type captchaTicketPool struct {
	scope                  captchaTicketScope
	ticket                 *captchaTicket
	runtimeState           shared.M
	job                    *captchaPrefetchJob
	lastUsed, lastAccepted time.Time
}

type captchaVerification struct {
	Headers  http.Header
	pool     *captchaTicketPool
	proxyURL string
	accepted bool
}

func (a *App) captchaAccountVersion(account shared.M) string {
	token := shared.Str(a.tokens(shared.Str(account["id"]))["zcode_jwt"])
	return captchasdk.CaptchaCredentialVersion(account, token)
}

func validateCaptchaRequestToken(verification captchaVerification, account shared.M, token string) *fault.Failure {
	if verification.pool != nil && verification.pool.scope.accountVersion != captchasdk.CaptchaCredentialVersion(account, token) {
		return captchasdk.CaptchaScopeChanged()
	}
	return nil
}

func (a *App) captchaScope(ctx context.Context, account shared.M, purpose string, scene shared.M) (captchaTicketScope, *fault.Failure) {
	if shared.Str(account["id"]) == "" || (purpose != captchasdk.CaptchaPurposeInference && purpose != captchasdk.CaptchaPurposeClaim && purpose != captchasdk.CaptchaPurposeScheduledClaim) {
		return captchaTicketScope{}, captchasdk.CaptchaScopeChanged()
	}
	scope := captchaTicketScope{sceneKey: a.captchaSceneKey(ctx), accountID: shared.Str(account["id"]), accountVersion: a.captchaAccountVersion(account), purpose: purpose, guard: a.captchaRequestScope(ctx, account, scene), accountDevice: a.upstreamScope(ctx, account, a.origin, true).device}
	if scope.accountVersion == "" {
		return scope, &fault.Failure{Kind: "auth", Code: "authentication_error", Message: "验证账号凭据不可用"}
	}
	scope.route, scope.pooled = ctx.Value(routing.ProxySelectionKey{}).(routing.ProxySelection)
	a.captcha.mu.Lock()
	current := a.captcha.scenes[scope.sceneKey]
	scope.generation = current.Generation
	valid := current.Generation != 0 && current.Loading == nil && shared.JSON(current.Value) == shared.JSON(scene)
	a.captcha.mu.Unlock()
	if !valid || !a.captchaScopeCurrent(ctx, scope) {
		return captchaTicketScope{}, captchasdk.CaptchaScopeChanged()
	}
	return scope, nil
}

func (a *App) captchaScopeCurrent(ctx context.Context, scope captchaTicketScope) bool {
	account := a.account(scope.accountID)
	route, pooled := ctx.Value(routing.ProxySelectionKey{}).(routing.ProxySelection)
	if account == nil || scope.accountVersion == "" || account["status"] == "needs_relogin" || account["status"] == "disabled" || pooled != scope.pooled || route != scope.route || scope.sceneKey != a.captchaSceneKey(ctx) || scope.accountVersion != a.captchaAccountVersion(account) {
		return false
	}
	a.captcha.mu.Lock()
	scene := a.captcha.scenes[scope.sceneKey]
	valid := scene.Generation == scope.generation && scene.Loading == nil && shared.Truth(scene.Value["enabled"]) && time.Now().Before(scene.Expires)
	a.captcha.mu.Unlock()
	return valid
}

func (a *App) captchaPool(scope captchaTicketScope) *captchaTicketPool {
	a.captcha.mu.Lock()
	defer a.captcha.mu.Unlock()
	if a.captcha.pools == nil {
		a.captcha.pools = map[string]*captchaTicketPool{}
	}
	key := scope.key()
	if pool := a.captcha.pools[key]; pool != nil {
		pool.lastUsed = time.Now()
		return pool
	}
	for candidate, pool := range a.captcha.pools {
		if pool.scope.accountID == scope.accountID && pool.scope.accountVersion != scope.accountVersion {
			a.dropCaptchaPoolLocked(candidate)
		}
	}
	if len(a.captcha.pools) >= captchasdk.MaxCaptchaPools {
		oldest := ""
		for candidate, pool := range a.captcha.pools {
			if oldest == "" || pool.lastUsed.Before(a.captcha.pools[oldest].lastUsed) {
				oldest = candidate
			}
		}
		a.dropCaptchaPoolLocked(oldest)
	}
	pool := &captchaTicketPool{scope: scope, lastUsed: time.Now()}
	a.captcha.pools[key] = pool
	return pool
}

func (a *App) dropCaptchaPoolLocked(key string) {
	if pool := a.captcha.pools[key]; pool != nil && pool.job != nil {
		pool.job.cancel()
	}
	delete(a.captcha.pools, key)
}

func (a *App) dropCaptchaScenePoolsLocked(sceneKey string) {
	for key, pool := range a.captcha.pools {
		if pool.scope.sceneKey == sceneKey {
			a.dropCaptchaPoolLocked(key)
		}
	}
}

func (a *App) takeCaptchaTicket(pool *captchaTicketPool) (captchaTicket, bool) {
	if captchasdk.CaptchaTicketTTL(shared.Obj(a.configValue("http", "captcha"))) <= 0 {
		return captchaTicket{}, false
	}
	a.captcha.mu.Lock()
	defer a.captcha.mu.Unlock()
	scene := a.captcha.scenes[pool.scope.sceneKey]
	if a.captcha.pools[pool.scope.key()] != pool || pool.ticket == nil {
		return captchaTicket{}, false
	}
	ticket := *pool.ticket
	pool.ticket = nil
	if ticket.scope != pool.scope || scene.Generation != pool.scope.generation || !time.Now().Before(scene.Expires) || !time.Now().Add(captchasdk.CaptchaTicketSafetyMargin).Before(ticket.expiresAt) {
		return captchaTicket{}, false
	}
	return ticket, true
}

// Count accepted business requests, not ticket generation. A lone request never
// starts a speculative SDK execution; demand expires after a quiet interval.
func (a *App) acceptCaptchaVerification(ctx context.Context, verification *captchaVerification) {
	pool := verification.pool
	if pool == nil || verification.accepted {
		return
	}
	verification.accepted = true
	if !a.captchaScopeCurrent(ctx, pool.scope) {
		return
	}
	a.captcha.mu.Lock()
	if a.captcha.pools[pool.scope.key()] != pool {
		a.captcha.mu.Unlock()
		return
	}
	now := time.Now()
	recent := !pool.lastAccepted.IsZero() && now.Sub(pool.lastAccepted) <= captchasdk.CaptchaPrefetchDemandWindow
	pool.lastAccepted, pool.lastUsed = now, now
	a.captcha.mu.Unlock()
	if recent {
		a.scheduleTicketRenewal(pool, verification.proxyURL)
	}
}

func (a *App) rejectCaptchaVerification(verification captchaVerification) {
	if verification.pool == nil {
		return
	}
	a.captcha.mu.Lock()
	defer a.captcha.mu.Unlock()
	key := verification.pool.scope.key()
	if a.captcha.pools[key] == verification.pool {
		a.dropCaptchaPoolLocked(key)
	}
}

// Called after a shared cooldown is published. Removing the pool also prevents
// an older in-flight success from reviving demand or storing a canceled mint.
func (a *App) pauseCaptchaPrefetch(cooldownKey string) {
	a.captcha.mu.Lock()
	defer a.captcha.mu.Unlock()
	for key, pool := range a.captcha.pools {
		if pool.scope.matchesCooldown(cooldownKey) {
			a.dropCaptchaPoolLocked(key)
		}
	}
}

func (a *App) captchaPrefetchBlocked(scope captchaTicketScope) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.upstreamCooldowns {
		if scope.matchesCooldown(key) {
			return true
		}
	}
	return false
}

func (a *App) scheduleTicketRenewal(pool *captchaTicketPool, proxyURL string) {
	options := shared.Obj(a.configValue("http", "captcha"))
	// Claims mint on demand: prefetch would bypass worker limits and race another claim.
	if pool.scope.purpose != captchasdk.CaptchaPurposeInference {
		return
	}
	if captchasdk.CaptchaTicketTTL(options) <= 0 || a.ctx.Err() != nil || a.captchaPrefetchBlocked(pool.scope) {
		return
	}
	a.captcha.mu.Lock()
	scene := a.captcha.scenes[pool.scope.sceneKey]
	if a.captcha.pools[pool.scope.key()] != pool || pool.job != nil || time.Since(pool.lastAccepted) > captchasdk.CaptchaPrefetchDemandWindow || scene.Generation != pool.scope.generation || !shared.Truth(scene.Value["enabled"]) {
		a.captcha.mu.Unlock()
		return
	}
	if pool.ticket != nil && time.Now().Add(captchasdk.CaptchaTicketSafetyMargin).Before(pool.ticket.expiresAt) {
		a.captcha.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(routing.PinnedProxyContext(a.ctx, proxyURL), shared.Ms(shared.Fallback(options["timeout_ms"], 30000))+3*time.Second)
	if pool.scope.pooled {
		ctx = context.WithValue(ctx, routing.ProxySelectionKey{}, pool.scope.route)
	}
	job := &captchaPrefetchJob{cancel: cancel}
	pool.job = job
	a.captcha.mu.Unlock()
	release, retained := a.retainCaptchaProxy(proxyURL)
	a.background(func() {
		defer cancel()
		defer release()
		ticket, ready := captchaTicket{}, false
		if retained {
			ticket, ready = a.renewCaptchaTicket(ctx, pool, options)
		}
		a.captcha.mu.Lock()
		defer a.captcha.mu.Unlock()
		if a.captcha.pools[pool.scope.key()] != pool || pool.job != job {
			return
		}
		pool.job = nil
		scene := a.captcha.scenes[pool.scope.sceneKey]
		if !ready || ctx.Err() != nil || scene.Generation != pool.scope.generation {
			pool.lastAccepted, pool.ticket = time.Time{}, nil
			return
		}
		ticket.expiresAt = ticket.expiresAt.Add(captchasdk.CaptchaTicketTTL(options) - captchasdk.MaxTicketTTL)
		if _, used := a.captcha.used[shared.Hash(ticket.param)]; !used && time.Now().Add(captchasdk.CaptchaTicketSafetyMargin).Before(ticket.expiresAt) {
			pool.ticket = &ticket
		}
	})
}

func (a *App) renewCaptchaTicket(ctx context.Context, pool *captchaTicketPool, options shared.M) (captchaTicket, bool) {
	if !a.acquireCaptchaSlot(ctx, true) {
		return captchaTicket{}, false
	}
	defer a.releaseCaptchaSlot()
	if ctx.Err() != nil || !a.captchaScopeCurrent(ctx, pool.scope) || a.captchaPrefetchBlocked(pool.scope) || shared.JSON(options) != shared.JSON(shared.Obj(a.configValue("http", "captcha"))) {
		return captchaTicket{}, false
	}
	a.captcha.mu.Lock()
	scene := a.captcha.scenes[pool.scope.sceneKey]
	valid := a.captcha.pools[pool.scope.key()] == pool && scene.Generation == pool.scope.generation && time.Now().Before(scene.Expires) && time.Since(pool.lastAccepted) <= captchasdk.CaptchaPrefetchDemandWindow
	a.captcha.mu.Unlock()
	if !valid {
		return captchaTicket{}, false
	}
	account := a.account(pool.scope.accountID)
	ticket, failure := a.mintCaptchaTicket(ctx, account, options, scene.Value, pool.scope)
	if failure != nil {
		if ctx.Err() == nil {
			a.log.Printf("验证码预取失败: %s", fault.ConciseFailureMessage(failure))
		}
		return captchaTicket{}, false
	}
	return ticket, ctx.Err() == nil && a.captchaScopeCurrent(ctx, pool.scope) && !a.captchaPrefetchBlocked(pool.scope)
}
