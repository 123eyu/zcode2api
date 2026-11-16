package server

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// Login providers and billing families are distinct in ZCode 3.18.2.
// zai_cn uses a ZCode subject and may optionally link a BigModel customer.
func validLoginProvider(provider string) bool {
	return provider == "bigmodel" || provider == "zai" || provider == "zai_cn"
}

func loginFamily(provider string) string {
	if provider == "zai_cn" {
		return "bigmodel"
	}
	return provider
}

func loginProvider(family string, tokens map[string]string) string {
	if provider := tokens["active_provider"]; provider != "" {
		return provider
	}
	// Existing installations and older exports did not record the login channel.
	return family
}

func (a *App) accountLoginProvider(r M) string {
	return loginProvider(str(r["family"]), a.accountTokens(r))
}

func jwtExpiresAt(token string) int64 {
	exp := num(jwtPayload(token)["exp"])
	if math.IsNaN(exp) || math.IsInf(exp, 0) || exp <= 0 || exp > 9007199254740 {
		return 0
	}
	return int64(exp * 1000)
}

func optionalExpiry(expiry int64) any {
	if expiry <= 0 {
		return nil
	}
	return expiry
}

func (a *App) sameLoginSession(left, right M) bool {
	if left == nil || right == nil || left["id"] != right["id"] || left["family"] != right["family"] || left["customer_id"] != right["customer_id"] {
		return false
	}
	l, r := a.accountTokens(left), a.accountTokens(right)
	return loginProvider(str(left["family"]), l) == loginProvider(str(right["family"]), r) && l["zcode_jwt"] == r["zcode_jwt"] && l["session_revision"] == r["session_revision"]
}

// Serialize result application with credential replacement and disabling. An
// old request must not mark a freshly authorized account as needing login.
func (a *App) updateAccountStatusIfCurrent(snapshot, balance, fields M) bool {
	a.balances.applyMu.Lock()
	defer a.balances.applyMu.Unlock()
	id := str(snapshot["id"])
	current := a.account(id)
	if current == nil || current["status"] == "disabled" || current["enc_tokens"] != snapshot["enc_tokens"] || current["device_mid"] != snapshot["device_mid"] || current["proxy_url"] != snapshot["proxy_url"] || balance != nil && !a.balanceResultCurrent(current, balance) {
		return false
	}
	if err := a.update("accounts", "id", id, fields, accountFields); err != nil {
		return false
	}
	if fields["status"] == "needs_relogin" || fields["status"] == "web_only" {
		a.invalidateBalanceLocked(id)
	}
	return true
}

func loginJWTExpired(tokens map[string]string) bool {
	expiry := jwtExpiresAt(tokens["zcode_jwt"])
	return expiry > 0 && expiry <= nowMS()+30000
}

func loginProfile(profile M) M {
	// Store useful identity fields, never an arbitrary response or avatar blob.
	result := M{}
	for _, key := range []string{"user_id", "id", "name", "username", "displayName", "email", "customerNumber", "customerName", "nickName"} {
		if value, ok := profile[key].(string); ok && strings.TrimSpace(value) != "" {
			result[key] = value
		}
	}
	return result
}

func newLoginTokens(provider, jwt string, profile M) (M, string, error) {
	identity := str(jwtPayload(jwt)["user_id"])
	if !validLoginProvider(provider) || jwt == "" || identity == "" || len(strings.Split(jwt, ".")) != 3 {
		return nil, "", fmt.Errorf("缺少有效的 ZCode 登录身份或登录渠道")
	}
	if subject := str(profile["user_id"]); subject != "" && subject != identity {
		return nil, "", fmt.Errorf("用户资料与 ZCode 凭据身份不一致，未更新账号")
	}
	// A new session replaces all channel-specific material in one write. Missing
	// refresh/linked tokens must not inherit credentials from an older login.
	tokens := M{
		"zcode_jwt": jwt, "active_provider": provider, "session_revision": uuid(),
		"provider_access_token": nil, "provider_access_kind": nil, "refresh_token": nil,
		"oauth_access": nil, "business_token": nil, "business_customer_id": nil,
		"business_token_obtained_at": nil,
		"linked_bigmodel":            nil, "provider_user_info": nil,
	}
	if cleaned := loginProfile(profile); len(cleaned) > 0 {
		tokens["provider_user_info"] = js(cleaned)
	}
	return tokens, identity, nil
}

