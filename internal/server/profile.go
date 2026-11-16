package server

import (
	"context"
	"strings"
	"time"
)

func profileName(p M) any {
	for _, m := range []M{p, obj(p["customer"])} {
		for _, k := range []string{"name", "displayName", "username", "userName", "customerName", "nickName"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	// Batch-registered accounts carry no official name; the masked email is
	// the only readable identity upstream returns, so surface its local part.
	for _, m := range []M{p, obj(p["customer"])} {
		if s, ok := m["email"].(string); ok {
			if local, _, found := strings.Cut(strings.TrimSpace(s), "@"); found && local != "" {
				return local
			}
		}
	}
	return nil
}

func profileCacheKey(r M) string {
	return hash(str(r["enc_tokens"]) + "\x00" + str(r["family"]) + "\x00" + str(r["proxy_url"]))
}

// Profile is authenticated with the ZCode login, never a linked business token.
// A missing link is not proof of unlinking; only a new login clears old material.
func (a *App) refreshLoginProfile(ctx context.Context, r M, tokens map[string]string) M {
	if tokens["zcode_jwt"] == "" || loginJWTExpired(tokens) || r["status"] == "disabled" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	res, err := a.api(ctx, r, "GET", "/api/v1/oauth/profile", tokens["zcode_jwt"], nil, nil, nil)
	if err != nil || !res.OK || !codeIs(res.JSON["code"], 0) {
		return nil
	}
	data := obj(res.JSON["data"])
	profile := loginProfile(obj(data["user"]))
	identity := str(jwtPayload(tokens["zcode_jwt"])["user_id"])
	if identity == "" || str(profile["user_id"]) != identity {
		return nil
	}
	changes, fields := M{}, M{}
	if js(profile) != tokens["provider_user_info"] {
		changes["provider_user_info"] = js(profile)
	}
	linked := M{"active_provider": loginProvider(str(r["family"]), tokens)}
	attachLinkedBigModel(linked, identity, obj(data["bigmodel"]))
	for _, key := range []string{"linked_bigmodel", "business_token", "business_customer_id"} {
		if value := linked[key]; truth(value) && str(value) != tokens[key] {
			changes[key] = value
		}
	}
	if truth(changes["business_token"]) {
		fields["business_token_expires_at"] = optionalExpiry(jwtExpiresAt(str(changes["business_token"])))
	}
	if len(changes) > 0 {
		if changed, err := a.updateLoginMaterialIfCurrent(r, changes, fields); err != nil || !changed {
			return nil
		}
	}
	return profile
}

func (a *App) platformName(ctx context.Context, r M) any {
	id := str(r["id"])
	key := profileCacheKey(r)
	a.mu.Lock()
	if a.profiles == nil {
		a.profiles = map[string]*profileEntry{}
	}
	if cached := a.profiles[id]; cached != nil && cached.Key == key && cached.Until > nowMS() {
		done := cached.Done
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return cached.Name
		}
	}
	entry := &profileEntry{Key: key, Until: nowMS() + 300000, Done: make(chan struct{})}
	a.profiles[id] = entry
	a.mu.Unlock()
	defer close(entry.Done)
	tokens := a.accountTokens(r)
	entry.Name = profileName(obj(parse(tokens["provider_user_info"], nil)))
	provider := loginProvider(str(r["family"]), tokens)
	if entry.Name == nil && (provider == "zai_cn" || provider == "zai") {
		if profile := a.refreshLoginProfile(ctx, r, tokens); profile != nil {
			if name := profileName(profile); name != nil {
				entry.Name = name
			}
			if current := a.account(id); a.sameLoginSession(r, current) {
				r, tokens = current, a.accountTokens(current)
				a.mu.Lock()
				entry.Key = profileCacheKey(current)
				a.mu.Unlock()
			}
		}
	}
	if entry.Name != nil {
		return entry.Name
	}
	for _, k := range []string{"business_token", "web_jwt", "zcode_jwt"} {
		if entry.Name = profileName(jwtPayload(tokens[k])); entry.Name != nil {
			return entry.Name
		}
	}
	if tokens["zcode_jwt"] != "" {
		if res, err := a.businessCustomer(ctx, r); err == nil && res.OK {
			entry.Name = profileName(obj(res.JSON["data"]))
		}
	} else if token := tokens["web_jwt"]; token != "" {
		if res, e := a.customer(ctx, r, token); e == nil && res.OK {
			entry.Name = profileName(obj(res.JSON["data"]))
		}
	}
	return entry.Name
}
func (a *App) addPlatformNames(ctx context.Context, accounts []any) {
	parallelFor(len(accounts), 4, func(i int) {
		if ctx.Err() == nil {
			r := obj(accounts[i])
			r["platform_name"] = a.platformName(ctx, r)
		}
	})
}
