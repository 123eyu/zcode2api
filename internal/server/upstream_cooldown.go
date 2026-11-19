package server

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"zcode2api/internal/server/fault"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

// Guard state is shared by inference, management requests and SDK executions.
// It is separate from login state and proxy connection health.
type upstreamCooldown struct {
	until      time.Time
	generation uint64
	probing    bool
	failures   int
	failure    fault.Failure
	inference  bool // Only an inference probe can clear an inference account refusal.
	recovery   *poolRecoveryRound
}

type upstreamRequestScope struct {
	account, exit, device, verification string
	inference                           bool
}

const riskCooldownLimit = 64 * time.Minute

func cooldownDelay(base time.Duration, failures int) time.Duration {
	return min(riskCooldownLimit, base*time.Duration(1<<uint(max(0, min(7, failures)-1))))
}

func (entry *upstreamCooldown) refusal() *fault.Failure {
	failure := entry.failure
	failure.Detail = fault.FailureDetail(&failure)
	failure.Detail["local_cooldown"] = true
	failure.Detail["cooldown_until"] = entry.until.UnixMilli()
	failure.Detail["cooldown_step"] = entry.failures
	failure.Committed, failure.ProxyFailover, failure.Attempts = false, false, 0
	failure.RetryAfter = max(time.Second, time.Until(entry.until))
	failure.CooldownRecorded, failure.LocalOnly = true, true
	return &failure
}

// Call with a.mu held. Only the admitted generation can advance the backoff;
// concurrent replies from an older batch cannot turn one refusal into 64 minutes.
func (a *App) recordUpstreamCooldownLocked(key string, generation uint64, failure *fault.Failure, base time.Duration, inference bool) {
	if a.cooldownEpochs[key] != generation {
		if entry := a.upstreamCooldowns[key]; entry != nil {
			// A real later Retry-After can extend a deadline, but never the step.
			if until := time.Now().Add(failure.RetryAfter); until.After(entry.until) {
				entry.until = until
				a.persistUpstreamCooldownLocked(key)
			}
			failure.RetryAfter = max(time.Second, time.Until(entry.until))
		}
		return
	}
	if a.upstreamCooldowns == nil {
		a.upstreamCooldowns = map[string]*upstreamCooldown{}
	}
	entry := a.upstreamCooldowns[key]
	if entry == nil {
		entry = &upstreamCooldown{}
		a.upstreamCooldowns[key] = entry
	}
	entry.failures = min(7, entry.failures+1)
	entry.until = shared.MaxTime(entry.until, time.Now().Add(max(cooldownDelay(base, entry.failures), failure.RetryAfter)))
	entry.generation, entry.probing = a.advanceCooldownEpochLocked(key), false
	entry.recovery = nil
	entry.inference = entry.inference || inference
	failure.RetryAfter = max(time.Second, time.Until(entry.until))
	// Keep only the classification in shared/persistent state. Request-specific
	// trace IDs and response bodies belong to that request's usage log.
	entry.failure = fault.Failure{Kind: failure.Kind, Code: failure.Code, Message: fault.ConciseFailureMessage(failure), VerifyCode: failure.VerifyCode, Retryable: failure.Retryable}
	a.persistUpstreamCooldownLocked(key)
}

func (a *App) advanceCooldownEpochLocked(key string) uint64 {
	if a.cooldownEpochs == nil {
		a.cooldownEpochs = map[string]uint64{}
	}
	a.cooldownGeneration++
	a.cooldownEpochs[key] = a.cooldownGeneration
	return a.cooldownGeneration
}

func (a *App) clearUpstreamCooldownLocked(key string) {
	delete(a.upstreamCooldowns, key)
	a.advanceCooldownEpochLocked(key)
	a.persistUpstreamCooldownLocked(key)
}

