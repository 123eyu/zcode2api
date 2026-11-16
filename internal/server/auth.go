package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"
	settings "zcode2api/internal/server/config"
	"zcode2api/internal/server/fault"
	wire "zcode2api/internal/server/protocol"
	"zcode2api/internal/server/shared"
)

const adminSessionTTL = 24 * time.Hour

func bearer(r *http.Request) string {
	s := r.Header.Get("Authorization")
	if strings.HasPrefix(s, "Bearer ") {
		return strings.TrimSpace(s[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func adminSessionToken(r *http.Request) string {
	token := bearer(r)
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	return token
}

func (a *App) adminCredentialHash() string {
	mac := hmac.New(sha256.New, []byte(a.secret))
	_, _ = mac.Write([]byte(shared.Str(a.configValue("admin_password"))))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *App) newAdminSession(ctx context.Context) (string, int64, error) {
	now, credentialHash := shared.NowMS(), a.adminCredentialHash()
	if _, err := a.db.ExecContext(ctx, "DELETE FROM admin_sessions WHERE expires_at<=? OR credential_hash<>?", now, credentialHash); err != nil {
		return "", 0, err
	}
	token, expiry := shared.Random(24), now+adminSessionTTL.Milliseconds()
	_, err := a.db.ExecContext(ctx, "INSERT INTO admin_sessions(token_hash,credential_hash,expires_at) VALUES(?,?,?)", shared.Hash(token), credentialHash, expiry)
	if err != nil {
		return "", 0, err
	}
	return token, expiry, nil
}

func (a *App) adminAuth(r *http.Request) (bool, error) {
	token := adminSessionToken(r)
	if token == "" {
		return false, nil
	}
	var expiry int64
	err := a.db.QueryRowContext(r.Context(), "SELECT expires_at FROM admin_sessions WHERE token_hash=? AND credential_hash=?", shared.Hash(token), a.adminCredentialHash()).Scan(&expiry)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && expiry > shared.NowMS(), err
}

func (a *App) password(v any) bool {
	s, ok := v.(string)
	if !ok || s == "" {
		return false
	}
	x, y := shared.Hash(s), shared.Hash(shared.Str(a.configValue("admin_password")))
	return subtle.ConstantTimeCompare([]byte(x), []byte(y)) == 1
}

func ipAllowed(ip string, patterns []any) bool {
	for _, v := range patterns {
		p := strings.TrimSpace(shared.Str(v))
		if p == "" {
			continue
		}
		if ip == p {
			return true
		}
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(ip, strings.TrimSuffix(p, "*")) {
			return true
		}
		if _, n, e := net.ParseCIDR(p); e == nil && n.Contains(net.ParseIP(ip)) {
			return true
		}
	}
	return false
}

func (a *App) authenticate(r *http.Request) (shared.M, *fault.Failure) {
	secret := bearer(r)
	if secret == "" {
		return nil, &fault.Failure{Status: 401, Code: "invalid_api_key", Message: "缺少 API 密钥"}
	}
	k := a.row("SELECT * FROM keys WHERE key_hash=?", shared.Hash(secret))
	if k == nil {
		return nil, &fault.Failure{Status: 401, Code: "invalid_api_key", Message: "无效密钥"}
	}
	if !shared.Truth(k["enabled"]) {
		return nil, &fault.Failure{Status: 403, Code: "invalid_api_key", Message: "密钥已禁用"}
	}
	if shared.Truth(k["expires_at"]) && shared.Integer(k["expires_at"]) < shared.NowMS() {
		return nil, &fault.Failure{Status: 403, Code: "invalid_api_key", Message: "密钥已过期"}
	}
	if k["quota_limit"] != nil && shared.Num(k["used_units"]) >= shared.Num(k["quota_limit"]) {
		return nil, &fault.Failure{Status: 429, Code: "quota_exceeded", Message: "密钥配额已用尽"}
	}
	ips := shared.Arr(shared.Parse(k["ip_whitelist"], nil))
	if len(ips) > 0 && !ipAllowed(a.ip(r), ips) {
		return nil, &fault.Failure{Status: 403, Code: "ip_not_allowed", Message: "IP 不在白名单"}
	}
	_ = a.exec("UPDATE keys SET last_used_at=? WHERE id=?", shared.NowMS(), k["id"])
	return k, nil
}

func (a *App) createKey(b shared.M) (shared.M, error) {
	id, secret := "key_"+shared.Random(4), "zk2-"+shared.Random(20)
	level := shared.First(b["defaultLevel"], b["default_level"])
	if !settings.ValidLevel(shared.Str(level)) {
		level = nil
	}
	groups := shared.First(b["groupOrder"], b["group_order"])
	if groups != nil {
		groups = shared.JSON(groups)
	}
	e := a.exec("INSERT INTO keys(id,name,key_hash,key_tail,quota_limit,expires_at,ip_whitelist,model_whitelist,group_order,sticky_minutes,default_level,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", id, shared.Fallback(b["name"], "default"), shared.Hash(secret), secret[len(secret)-6:], shared.First(b["quotaLimit"], b["quota_limit"]), shared.First(b["expiresAt"], b["expires_at"]), shared.JSON(shared.First(b["ipWhitelist"], b["ip_whitelist"], []any{})), shared.JSON(shared.First(b["modelWhitelist"], b["model_whitelist"], []any{})), groups, shared.First(b["stickyMinutes"], b["sticky_minutes"], 0), level, shared.NowMS())
	return shared.M{"ok": e == nil, "id": id, "secret": secret}, e
}

func (a *App) keyChain(k shared.M) []any {
	if s := k["group_order"]; s != nil {
		if v, ok := shared.Parse(s, nil).([]any); ok {
			return v
		}
	}
	return a.defaultChain()
}

func (a *App) modelAllowed(k shared.M, model string) bool {
	list := shared.Arr(shared.Parse(k["model_whitelist"], nil))
	if len(list) == 0 {
		return true
	}
	for _, v := range list {
		if strings.EqualFold(a.resolveModel(shared.Str(v)), model) {
			return true
		}
	}
	return false
}

func (a *App) resolveModel(s string) string {
	s = strings.TrimSpace(s)
	s = wire.ModelSuffix.ReplaceAllString(s, "")
	for alias, v := range shared.Obj(a.configValue("aliases")) {
		if strings.EqualFold(alias, s) {
			return shared.Str(v)
		}
	}
	if s == "" {
		return "GLM-5.3-Flash"
	}
	return s
}

func (a *App) effectiveEffort(b, k shared.M) string {
	if v := settings.Effort(b); v != "" {
		return v
	}
	if v := shared.Str(k["default_level"]); settings.ValidLevel(v) {
		return v
	}
	v := shared.Str(a.configValue("default_reasoning_level"))
	if settings.ValidLevel(v) {
		return v
	}
	return ""
}
