package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
)

const accountFields = "label family customer_id group_id status enc_tokens device_mid app_version card_started_at card_expires_at web_cookie_expires_at business_token_expires_at zcode_jwt_last_ok_at fail_streak cooldown_until capabilities last_balance last_error proxy_url last_ok_at last_used_at"

func (a *App) updateAccount(id string, m M) error {
	invalidate := m["status"] == "disabled" || m["status"] == "needs_relogin" || m["status"] == "web_only"
	for _, key := range []string{"enc_tokens", "device_mid", "proxy_url"} {
		if _, changed := m[key]; changed {
			invalidate = true
		}
	}
	if invalidate {
		a.balances.applyMu.Lock()
		defer a.balances.applyMu.Unlock()
	}
	err := a.update("accounts", "id", id, m, accountFields)
	if err == nil && invalidate {
		a.invalidateBalanceLocked(id)
	}
	return err
}
func (a *App) tokens(id string) map[string]string { return a.accountTokens(a.account(id)) }

func (a *App) accountTokens(r M) map[string]string {
	out := map[string]string{}
	for k, v := range obj(parse(r["enc_tokens"], nil)) {
		s, e := decrypt(str(v), a.secret)
		if e == nil {
			out[k] = s
		}
	}
	return out
}
func (a *App) setTokens(id string, m M) error {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	r := a.account(id)
	if r == nil {
		return fmt.Errorf("账号不存在")
	}
	enc, err := a.encryptedTokenChanges(r, m)
	if err != nil {
		return err
	}
	return a.updateAccount(id, M{"enc_tokens": enc})
}
func (a *App) insertAccount(fields, tokens M) (M, error) {
	plain := map[string]string{}
	for k, v := range tokens {
		if truth(v) {
			plain[k] = str(v)
		}
	}
	id, err := a.insertAccountRow(a.db, fields, plain, str(a.configValue("app_version")))
	if err != nil {
		return nil, err
	}
	return a.account(id), nil
}

