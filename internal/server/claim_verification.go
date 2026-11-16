package server

import (
	"context"
	"fmt"
	"strings"
	"time"
	captchasdk "zcode2api/internal/server/captcha"
	"zcode2api/internal/server/fault"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

type claimCaptchaDiagnosticKey struct{}

func (a *App) claimVerification(ctx context.Context, id, plan string) (shared.M, error) {
	if strings.TrimSpace(plan) == "" {
		return nil, fault.Invalid("缺少 plan_id")
	}
	account := a.account(id)
	if account == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	ctx, release, err := a.pinClaimOutboundProxy(ctx, account)
	if err != nil {
		return nil, err
	}
	defer release()
	// Check the ordinary account/device/exit guard before loading scene data.
	// A browser dialog must wait just like an automatic claim does.
	cooldownResult := func(guard upstreamRequestScope) shared.M {
		var retryAt time.Time
		a.mu.Lock()
		for _, key := range guard.keys() {
			if cooldown := a.upstreamCooldowns[key]; cooldown != nil {
				retryAt = shared.MaxTime(retryAt, cooldown.until)
				if cooldown.probing {
					retryAt = shared.MaxTime(retryAt, time.Now().Add(time.Second))
				}
			}
		}
		a.mu.Unlock()
		if time.Now().Before(retryAt) {
			return shared.M{"enabled": true, "verification_retry_at": retryAt.UnixMilli()}
		}
		return nil
	}
	if waiting := cooldownResult(a.upstreamScope(ctx, account, a.origin, true)); waiting != nil {
		return waiting, nil
	}
	a.prepareCaptchaChallenge(ctx)
	scene, failure := a.captchaScene(ctx, account)
	if failure != nil {
		return nil, failure
	}
	if !shared.Truth(scene["enabled"]) {
		return shared.M{"enabled": false}, nil
	}
	scope, failure := a.captchaScope(ctx, account, captchasdk.CaptchaPurposeClaim, scene)
	if failure != nil {
		return nil, failure
	}
	// Scene-specific SDK cooldowns also apply before issuing a short-lived session.
	if waiting := cooldownResult(scope.guard); waiting != nil {
		return waiting, nil
	}
	token, failure := a.newClaimCaptchaSession(ctx, scope, plan, shared.Str(scene["region"]))
	if failure != nil {
		return nil, failure
	}
	return shared.M{"enabled": true, "scene_id": scene["sceneId"], "region": scene["region"], "prefix": scene["prefix"], "verification_session": token, "expires_at": shared.NowMS() + claimCaptchaSessionTTL.Milliseconds()}, nil
}

func (a *App) reserveCaptchaTicket(param string) *fault.Failure {
	a.captcha.mu.Lock()
	defer a.captcha.mu.Unlock()
	if a.captcha.used == nil {
		a.captcha.used = map[string]time.Time{}
	}
	for key, expires := range a.captcha.used {
		if time.Now().After(expires) {
			delete(a.captcha.used, key)
		}
	}
	id := shared.Hash(param)
	if _, exists := a.captcha.used[id]; exists {
		return &fault.Failure{Kind: "risk", Code: "captcha_duplicate_result", Message: "验证票据已使用，请重新完成安全验证"}
	}
	if len(a.captcha.used) >= 2048 {
		return &fault.Failure{Kind: "server", Code: "captcha_capacity_exceeded", Message: "安全验证票据保护缓存已满，请稍后重试"}
	}
	a.captcha.used[id] = time.Now().Add(20 * time.Minute)
	return nil
}

const claimCaptchaSessionTTL = 3 * time.Minute

const maxClaimCaptchaSessions = 64

type claimCaptchaSession struct {
	scope                    captchaTicketScope
	planID, region, proxyURL string
	expires                  time.Time
	done                     chan struct{}
	release                  func()
}

func claimCaptchaSessionInvalid() *fault.Failure {
	return &fault.Failure{Kind: "risk", Code: "captcha_session_invalid", Message: "领取验证会话已失效或与当前账号、计划不匹配，请重新验证"}
}

func (a *App) newClaimCaptchaSession(ctx context.Context, scope captchaTicketScope, plan, region string) (string, *fault.Failure) {
	release, retained := a.retainCaptchaProxy(routing.SelectedProxy(ctx))
	if !retained {
		return "", claimCaptchaSessionInvalid()
	}
	session := &claimCaptchaSession{scope: scope, planID: plan, region: region, proxyURL: routing.SelectedProxy(ctx), expires: time.Now().Add(claimCaptchaSessionTTL), done: make(chan struct{}), release: release}
	token := shared.Random(32)
	key := shared.Hash(token)
	a.captcha.mu.Lock()
	current := a.captcha.scenes[scope.sceneKey]
	if a.ctx.Err() != nil || current.Generation != scope.generation || !time.Now().Before(current.Expires) || len(a.captcha.browser) >= maxClaimCaptchaSessions {
		a.captcha.mu.Unlock()
		release()
		return "", &fault.Failure{Kind: "server", Code: "captcha_capacity_exceeded", Message: "领取验证会话暂不可用，请稍后重试"}
	}
	if a.captcha.browser == nil {
		a.captcha.browser = map[string]*claimCaptchaSession{}
	}
	a.captcha.browser[key] = session
	a.captcha.mu.Unlock()
	a.background(func() {
		timer := time.NewTimer(time.Until(session.expires))
		defer timer.Stop()
		select {
		case <-session.done:
			return // The consumer owns the retained route after taking the session.
		case <-a.ctx.Done():
		case <-timer.C:
		}
		a.cancelClaimVerification(token)
	})
	return token, nil
}

func (a *App) cancelClaimVerification(token string) {
	if !routing.HexIDPattern.MatchString(token) {
		return
	}
	key := shared.Hash(token)
	a.captcha.mu.Lock()
	session := a.captcha.browser[key]
	if session != nil {
		delete(a.captcha.browser, key)
		close(session.done)
	}
	a.captcha.mu.Unlock()
	if session != nil {
		session.release()
	}
}

// The nonce is local request binding, not a claim about the browser's public IP.
// The upstream verifier still validates the unmodified browser-issued ticket.
func (a *App) claimVerificationContext(parent context.Context, account shared.M, plan, token string, verification shared.M) (context.Context, func(), error) {
	if shared.Str(verification["captcha_verify_param"]) == "" {
		return a.pinClaimOutboundProxy(parent, account)
	}
	key := shared.Hash(shared.Str(verification["verification_session"]))
	a.captcha.mu.Lock()
	session := a.captcha.browser[key]
	a.captcha.mu.Unlock()
	if session == nil || session.scope.accountID != shared.Str(account["id"]) || session.planID != plan || session.region != shared.Str(verification["captcha_region"]) || session.scope.purpose != captchasdk.CaptchaPurposeClaim || session.scope.accountVersion != captchasdk.CaptchaCredentialVersion(account, token) {
		return parent, func() {}, claimCaptchaSessionInvalid()
	}
	ctx := routing.PinnedProxyContext(parent, session.proxyURL)
	if session.scope.pooled {
		ctx = context.WithValue(ctx, routing.ProxySelectionKey{}, session.scope.route)
	}
	if !a.captchaScopeCurrent(ctx, session.scope) {
		a.cancelClaimVerification(shared.Str(verification["verification_session"]))
		return parent, func() {}, claimCaptchaSessionInvalid()
	}
	a.captcha.mu.Lock()
	current := a.captcha.scenes[session.scope.sceneKey]
	if a.captcha.browser[key] != session || !time.Now().Before(session.expires) || current.Generation != session.scope.generation || !time.Now().Before(current.Expires) || !shared.Truth(current.Value["enabled"]) {
		a.captcha.mu.Unlock()
		a.cancelClaimVerification(shared.Str(verification["verification_session"]))
		return parent, func() {}, claimCaptchaSessionInvalid()
	}
	delete(a.captcha.browser, key)
	close(session.done)
	a.captcha.mu.Unlock()
	return ctx, session.release, nil
}

const manualClaimConcurrency = 5

type manualClaimWorkerKey struct{}

// Lease a verification worker for the entire manual claim, so multiple tabs
// and batch requests share the same bound. Account locks still prevent two
// simultaneous submissions for the same account.
func (a *App) acquireManualClaimWorker(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	if a.manualClaimSlots == nil {
		a.manualClaimSlots = make(chan int, manualClaimConcurrency)
		for worker := range manualClaimConcurrency {
			a.manualClaimSlots <- worker
		}
	}
	slots := a.manualClaimSlots
	a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	select {
	case worker := <-slots:
		return context.WithValue(ctx, manualClaimWorkerKey{}, worker), func() { slots <- worker }, nil
	case <-ctx.Done():
		return ctx, nil, ctx.Err()
	case <-a.ctx.Done():
		return ctx, nil, a.ctx.Err()
	}
}