func (a *App) persistUpstreamCooldownLocked(key string) {
	if a.db == nil {
		return
	}
	var err error
	if entry := a.upstreamCooldowns[key]; entry != nil {
		err = a.exec("INSERT OR REPLACE INTO upstream_cooldowns(scope,until_ms,failures,kind,code,verify_code,inference) VALUES(?,?,?,?,?,?,?)", key, entry.until.UnixMilli(), entry.failures, entry.failure.Kind, entry.failure.Code, entry.failure.VerifyCode, shared.Integer(entry.inference))
	} else {
		err = a.exec("DELETE FROM upstream_cooldowns WHERE scope=?", key)
	}
	if err != nil && a.log != nil {
		a.log.Printf("保存上游冷却状态失败: %v", err)
	}
}

func (a *App) restoreUpstreamCooldowns() {
	a.upstreamCooldowns = map[string]*upstreamCooldown{}
	for _, value := range a.rows("SELECT * FROM upstream_cooldowns") {
		row := shared.Obj(value)
		key := shared.Str(row["scope"])
		failure := fault.Failure{Kind: shared.Str(row["kind"]), Code: shared.Str(row["code"]), VerifyCode: shared.Str(row["verify_code"])}
		failure.Message = fault.ConciseFailureMessage(&failure)
		a.upstreamCooldowns[key] = &upstreamCooldown{until: time.UnixMilli(shared.Integer(row["until_ms"])), failures: max(1, min(7, int(shared.Integer(row["failures"])))), failure: failure, inference: shared.Truth(row["inference"]), generation: a.advanceCooldownEpochLocked(key)}
	}
}

