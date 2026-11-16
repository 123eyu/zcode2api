package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func testOAuthURL(provider string) string {
	key, host := "redirect_uri", "chat.z.ai"
	if provider == "zai_cn" {
		host = "zchat.z.ai"
	} else if provider == "bigmodel" {
		key, host = "redirect", "bigmodel.cn"
	}
	return "https://" + host + "/login?" + key + "=" + url.QueryEscape("https://zcode.z.ai/api/v1/oauth/cli/callback/"+provider) + "&state=flow-state&client_id=zcode&extra=keep%20this"
}

func testOAuthReady(provider string) M {
	data := M{"status": "ready", "token": testJWT(`{"user_id":"oauth-user"}`), "user": M{"user_id": "oauth-user", "name": "OAuth user"}}
	key := "zai"
	if provider == "bigmodel" {
		key = "bigmodel"
	}
	data[key] = M{"access_token": provider + "-access", "refresh_token": provider + "-refresh"}
	return data
}

func TestOAuthPollingPreservesCallbackAndSavesAccount(t *testing.T) {
	for _, provider := range []string{"zai_cn", "zai", "bigmodel"} {
		t.Run(provider, func(t *testing.T) {
			app := testApp(t)
			authorizeURL := testOAuthURL(provider)
			var pollAuthorization string
			testInstallClient(app, "", func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/v1/oauth/cli/init":
					var body M
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						return nil, err
					}
					if body["provider"] != provider || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
						return nil, fmt.Errorf("invalid OAuth initialization")
					}
					pollAuthorization = r.Header.Get("Authorization")
					return testAPIResponse(r, 200, js(M{"code": 0, "data": M{"flow_id": "flow", "authorize_url": authorizeURL, "expires_at": nowMS()/1000 + 300, "poll_interval_sec": 1}})), nil
				case "/api/v1/oauth/cli/poll/flow":
					if r.Header.Get("Authorization") != pollAuthorization {
						return nil, fmt.Errorf("polling credential changed")
					}
					return testAPIResponse(r, 200, js(M{"code": 0, "data": testOAuthReady(provider)})), nil
				case "/api/v1/zcode-plan/billing/balance":
					return testAPIResponse(r, 200, `{"code":0,"data":{"balances":[]}}`), nil
				default:
					return nil, fmt.Errorf("unexpected request: %s", r.URL.Path)
				}
			})
			flow, err := app.startOAuth(context.Background(), provider, "", true)
			testMust(t, err)
			testEqual(t, flow["authorize_url"], authorizeURL)
			testEqual(t, app.row("SELECT authorize_url FROM oauth_flows WHERE flow_id='flow'")["authorize_url"], authorizeURL)
			testWait(t, func() bool { return app.row("SELECT status FROM oauth_flows WHERE flow_id='flow'")["status"] == "done" })
			saved := app.row("SELECT * FROM oauth_flows WHERE flow_id='flow'")
			account := app.account(str(saved["account_id"]))
			if account == nil {
				t.Fatal("completed OAuth did not save an account")
			}
			testEqual(t, saved["poll_token_enc"], "")
			testEqual(t, obj(parse(saved["result_json"], nil))["verified"], true)
			testEqual(t, account["family"], loginFamily(provider))
			testEqual(t, app.accountLoginProvider(account), provider)
			testEqual(t, app.accountTokens(account)["provider_access_token"], provider+"-access")
			testEqual(t, app.accountTokens(account)["refresh_token"], provider+"-refresh")
			testEqual(t, len(app.rows("SELECT id FROM accounts")), 1)
		})
	}
}

func TestOAuthCodeExchangeUsesFlowRedirect(t *testing.T) {
	for _, provider := range []string{"zai_cn", "zai", "bigmodel"} {
		t.Run(provider, func(t *testing.T) {
			app := testApp(t)
			var exchanged M
			testInstallClient(app, "", func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/v1/oauth/token":
					if err := json.NewDecoder(r.Body).Decode(&exchanged); err != nil {
						return nil, err
					}
					return testAPIResponse(r, 200, js(M{"code": 0, "data": testOAuthReady(provider)})), nil
				case "/api/v1/zcode-plan/billing/balance":
					// A temporary verification failure must not discard a completed login.
					return testAPIResponse(r, 503, `{"code":503}`), nil
				default:
					return nil, fmt.Errorf("unexpected request: %s", r.URL.Path)
				}
			})
			flow := M{"provider": provider, "authorize_url": testOAuthURL(provider)}
			_, err := app.completeOAuth(context.Background(), flow, M{"code": "one-time-code", "state": "wrong-state"})
			if err == nil || exchanged != nil || len(app.rows("SELECT id FROM accounts")) != 0 {
				t.Fatal("mismatched state must not exchange or save credentials")
			}
			result, err := app.completeOAuth(context.Background(), flow, M{"code": "one-time-code", "state": "flow-state"})
			testMust(t, err)
			testEqual(t, exchanged["redirect_uri"], "https://zcode.z.ai/api/v1/oauth/cli/callback/"+provider)
			testEqual(t, exchanged["provider"], provider)
			testEqual(t, result["verified"], false)
			if app.account(str(result["id"])) == nil {
				t.Fatal("verification failure discarded the authorized account")
			}
		})
	}
}

func TestOAuthExpiryDuringPollWait(t *testing.T) {
	app := testApp(t)
	var requests atomic.Int32
	testInstallClient(app, "", func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return testAPIResponse(r, 400, `{"code":400}`), nil
	})
	token, err := encrypt("poll-token", app.secret)
	testMust(t, err)
	testMust(t, app.exec("INSERT INTO oauth_flows(flow_id,provider,poll_token_enc,created_at,expires_at,poll_interval_sec) VALUES(?,?,?,?,?,?)", "expired", "zai_cn", token, nowMS(), nowMS()+500, 1))
	app.watchOAuth("expired")
	testWait(t, func() bool {
		return app.row("SELECT status FROM oauth_flows WHERE flow_id='expired'")["status"] == "expired"
	})
	testEqual(t, requests.Load(), 0)
	testEqual(t, app.row("SELECT poll_token_enc FROM oauth_flows WHERE flow_id='expired'")["poll_token_enc"], "")
}