func attachLinkedBigModel(tokens M, identity string, linked M) {
	// Local records use camelCase; backend OAuth/profile responses use snake_case.
	if owner := str(linked["userId"]); owner != "" && owner != identity {
		return
	}
	customer := strings.TrimSpace(str(first(linked["customerId"], linked["customer_id"])))
	access := strings.TrimSpace(str(first(linked["accessToken"], linked["access_token"])))
	if customer == "" || access == "" {
		return
	}
	tokens["linked_bigmodel"] = js(M{"userId": identity, "customerId": customer, "accessToken": access})
	if tokens["active_provider"] == "zai_cn" {
		tokens["business_token"], tokens["business_customer_id"] = access, customer
	}
}

func (a *App) encryptedTokenChanges(account, changes M) (M, error) {
	enc := obj(parse(account["enc_tokens"], nil))
	for key, value := range changes {
		if value == nil || str(value) == "" {
			delete(enc, key)
			continue
		}
		ciphertext, err := encrypt(str(value), a.secret)
		if err != nil {
			return nil, err
		}
		enc[key] = ciphertext
	}
	return enc, nil
}

func (a *App) saveLoginAccount(target, identity string, fields, tokens M) (M, bool, error) {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	a.balances.applyMu.Lock()
	defer a.balances.applyMu.Unlock()
	provider := str(tokens["active_provider"])
	family := loginFamily(provider)
	var account M
	if target != "" {
		account = a.account(target)
		if account == nil || str(account["customer_id"]) != identity || a.accountLoginProvider(account) != provider {
			return nil, false, fmt.Errorf("授权账号或登录渠道与原账号不一致，未更新凭据")
		}
	} else {
		rows, err := query(a.db, "SELECT * FROM accounts WHERE family=? AND customer_id=? ORDER BY created_at", family, identity)
		if err != nil {
			return nil, false, err
		}
		for _, row := range rows {
			candidate := obj(row)
			if a.accountLoginProvider(candidate) == provider {
				account = candidate
				break
			}
		}
	}
	fields = clone(fields)
	fields["family"], fields["customer_id"] = family, identity
	if _, exists := fields["business_token_expires_at"]; !exists {
		fields["business_token_expires_at"] = optionalExpiry(jwtExpiresAt(str(tokens["business_token"])))
	}
	if account == nil {
		created, err := a.insertAccount(fields, tokens)
		return created, false, err
	}
	previous := a.accountTokens(account)
	// A desktop reimport of the exact same login need not discard OAuth material
	// already retained by this gateway, but a new revision/JWT cannot reuse it.
	sameSession := previous["zcode_jwt"] == str(tokens["zcode_jwt"]) && previous["session_revision"] == str(tokens["session_revision"])
	if sameSession && !truth(tokens["oauth_access"]) && previous["oauth_access"] != "" {
		tokens["oauth_access"] = previous["oauth_access"]
	}
	if sameSession && provider == "zai" && !truth(tokens["business_token"]) && truth(tokens["oauth_access"]) && str(tokens["oauth_access"]) == previous["oauth_access"] {
		tokens["business_token"] = nullable(previous["business_token"])
		tokens["business_token_obtained_at"] = nullable(previous["business_token_obtained_at"])
		fields["business_token_expires_at"] = account["business_token_expires_at"]
	}
	if previous["business_token"] == str(tokens["business_token"]) && fields["business_token_expires_at"] == nil {
		fields["business_token_expires_at"] = account["business_token_expires_at"]
	}
	enc, err := a.encryptedTokenChanges(account, tokens)
	if err != nil {
		return nil, false, err
	}
	changes := M{"enc_tokens": enc, "business_token_expires_at": fields["business_token_expires_at"]}
	if truth(fields["device_mid"]) {
		changes["device_mid"] = fields["device_mid"]
	}
	if account["status"] != "disabled" {
		changes["status"], changes["last_error"], changes["fail_streak"] = "unverified", nil, 0
	}
	id := str(account["id"])
	if err := a.update("accounts", "id", id, changes, accountFields); err != nil {
		return nil, false, err
	}
	a.invalidateBalanceLocked(id)
	return a.account(id), true, nil
}

func (a *App) verifyImportedAccount(ctx context.Context, account M) M {
	id := str(account["id"])
	if account["status"] == "disabled" {
		return M{"ok": false, "id": id, "status": "disabled", "reason": "账号保持停用"}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result, err := a.verify(verifyCtx, account)
	if err != nil {
		reason := conciseFailureMessage(upstreamRequestFailure(verifyCtx, err))
		if verifyCtx.Err() == context.DeadlineExceeded {
			reason = "验证超时，可在账号列表重新验证"
		}
		result = M{"ok": false, "status": "unverified", "reason": reason}
		// A response for an older credential must not overwrite a later login.
		if !a.updateAccountStatusIfCurrent(account, nil, M{"last_error": reason}) {
			result["status"], result["reason"] = a.account(id)["status"], "账号已更新，请在账号列表查看"
		}
	}
	result["id"] = id
	a.scheduleNext(id, time.Second)
	return result
}
