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
}

type upstreamRequestScope struct {
	account, exit, device, verification string
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
	scope := upstreamRequestScope{exit: "exit|" + origin + "|" + shared.Hash(routing.SelectedProxy(ctx))}
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
}

// Nested SDK/config calls inherit admission from the enclosing inference call.
// A newer refusal invalidates that admission; an older success cannot clear it.
func (a *App) beginUpstreamRequest(ctx context.Context, scope upstreamRequestScope) (context.Context, func(*fault.Failure), *fault.Failure) {
	if ctx.Err() != nil {
		return ctx, func(*fault.Failure) {}, &fault.Failure{Kind: "canceled", Code: "canceled", Message: "请求已取消"}
	}
	baseDelay := min(15*time.Minute, max(time.Second, shared.Ms(shared.Fallback(a.retryParams()["rate_limit_retry_interval_ms"], 1000))))
	parent, _ := ctx.Value(upstreamAdmissionKey{}).(upstreamAdmission)
	admitted := upstreamAdmission{app: a, generations: map[string]uint64{}}
	if parent.app == a {
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
			failure := entry.failure
			failure.Committed, failure.ProxyFailover, failure.Attempts = false, false, 0
			failure.RetryAfter = max(time.Second, entry.until.Sub(now))
			failure.CooldownRecorded = true
			a.mu.Unlock()
			return ctx, func(*fault.Failure) {}, &failure
		}
	}
	for _, key := range keys {
		entry := a.upstreamCooldowns[key]
		generation := uint64(0)
		if entry != nil {
			generation = entry.generation
			if inherited, exists := admitted.generations[key]; !exists || inherited != generation {
				entry.probing = true
				probes[key] = generation
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
		if failure != nil && !failure.CooldownRecorded {
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
				if a.upstreamCooldowns == nil {
					a.upstreamCooldowns = map[string]*upstreamCooldown{}
				}
				entry := a.upstreamCooldowns[key]
				if entry == nil {
					entry = &upstreamCooldown{}
					a.upstreamCooldowns[key] = entry
				}
				entry.failures = min(6, entry.failures+1)
				delay = min(15*time.Minute, delay*time.Duration(1<<uint(entry.failures-1)))
				// Retry-After is a minimum, including when it exceeds our backoff cap.
				delay = max(delay, failure.RetryAfter)
				entry.until = shared.MaxTime(entry.until, time.Now().Add(delay))
				a.cooldownGeneration++
				entry.generation, entry.probing = a.cooldownGeneration, false
				failure.RetryAfter = max(time.Second, time.Until(entry.until))
				entry.failure = *failure
			}
			failure.CooldownRecorded = true
		}
		for key, generation := range probes {
			entry := a.upstreamCooldowns[key]
			if entry == nil || entry.generation != generation {
				continue
			}
			if failure == nil {
				delete(a.upstreamCooldowns, key)
			} else {
				entry.probing = false
				entry.until = shared.MaxTime(entry.until, time.Now().Add(baseDelay))
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
