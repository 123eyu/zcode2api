package server

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"time"
)

func publicFlow(r M) M {
	return M{"ok": true, "flow_id": r["flow_id"], "status": r["status"], "error": r["error"], "account_id": r["account_id"], "result": parse(r["result_json"], nil), "authorize_url": r["authorize_url"], "expires_at": r["expires_at"], "provider": r["provider"], "target_id": r["account_rowid"]}
}
func (a *App) startOAuth(ctx context.Context, provider, target string, force bool) (M, error) {
	if !validLoginProvider(provider) {
		return nil, fmt.Errorf("provider 须为 zai_cn、zai 或 bigmodel")
	}
	if target != "" {
		r := a.account(target)
		if r == nil || a.accountLoginProvider(r) != provider {
			return nil, fmt.Errorf("账号不存在或授权平台不匹配")
		}
		a.mu.Lock()
		if a.flowLocks["start:"+target] {
			a.mu.Unlock()
			return M{"ok": false, "error": "正在准备授权流程"}, nil
		}
		a.flowLocks["start:"+target] = true
		last := a.reauth[target]
		a.mu.Unlock()
		defer func() { a.mu.Lock(); delete(a.flowLocks, "start:"+target); a.mu.Unlock() }()
		if r := a.row("SELECT * FROM oauth_flows WHERE account_rowid=? AND status='pending' AND expires_at>?", target, nowMS()); r != nil {
			return merge(publicFlow(r), M{"reused": true}), nil
		}
		if !force && last > 0 && nowMS()-last < 30*60000 {
			return nil, nil
		}
		a.mu.Lock()
		a.reauth[target] = nowMS()
		a.mu.Unlock()
	}
	token := random(32)
	res, e := a.api(ctx, nil, "POST", "/api/v1/oauth/cli/init", token, M{"provider": provider}, nil, nil)
	if e != nil {
		return nil, e
	}
	if !res.OK || !codeIs(res.JSON["code"], 0) {
		return nil, apiFailure(res, "授权初始化失败")
	}
	d := obj(res.JSON["data"])
	id := str(d["flow_id"])
	authorizeURL := str(d["authorize_url"])
	u, e := url.Parse(authorizeURL)
	if id == "" || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Query().Get("state") == "" {
		return nil, fmt.Errorf("授权初始化响应无效")
	}
	expires := integer(d["expires_at"]) * 1000
	interval := num(d["poll_interval_sec"])
	if !integerInRange(d["expires_at"], 1, 9007199254740) || math.IsNaN(interval) || math.IsInf(interval, 0) || expires <= nowMS() || interval < 1 || interval*1000 >= float64(expires-nowMS()) {
		return nil, fmt.Errorf("授权初始化响应的有效期或轮询间隔无效")
	}
	// This is a CLI polling flow. Its server-issued HTTPS callback completes
	// the flow we poll; a zcode:// callback instead signs in the desktop app.
	// Preserve the entire URL, including each provider's redirect and state.
	enc, e := encrypt(token, a.secret)
	if e != nil {
		return nil, e
	}
	e = a.exec("INSERT INTO oauth_flows(flow_id,account_rowid,provider,poll_token_enc,authorize_url,status,created_at,expires_at,poll_interval_sec,updated_at) VALUES(?,?,?,?,?,'pending',?,?,?,?)", id, nullable(target), provider, enc, authorizeURL, nowMS(), expires, interval, nowMS())
	if e != nil {
		return nil, e
	}
	a.watchOAuth(id)
	return M{"ok": true, "reused": false, "flow_id": id, "authorize_url": authorizeURL, "expires_at": expires, "provider": provider}, nil
}