func (s upstreamRequestScope) keys() []string {
	keys := []string{}
	for _, key := range []string{s.account, s.exit, s.device, s.verification} {
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func (a *App) upstreamScope(ctx context.Context, account shared.M, target string, authenticated bool) upstreamRequestScope {
	origin := strings.TrimRight(target, "/")
	if parsed, err := url.Parse(target); err == nil && parsed.Host != "" {
		origin = strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	}
	exit := routing.SelectedProxy(ctx)
	if selection, ok := ctx.Value(routing.ProxySelectionKey{}).(routing.ProxySelection); ok {
		// Managed proxy ports change on restart; the node identity does not.
		exit = "pool|" + selection.Identity
	}
	scope := upstreamRequestScope{exit: "exit|" + origin + "|" + shared.Hash(exit)}
	if authenticated && shared.Str(account["id"]) != "" {
		scope.account = "account|" + origin + "|" + shared.Str(account["id"])
		if device := shared.Str(account["device_mid"]); device != "" {
			scope.device = "device|" + origin + "|" + shared.Hash(device)
		}
	}
	return scope
}

func (a *App) captchaRequestScope(ctx context.Context, account, scene shared.M) upstreamRequestScope {
	scope := a.upstreamScope(ctx, account, a.origin, true)
	identity := shared.Str(scene["region"]) + "|" + shared.Str(scene["prefix"])
	// The SDK does not receive account.device_mid. Its device restriction must
	// not be attributed to that unrelated account header or escaped by a route change.
	scope.device = "sdk-device|" + a.origin + "|" + shared.Hash(identity)
	scope.verification = "verification|" + scope.exit + "|" + shared.Hash(identity+"|"+shared.Str(scene["sceneId"]))
	return scope
}

type upstreamAdmissionKey struct{}

type upstreamAdmission struct {
	app         *App
	generations map[string]uint64
	inference   bool
}

// Nested SDK/config calls inherit admission from the enclosing inference call.
// A newer refusal invalidates that admission; an older success cannot clear it.
func (a *App) beginUpstreamRequest(ctx context.Context, scope upstreamRequestScope) (context.Context, func(*fault.Failure), *fault.Failure) {
	if ctx.Err() != nil {
		return ctx, func(*fault.Failure) {}, &fault.Failure{Kind: "canceled", Code: "canceled", Message: "请求已取消"}
	}
	// User-initiated claims surface each upstream result directly.
	if ctx.Value(manualClaimContextKey{}) == true {
		return ctx, func(*fault.Failure) {}, nil
	}
	baseDelay := min(15*time.Minute, max(time.Second, shared.Ms(shared.Fallback(a.retryParams()["rate_limit_retry_interval_ms"], 1000))))
	parent, _ := ctx.Value(upstreamAdmissionKey{}).(upstreamAdmission)
	admitted := upstreamAdmission{app: a, generations: map[string]uint64{}, inference: scope.inference}
	if parent.app == a {
		admitted.inference = admitted.inference || parent.inference
		for key, generation := range parent.generations {
			admitted.generations[key] = generation
		}
	}
	keys := scope.keys()
	probes := map[string]uint64{}
	now := time.Now()
	a.mu.Lock()
	for _, key := range keys {
		entry := a.upstreamCooldowns[key]
		if entry == nil {
			continue
		}
		if generation, inherited := admitted.generations[key]; inherited && generation == entry.generation {
			continue
		}
		if now.Before(entry.until) || entry.probing {
			failure := entry.refusal()
			a.mu.Unlock()
			return ctx, func(*fault.Failure) {}, failure
		}
	}
	for _, key := range keys {
		entry := a.upstreamCooldowns[key]
		generation := a.cooldownEpochs[key]
		if entry != nil {
			generation = entry.generation
			if inherited, exists := admitted.generations[key]; !exists || inherited != generation {
				// Balance refresh must remain possible after the account cooldown;
				// its success alone cannot establish inference recovery.
				if !entry.inference || admitted.inference {
					entry.probing = true
					probes[key] = generation
				}
			}
		}
		admitted.generations[key] = generation
	}
	a.mu.Unlock()
	done := false
	finish := func(failure *fault.Failure) {
		if failure != nil {
			fault.Classify(failure)
		}
		a.mu.Lock()
		pauseKey := ""
		defer func() {
			a.mu.Unlock()
			if pauseKey != "" {
				a.pauseCaptchaPrefetch(pauseKey)
			}
		}()
		if done {
			return
		}
		done = true
		if failure != nil && failure.Kind == "auth" {
			pauseKey = scope.account
		}
		if failure != nil && !failure.CooldownRecorded && !failure.LocalOnly {
			key := ""
			delay := baseDelay
			switch {
			case failure.Code == "upstream_waf_blocked":
				key, delay = scope.exit, time.Minute
			case failure.VerifyCode == "F010" || failure.Code == "F010":
				key, delay = scope.exit, time.Minute
			case failure.VerifyCode == "F011" || failure.Code == "F011":
				key, delay = scope.device, time.Minute
			case failure.Kind == "risk" && (failure.Code == "captcha_risk_blocked" || failure.Code == "F001"):
				key, delay = scope.verification, time.Minute
				if key == "" {
					// A provider F001 may arrive on the inference response itself,
					// without an SDK scene. Do not lose the refusal in that case.
					key = scope.exit
				}
			case failure.Kind == "account_risk":
				// Anonymous refusals provide no account or proxy health evidence.
				key, delay = scope.account, time.Minute
			case failure.Kind == "rate_limit" && failure.Code != "throttle_window_exhausted":
				key = scope.account
				if key == "" || scope.verification != "" {
					key = scope.exit
				}
			}
			if key != "" {
				pauseKey = key
				a.recordUpstreamCooldownLocked(key, admitted.generations[key], failure, delay, admitted.inference && failure.Kind == "account_risk")
			}
			failure.CooldownRecorded = true
		}
		for key, generation := range probes {
			entry := a.upstreamCooldowns[key]
			if entry == nil || entry.generation != generation {
				continue
			}
			if failure == nil {
				a.clearUpstreamCooldownLocked(key)
			} else {
				entry.probing = false
				entry.until = shared.MaxTime(entry.until, time.Now().Add(baseDelay))
				a.persistUpstreamCooldownLocked(key)
			}
		}
		a.notifyAvailableLocked()
	}
	return context.WithValue(ctx, upstreamAdmissionKey{}, admitted), finish, nil
}

func upstreamRequestFailure(ctx context.Context, err error) *fault.Failure {
	var failure *fault.Failure
	if errors.As(err, &failure) {
		return fault.Classify(failure)
	}
	if ctx.Err() != nil {
		return &fault.Failure{Kind: "canceled", Code: "canceled", Message: "请求已取消"}
	}
	return routing.TransportFailure(ctx, err)
}
