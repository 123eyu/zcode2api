package server

import (
	"context"
	"errors"
	"sort"
	"time"
	accountdata "zcode2api/internal/server/account"
	"zcode2api/internal/server/fault"
	"zcode2api/internal/server/shared"
	"zcode2api/internal/server/storage"
)

const claimDiscoveryConcurrency = 3

const claimDiscoveryAccountTimeout = 10 * time.Second

const dailyClaimCandidateRecoveryVersion = 2

func (a *App) discoverClaimAccount(ctx context.Context, account shared.M, dailyOnly bool) shared.M {
	ctx, cancel := context.WithTimeout(ctx, claimDiscoveryAccountTimeout)
	defer cancel()
	result := shared.M{"account_id": account["id"], "label": account["label"], "plans": []any{}}
	var preview shared.M
	err := ctx.Err()
	if err == nil {
		preview, err = a.preview(ctx, account, dailyOnly)
	}
	if err == nil {
		result["plans"], result["checked_at"] = shared.Arr(preview["plans"]), shared.NowMS()
		if preview["warning"] != nil {
			result["warning"] = preview["warning"]
		}
	} else {
		result["error"] = err.Error()
		var failure *fault.Failure
		if errors.As(err, &failure) {
			result["error"] = fault.ConciseFailureMessage(failure)
			result["code"], result["retry_after_ms"] = failure.Code, failure.RetryAfter.Milliseconds()
		} else if errors.Is(err, context.DeadlineExceeded) {
			result["error"] = "查询超时，请刷新重试"
		} else if errors.Is(err, context.Canceled) {
			result["error"] = "查询已中断，请刷新重试"
		}
		if dailyOnly {
			// An upstream failure does not erase an account's known deliveries.
			// Keep the error: cached cards are not a successful fresh query and
			// must not cause the scheduler to proceed after a failed preview.
			if cached, cacheErr := a.pendingDailyClaimPlans(account, nil); cacheErr == nil {
				result["plans"], result["stale"] = cached, len(cached) > 0
			} else {
				result["error"] = shared.Str(result["error"]) + "；已保存卡片读取失败"
			}
		}
	}
	if !dailyOnly {
		if reward, err := a.incentives(ctx, account); err == nil {
			result["incentives"] = reward
		} else {
			result["incentives_error"] = err.Error()
		}
	}
	return result
}