func (a *App) restoreOAuth() {
	for _, v := range a.rows("SELECT * FROM oauth_flows WHERE status IN ('pending','exchanging')") {
		r := obj(v)
		id := str(r["flow_id"])
		if str(r["status"]) == "exchanging" {
			_ = a.exec("UPDATE oauth_flows SET status='failed',poll_token_enc='',error=?,updated_at=? WHERE flow_id=?", "服务在令牌处理中断，请重新授权", nowMS(), id)
		} else {
			a.watchOAuth(id)
		}
	}
}
func (a *App) watchOAuth(id string) {
	a.mu.Lock()
	if a.flowLocks[id] {
		a.mu.Unlock()
		return
	}
	a.flowLocks[id] = true
	a.mu.Unlock()
	a.background(func() {
		defer func() { a.mu.Lock(); delete(a.flowLocks, id); a.mu.Unlock() }()
		for {
			r := a.row("SELECT * FROM oauth_flows WHERE flow_id=?", id)
			if r == nil || str(r["status"]) != "pending" {
				return
			}
			if integer(r["expires_at"]) <= nowMS() {
				_ = a.exec("UPDATE oauth_flows SET status='expired',poll_token_enc='',error=?,updated_at=? WHERE flow_id=? AND status='pending'", "授权流程已过期", nowMS(), id)
				return
			}
			if pause(a.ctx, time.Duration(max(1, num(r["poll_interval_sec"]))*float64(time.Second))) != nil {
				return
			}
			if integer(r["expires_at"]) <= nowMS() {
				continue
			}
			token, e := decrypt(str(r["poll_token_enc"]), a.secret)
			if e != nil {
				_ = a.exec("UPDATE oauth_flows SET status='failed',poll_token_enc='',error=?,updated_at=? WHERE flow_id=? AND status='pending'", "无法恢复授权流程", nowMS(), id)
				return
			}
			res, e := a.api(a.ctx, nil, "GET", "/api/v1/oauth/cli/poll/"+url.PathEscape(id), token, nil, nil, nil)
			// Expiry while waiting for the network is still an expired flow,
			// not an unexplained HTTP 400 or a late credential update.
			if integer(r["expires_at"]) <= nowMS() {
				continue
			}
			if e != nil {
				_ = a.exec("UPDATE oauth_flows SET error=?,updated_at=? WHERE flow_id=? AND status='pending'", "轮询暂时失败: "+e.Error(), nowMS(), id)
				continue
			}
			if res.Status >= 400 && res.Status < 500 && res.Status != 408 && res.Status != 429 {
				_ = a.exec("UPDATE oauth_flows SET status='failed',poll_token_enc='',error=?,updated_at=? WHERE flow_id=? AND status='pending'", fmt.Sprintf("授权失败 HTTP %d", res.Status), nowMS(), id)
				return
			}
			if !res.OK {
				continue
			}
			if !codeIs(res.JSON["code"], 0) {
				_ = a.exec("UPDATE oauth_flows SET status='failed',poll_token_enc='',error=?,updated_at=? WHERE flow_id=? AND status='pending'", "授权状态查询被上游拒绝，请重新授权", nowMS(), id)
				return
			}
			d := obj(res.JSON["data"])
			state := str(d["status"])
			if state == "failed" || state == "expired" {
				_ = a.exec("UPDATE oauth_flows SET status=?,poll_token_enc='',error=?,updated_at=? WHERE flow_id=? AND status='pending'", state, "授权失败", nowMS(), id)
				return
			}
			if state == "pending" {
				continue
			}
			if state != "ready" {
				_ = a.exec("UPDATE oauth_flows SET status='failed',poll_token_enc='',error=?,updated_at=? WHERE flow_id=? AND status='pending'", "授权状态响应无效，请重新授权", nowMS(), id)
				return
			}
			result, e := a.db.Exec("UPDATE oauth_flows SET status='exchanging',updated_at=? WHERE flow_id=? AND status='pending'", nowMS(), id)
			if e != nil {
				return
			}
			n, _ := result.RowsAffected()
			if n == 0 {
				return
			}
			completed, e := a.completeOAuth(a.ctx, r, d)
			if e != nil {
				_ = a.exec("UPDATE oauth_flows SET status='failed',poll_token_enc='',error=?,updated_at=? WHERE flow_id=?", e.Error(), nowMS(), id)
			} else {
				_ = a.exec("UPDATE oauth_flows SET status='done',poll_token_enc='',error=NULL,account_id=?,result_json=?,updated_at=? WHERE flow_id=?", completed["id"], js(completed), nowMS(), id)
			}
			return
		}
	})
}
func (a *App) completeOAuth(ctx context.Context, flow, data M) (M, error) {
	provider := str(flow["provider"])
	if !validLoginProvider(provider) {
		return nil, fmt.Errorf("授权登录渠道无效")
	}
	if !truth(data["token"]) {
		if !truth(data["code"]) || !truth(data["state"]) {
			return nil, fmt.Errorf("授权响应缺少 ZCode token 或有效的授权码")
		}
		u, err := url.Parse(str(flow["authorize_url"]))
		if err != nil || u.Query().Get("state") != str(data["state"]) {
			return nil, fmt.Errorf("授权响应与当前流程不匹配，请重新授权")
		}
		redirectKey := "redirect_uri"
		if provider == "bigmodel" {
			redirectKey = "redirect"
		}
		redirect := u.Query().Get(redirectKey)
		if redirect == "" {
			return nil, fmt.Errorf("授权流程缺少回调地址，请重新授权")
		}
		res, e := a.api(ctx, nil, "POST", "/api/v1/oauth/token", "", M{"provider": provider, "code": data["code"], "redirect_uri": redirect, "state": data["state"]}, nil, nil)
		if e != nil {
			return nil, e
		}
		if !res.OK || !codeIs(res.JSON["code"], 0) {
			return nil, apiFailure(res, "令牌交换失败")
		}
		data = obj(res.JSON["data"])
	}
	profile := obj(data["user"])
	if provider != "bigmodel" && !truth(profile["user_id"]) {
		return nil, fmt.Errorf("授权响应缺少 ZCode 用户身份")
	}
	tokens, identity, err := newLoginTokens(provider, str(data["token"]), profile)
	if err != nil {
		return nil, err
	}
	var expiry any
	if provider == "bigmodel" {
		d := obj(data["bigmodel"])
		access := first(nullable(d["access_token"]), nullable(d["accessToken"]), nullable(data["access_token"]), nullable(data["accessToken"]))
		if !truth(access) {
			return nil, fmt.Errorf("授权响应缺少 BigModel 业务令牌")
		}
		tokens["provider_access_token"], tokens["provider_access_kind"], tokens["business_token"] = access, "business", access
		tokens["refresh_token"] = first(nullable(d["refresh_token"]), nullable(d["refreshToken"]))
		tokens["business_customer_id"] = nullable(d["customer_id"])
		expiry = optionalExpiry(jwtExpiresAt(str(access)))
	} else {
		d := obj(data["zai"])
		access := first(nullable(d["access_token"]), nullable(d["accessToken"]))
		if !truth(access) {
			return nil, fmt.Errorf("授权响应缺少当前渠道的 OAuth 令牌")
		}
		tokens["provider_access_token"], tokens["provider_access_kind"] = access, "oauth"
		tokens["refresh_token"] = first(nullable(d["refresh_token"]), nullable(d["refreshToken"]))
		attachLinkedBigModel(tokens, identity, obj(data["bigmodel"]))
		if provider == "zai" {
			// Start Plan needs only the main JWT. Retain the raw OAuth token and
			// exchange lazily when a business endpoint is actually requested.
			tokens["oauth_access"] = access
		} else {
			// CN access tokens must never be submitted to the global Z.ai login API.
			expiry = optionalExpiry(jwtExpiresAt(str(tokens["business_token"])))
		}
	}
	account, updated, err := a.saveLoginAccount(str(flow["account_rowid"]), identity, M{"label": "OAuth-" + provider + "-" + identity[max(0, len(identity)-4):], "source": "oauth", "business_token_expires_at": expiry}, tokens)
	if err != nil {
		return nil, err
	}
	verified := a.verifyImportedAccount(ctx, account)
	a.mu.Lock()
	delete(a.reauth, str(account["id"]))
	a.mu.Unlock()
	return M{"id": account["id"], "updated": updated, "verified": verified["ok"], "status": verified["status"], "reason": verified["reason"]}, nil
}
