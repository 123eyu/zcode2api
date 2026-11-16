package fault

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"zcode2api/internal/server/shared"
)

type ApiResult struct {
	Status  int
	JSON    shared.M
	Header  http.Header
	OK      bool
	Failure *Failure `json:"-"`
}

func ApiFailure(r ApiResult, label string) error {
	failure := ApiResponseFailure(r)
	failure.Message = label + ": " + ConciseFailureMessage(failure)
	return failure
}

func ApiResponseFailure(r ApiResult) *Failure {
	if r.Failure != nil {
		failure := *r.Failure
		return &failure
	}
	return Classify(ProviderFailure(r.JSON, r.Status, r.Header.Get("Retry-After")))
}

func ProviderFailure(v shared.M, status int, retry string) *Failure {
	e := shared.Obj(v["error"])
	f := &Failure{Status: status, RetryAfter: RetryAfter(retry, time.Now())}
	for _, value := range []any{e["code"], v["code"], e["type"], "upstream_http_error"} {
		if code := strings.TrimSpace(shared.Str(value)); code != "" {
			f.Code = code
			break
		}
	}
	for _, value := range []any{e["message"], e["msg"], v["msg"], v["message"], fmt.Sprintf("上游 HTTP %d", status)} {
		if message := strings.TrimSpace(shared.Str(value)); message != "" {
			f.Message = message
			break
		}
	}
	f.Detail = FailureDetail(f)
	return f
}

func UpstreamHTTPFailure(res *http.Response, body []byte) *Failure {
	v := shared.Obj(shared.Parse(string(body), nil))
	f := ProviderFailure(v, res.StatusCode, res.Header.Get("Retry-After"))
	f.Detail["body"] = FailureBody(string(body))
	f.Detail["http_status"] = res.StatusCode
	f.Detail["response_headers"] = FailureResponseHeaders(res.Header)
	if res.StatusCode == http.StatusProxyAuthRequired {
		f.Kind, f.Code, f.Message, f.Retryable = "network", "proxy_auth_failed", "代理鉴权失败", true
		return f
	}
	if len(v) > 0 {
		return f
	}
	page := strings.ToLower(string(body))
	switch {
	case (res.StatusCode == 403 || res.StatusCode == 405 || res.StatusCode == 503) && strings.Contains(page, "errors.aliyun.com/") && strings.Contains(page, "block_message"):
		f.Kind, f.Code = "risk", "upstream_waf_blocked"
		f.Message = "上游安全网关拦截"
	case riskBlockMessage(page):
		f.Kind, f.Code, f.Message = "account_risk", "upstream_risk_blocked", "账号风控拒绝"
	case strings.TrimSpace(string(body)) == "":
		f.Message += "：上游返回空响应体"
	default:
		f.Message += "：上游返回非 JSON 错误响应"
	}
	return f
}