// Marketing touch returns deliveries for this visit, not an inventory of all
// unclaimed cards. Keep an account's actionable deliveries until expiry or a
// confirmed claim, so refreshing and scheduled recovery cannot lose them.
func (a *App) pendingDailyClaimPlans(account shared.M, fresh []any) ([]any, error) {
	id := shared.Str(account["id"])
	now := shared.NowMS()
	cutoff := now - (24 * time.Hour).Milliseconds()
	identity := accountdata.JwtPayload(a.accountTokens(account)["zcode_jwt"])
	scope := shared.Hash(shared.JSON([]any{a.origin, id, account["customer_id"], account["device_mid"], identity["user_id"]}))
	key := "daily_claim_candidates:" + id
	// Serialize read/merge/write against simultaneous page refreshes and the
	// scheduler. No network request is made while holding this lock.
	a.claimCandidatesMu.Lock()
	defer a.claimCandidatesMu.Unlock()
	settings, err := storage.Query(a.db, "SELECT v FROM settings WHERE k=?", key)
	if err != nil {
		return nil, err
	}
	saved := shared.M{}
	if len(settings) > 0 {
		saved = shared.Obj(shared.Parse(shared.Obj(settings[0])["v"], nil))
	}
	candidates := map[string]shared.M{}
	if saved["scope"] == scope {
		for _, value := range shared.Arr(saved["plans"]) {
			entry := shared.Obj(value)
			if shared.Integer(entry["expires_at"]) > now {
				candidates[shared.Str(shared.Obj(entry["plan"])["plan_id"])] = entry
			}
		}
	}
	add := func(plan shared.M, seen int64) {
		end := shared.Integer(accountdata.SecondsMS(plan["ends_at"]))
		if end <= now {
			return // Never retain a delivery with unknown or expired validity.
		}
		candidates[shared.Str(plan["plan_id"])] = shared.M{"plan": plan, "expires_at": min(end, seen+(24*time.Hour).Milliseconds())}
	}
	for _, value := range fresh {
		add(shared.Obj(value), now)
	}
	if (saved["scope"] == nil || saved["scope"] == scope) && shared.Integer(saved["recovery_version"]) < dailyClaimCandidateRecoveryVersion {
		// Upgrade recovery: old versions discarded delivery metadata, but a
		// scheduled verification failure proves this account had that entry.
		// Recover only those IDs; another account's plan alone is insufficient.
		attempts, err := storage.Query(a.db, "SELECT plan_id,MAX(ts) seen_at FROM claim_log WHERE account_id=? AND trigger='scheduled' AND ok=0 AND ts>=? AND code IN ('3012','upstream_risk_blocked','F001','captcha_risk_blocked','captcha_interaction_required','captcha_verification_failed','captcha_runtime_error','captcha_timeout','captcha_expired','claim_request_failed') GROUP BY plan_id", id, cutoff)
		if err != nil {
			return nil, err
		}
		if len(attempts) > 0 {
			rows, err := storage.Query(a.db, "SELECT last_balance FROM accounts WHERE last_balance IS NOT NULL")
			if err != nil {
				return nil, err
			}
			catalog := map[string]shared.M{}
			for _, row := range rows {
				for _, value := range dailyFreePlans(shared.Arr(accountdata.BalancePayload(shared.Parse(shared.Obj(row)["last_balance"], nil))["plans"])) {
					plan := shared.Obj(value)
					// Only public campaign metadata; user_plan_id and account
					// balances must never be copied to another account.
					catalog[shared.Str(plan["plan_id"])] = shared.M{"plan_id": plan["plan_id"], "name": plan["name"], "ends_at": plan["ends_at"], "entitlements": plan["entitlements"]}
				}
			}
			for _, value := range attempts {
				attempt := shared.Obj(value)
				planID := shared.Str(attempt["plan_id"])
				if candidates[planID] == nil && catalog[planID] != nil {
					add(catalog[planID], shared.Integer(attempt["seen_at"]))
				}
			}
		}
	}
	claimed := map[string]bool{}
	rows, err := storage.Query(a.db, "SELECT DISTINCT plan_id FROM claim_log WHERE account_id=? AND ok=1 AND (ts>=? OR ends_at>?)", id, cutoff, now)
	if err != nil {
		return nil, err
	}
	for _, value := range rows {
		claimed[shared.Str(shared.Obj(value)["plan_id"])] = true
	}
	// Re-read after discovery: a simultaneous manual claim or balance poll
	// may already have confirmed ownership while the upstream query ran.
	currentRows, err := storage.Query(a.db, "SELECT * FROM accounts WHERE id=?", id)
	if err != nil {
		return nil, err
	}
	if len(currentRows) == 0 {
		return []any{}, nil
	}
	current := shared.Obj(currentRows[0])
	for _, value := range shared.Arr(accountdata.BalanceData(current)["plans"]) {
		plan := shared.Obj(value)
		if shared.Integer(accountdata.SecondsMS(plan["ends_at"])) > now {
			claimed[shared.Str(plan["plan_id"])] = true
		}
	}
	ids := make([]string, 0, len(candidates))
	for planID := range candidates {
		if !claimed[planID] {
			ids = append(ids, planID)
		}
	}
	sort.Strings(ids)
	retained, result := []any{}, []any{}
	for _, planID := range ids {
		retained = append(retained, candidates[planID])
		result = append(result, candidates[planID]["plan"])
	}
	if err := a.setSetting(key, shared.M{"scope": scope, "plans": retained, "recovery_version": dailyClaimCandidateRecoveryVersion}); err != nil {
		return nil, err
	}
	// A fresh actionable entry can be shown even without an expiry, but it
	// cannot be carried forward into later discovery responses.
	for _, value := range fresh {
		plan := shared.Obj(value)
		planID := shared.Str(plan["plan_id"])
		if candidates[planID] == nil && !claimed[planID] && shared.Integer(plan["ends_at"]) == 0 {
			result = append(result, plan)
		}
	}
	return result, nil
}