func (a *App) insertAccountRow(db sqlStore, fields M, tokens map[string]string, appVersion string) (string, error) {
	enc := map[string]string{}
	for key, value := range tokens {
		ciphertext, err := encrypt(value, a.secret)
		if err != nil {
			return "", err
		}
		enc[key] = ciphertext
	}
	group := fields["group_id"]
	if _, ok := fields["group_id"]; !ok {
		groups, err := query(db, "SELECT id FROM groups WHERE enabled=1 AND role='primary' AND (family_filter IS NULL OR family_filter='' OR family_filter=?) ORDER BY created_at LIMIT 1", fallback(fields["family"], "bigmodel"))
		if err != nil {
			return "", err
		}
		if len(groups) > 0 {
			group = obj(groups[0])["id"]
		}
	}
	id := "acc_" + random(8)
	_, err := db.Exec("INSERT INTO accounts(id,label,family,customer_id,group_id,status,enc_tokens,device_mid,app_version,web_cookie_expires_at,business_token_expires_at,source,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", id, fallback(fields["label"], "账号"), fallback(fields["family"], "bigmodel"), fields["customer_id"], group, fallback(fields["status"], "unverified"), js(enc), fallback(fields["device_mid"], uuid()), appVersion, fields["web_cookie_expires_at"], fields["business_token_expires_at"], fallback(fields["source"], "local"), nowMS())
	return id, err
}
func jwtPayload(s string) M {
	p := strings.Split(s, ".")
	if len(p) < 2 {
		return nil
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(p[1], "="))
	if e != nil {
		return nil
	}
	v := parse(string(b), nil)
	m, _ := v.(map[string]any)
	return m
}
func (a *App) publicAccount(r M) M {
	m := clone(r)
	tokens := obj(parse(m["enc_tokens"], nil))
	delete(m, "enc_tokens")
	kinds := []any{}
	for k := range tokens {
		kinds = append(kinds, k)
	}
	m["token_kinds"] = kinds
	plain := a.accountTokens(r)
	m["login_provider"] = loginProvider(str(r["family"]), plain)
	m["business_customer_id"] = nullable(plain["business_customer_id"])
	m["zcode_jwt_expires_at"] = optionalExpiry(jwtExpiresAt(plain["zcode_jwt"]))
	for _, k := range []string{"zcode_jwt", "web_jwt", "business_token"} {
		m["has_"+k] = truth(tokens[k])
	}
	m["capabilities"] = parse(m["capabilities"], []any{})
	m["last_balance"] = parse(m["last_balance"], nil)
	m["platform_name"] = r["platform_name"]
	return m
}
func (a *App) fetchBalanceUpstream(ctx context.Context, r M) (M, error) {
	t := a.accountTokens(r)
	if loginJWTExpired(t) {
		return M{"ok": false, "authFailed": true, "httpStatus": 401, "failure_kind": "auth", "error_code": "login_expired", "reason": "ZCode 登录已过期，请重新授权"}, nil
	}
	res, e := a.api(ctx, r, "GET", "/api/v1/zcode-plan/billing/balance?app_version="+str(a.configValue("app_version")), str(t["zcode_jwt"]), nil, M{"X-Device-Mid": r["device_mid"], "X-Request-Id": uuid()}, a.jar(str(r["id"])))
	if e != nil {
		return nil, e
	}
	server := nowMS()
	if d, e := time.Parse(time.RFC1123, res.Header.Get("Date")); e == nil {
		server = d.UnixMilli()
	}
	data := obj(res.JSON["data"])
	if truth(data["server_time"]) {
		server = integer(data["server_time"]) * 1000
	}
	out := M{"ok": res.OK && codeIs(res.JSON["code"], 0), "authFailed": false, "httpStatus": res.Status, "plans": []any{}, "balances": []any{}, "capabilities": []any{}, "serverNowMs": server, "card": nil, "raw": res.JSON}
	if !truth(out["ok"]) {
		failure := apiResponseFailure(res)
		out["authFailed"] = failure.Kind == "auth"
		out["failure_kind"], out["error_code"] = failure.Kind, failure.Code
		out["retry_after_ms"] = failure.RetryAfter.Milliseconds()
		return out, nil
	}
	plans := arr(data["plans"])
	active := map[string]bool{}
	for _, v := range plans {
		p := obj(v)
		if str(p["status"]) != "active" {
			continue
		}
		if truth(p["ends_at"]) && integer(p["ends_at"])*1000 <= server {
			p["_staleExpired"] = true
			continue
		}
		active[str(first(p["user_plan_id"], p["plan_id"]))] = true
		if out["card"] == nil && (strings.Contains(lower(p["plan_id"]), "start-plan") || strings.Contains(lower(p["name"]), "start plan") || strings.Contains(lower(p["name"]), "start-plan")) {
			out["card"] = M{"userPlanId": p["user_plan_id"], "planId": p["plan_id"], "name": p["name"], "status": p["status"], "startedAt": secondsMS(p["starts_at"]), "endsAt": secondsMS(p["ends_at"])}
		}
	}
	balances, caps := []any{}, []any{}
	for _, v := range arr(data["balances"]) {
		b := obj(v)
		if truth(b["user_plan_id"]) && !active[str(b["user_plan_id"])] {
			continue
		}
		balances = append(balances, b)
		added := false
		for _, c := range arr(b["capabilities"]) {
			if s, ok := strings.CutPrefix(str(c), "model:"); ok {
				if !contains(caps, s) {
					caps = append(caps, s)
				}
				added = true
			}
		}
		if !added && truth(b["show_name"]) && !contains(caps, str(b["show_name"])) {
			caps = append(caps, b["show_name"])
		}
	}
	out["plans"], out["balances"], out["capabilities"] = plans, balances, caps
	return out, nil
}
func secondsMS(v any) any {
	if !truth(v) {
		return nil
	}
	return integer(v) * 1000
}
func (a *App) applyBalance(r, b M, snapshot bool) bool {
	if !truth(b["ok"]) {
		return false
	}
	id := str(r["id"])
	a.balances.applyMu.Lock()
	defer a.balances.applyMu.Unlock()
	r = a.account(id)
	if r == nil || r["status"] == "disabled" || !a.balanceResultCurrent(r, b) {
		return false
	}
	sequence := uint64(integer(b["_balance_sequence"]))
	if sequence > 0 && sequence < a.balances.applied[id] {
		return false
	}
	fields := M{"status": "active", "zcode_jwt_last_ok_at": nowMS(), "last_ok_at": nowMS(), "fail_streak": 0, "last_error": nil, "capabilities": b["capabilities"], "last_balance": b["raw"], "card_expires_at": nil}
	if c := obj(b["card"]); len(c) > 0 {
		fields["card_started_at"], fields["card_expires_at"] = c["startedAt"], c["endsAt"]
	}
	changed := balancePayloadFingerprint(parse(r["last_balance"], nil)) != balancePayloadFingerprint(b["raw"])
	for key, value := range fields {
		if key == "last_balance" || key == "last_ok_at" || key == "zcode_jwt_last_ok_at" {
			continue
		}
		previous := r[key]
		if key == "capabilities" {
			previous = parse(previous, []any{})
		}
		changed = changed || js(previous) != js(value)
	}
	if changed || nowMS()-integer(r["last_ok_at"]) >= balanceSnapshotBaseline.Milliseconds() {
		if err := a.updateAccount(id, fields); err != nil {
			return false
		}
	}
	if sequence > 0 {
		if a.balances.applied == nil {
			a.balances.applied = map[string]uint64{}
		}
		a.balances.applied[id] = sequence
	}
	a.mu.Lock()
	if s := a.states[id]; s != nil {
		s.Cooldown = 0
		if remainingBalance(balancePayload(b["raw"]), nowMS()) > 0 {
			s.QuotaExhausted = false
		}
	}
	a.notifyAvailableLocked()
	a.mu.Unlock()
	if snapshot {
		if err := a.saveBalanceSnapshot(id, arr(b["balances"])); err != nil {
			a.log.Printf("余额快照保存失败: %v", err)
		}
	}
	return true
}
func (a *App) verify(ctx context.Context, r M) (M, error) {
	id := str(r["id"])
	if r["status"] == "disabled" {
		return M{"ok": false, "status": "disabled", "reason": "账号已停用"}, nil
	}
	t := a.accountTokens(r)
	if !truth(t["zcode_jwt"]) {
		if truth(t["web_jwt"]) {
			res, e := a.customer(ctx, r, str(t["web_jwt"]))
			if e != nil {
				return nil, e
			}
			if res.OK {
				a.updateAccountStatusIfCurrent(r, nil, M{"status": "web_only", "last_ok_at": nowMS(), "last_error": nil})
			}
			return M{"ok": res.OK, "status": "web_only", "reason": "网页会话不可用于推理"}, nil
		}
		return M{"ok": false, "status": "unverified", "reason": "缺少可用凭据"}, nil
	}
	bal, e := a.fetchBalanceFresh(ctx, r)
	if e != nil {
		return nil, e
	}
	if truth(bal["ok"]) {
		if !a.applyBalance(r, bal, true) {
			return M{"ok": false, "status": a.account(id)["status"], "reason": "账号已更新，请重新验证"}, nil
		}
		publicBalance := clone(bal)
		delete(publicBalance, "_balance_scope")
		delete(publicBalance, "_balance_generation")
		delete(publicBalance, "_balance_sequence")
		return M{"ok": true, "status": "active", "balance": publicBalance}, nil
	}
	status := "cooldown"
	if str(r["status"]) == "unverified" {
		status = "unverified"
	}
	if truth(bal["authFailed"]) {
		status = "needs_relogin"
	}
	reason := str(fallback(bal["reason"], fmt.Sprintf("balance HTTP %v", bal["httpStatus"])))
	if !a.updateAccountStatusIfCurrent(r, bal, M{"status": status, "last_error": reason, "fail_streak": integer(r["fail_streak"]) + 1}) {
		return M{"ok": false, "status": a.account(id)["status"], "reason": "账号已更新，请重新验证"}, nil
	}
	return M{"ok": false, "status": status, "reason": reason}, nil
}
func balanceData(r M) M { return balancePayload(parse(r["last_balance"], nil)) }

func balancePayload(value any) M {
	d := obj(value)
	if x, ok := d["data"].(map[string]any); ok {
		return x
	}
	return d
}
func liveBalances(data M, now int64) []any {
	plans := arr(data["plans"])
	active := map[string]bool{}
	for _, v := range plans {
		p := obj(v)
		if str(p["status"]) == "active" && (!truth(p["ends_at"]) || integer(p["ends_at"])*1000 > now) {
			active[str(p["user_plan_id"])] = true
			active[str(p["plan_id"])] = true
		}
	}
	out := []any{}
	for _, v := range arr(data["balances"]) {
		b := obj(v)
		end := integer(first(b["expires_at"], b["period_end"]))
		if len(plans) > 0 && truth(b["user_plan_id"]) && !active[str(b["user_plan_id"])] {
			continue
		}
		if end > 0 && end*1000 <= now {
			continue
		}
		out = append(out, b)
	}
	return out
}

func remainingBalance(data M, now int64) float64 {
	remaining := float64(0)
	for _, value := range liveBalances(data, now) {
		remaining += num(obj(value)["remaining_units"])
	}
	return remaining
}
func (a *App) summary(ctx context.Context) M {
	out := M{"accounts": []any{}, "totalRemaining": 0, "totalUsed": 0, "totalGranted": 0, "totalPending": 0, "activeCount": 0, "count": 0}
	accounts := []any{}
	now := nowMS()
	deltas := map[string]any{}
	for _, v := range a.rows("SELECT account_id,SUM(delta) n FROM (SELECT account_id,MAX(0,MAX(used_units)-MIN(used_units)) delta FROM quota_snapshots WHERE ts>=? GROUP BY account_id,bucket_id) GROUP BY account_id", now-86400000) {
		r := obj(v)
		deltas[str(r["account_id"])] = r["n"]
	}
	rows := a.rows("SELECT * FROM accounts ORDER BY created_at")
	a.addPlatformNames(ctx, rows)
	for _, v := range rows {
		r := obj(v)
		m := a.publicAccount(r)
		d := balancePayload(m["last_balance"])
		buckets, pending := []any{}, []any{}
		remaining, used, total, pendingTotal := float64(0), float64(0), float64(0), float64(0)
		ents := map[string]bool{}
		for _, x := range liveBalances(d, now) {
			b := obj(x)
			remaining += num(b["remaining_units"])
			used += num(b["used_units"])
			total += num(b["total_units"])
			buckets = append(buckets, M{"name": first(b["show_name"], b["entitlement_id"], b["meter"], "额度"), "total": num(b["total_units"]), "used": num(b["used_units"]), "remaining": num(b["remaining_units"]), "unit": fallback(b["unit_type"], ""), "expires_at": secondsMS(first(b["expires_at"], b["period_end"])), "plan_id": b["plan_id"]})
			ents[str(b["entitlement_id"])] = true
			ents[str(b["plan_id"])+":"+str(first(b["show_name"], b["meter"]))] = true
		}
		for _, v := range arr(d["plans"]) {
			p := obj(v)
			if str(p["status"]) != "active" || (truth(p["ends_at"]) && integer(p["ends_at"])*1000 <= now) {
				continue
			}
			for _, v := range arr(p["entitlements"]) {
				e := obj(v)
				if integer(e["effective_at"])*1000 <= now || ents[str(e["entitlement_id"])] || ents[str(p["plan_id"])+":"+str(first(e["show_name"], e["meter"]))] {
					continue
				}
				pendingTotal += num(e["grant_units"])
				pending = append(pending, M{"name": first(e["show_name"], e["entitlement_id"], e["meter"], "额度"), "total": num(e["grant_units"]), "unit": fallback(e["unit_type"], ""), "effective_at": secondsMS(e["effective_at"]), "plan_id": p["plan_id"], "plan_name": p["name"]})
			}
		}
		sort.Slice(pending, func(i, j int) bool {
			return integer(obj(pending[i])["effective_at"]) < integer(obj(pending[j])["effective_at"])
		})
		m["remaining"], m["used"], m["total"] = remaining, used, total
		if len(d) == 0 {
			m["remaining"], m["used"], m["total"] = nil, nil, nil
		}
		m["pending_buckets"], m["pending_total"], m["quota_buckets"] = pending, pendingTotal, buckets
		m["balance_checked_at"] = r["zcode_jwt_last_ok_at"]
		m["expiring_in_min"] = nil
		if truth(r["card_expires_at"]) {
			m["expiring_in_min"] = (integer(r["card_expires_at"]) - now) / 60000
		}
		m["delta24h"] = num(deltas[str(r["id"])])
		accounts = append(accounts, m)
		out["totalRemaining"] = num(out["totalRemaining"]) + remaining
		out["totalUsed"] = num(out["totalUsed"]) + used
		out["totalGranted"] = num(out["totalGranted"]) + total
		out["totalPending"] = num(out["totalPending"]) + pendingTotal
		if str(r["status"]) == "active" {
			out["activeCount"] = num(out["activeCount"]) + 1
		}
	}
	out["accounts"], out["count"] = accounts, len(accounts)
	return out
}
