package captcha

import (
	"time"
	"zcode2api/internal/server/fault"
	"zcode2api/internal/server/shared"
)

// Aliyun rejects V3 tickets over 90 seconds old (F019).
const (
	MaxTicketTTL = 90 * time.Second
	// Leave time for the business request to reach the verifier.
	CaptchaTicketSafetyMargin = 5 * time.Second
)

// An explicit 0 disables prefetch; an absent setting keeps the default.
func CaptchaTicketTTL(options shared.M) time.Duration {
	raw, present := options["ticket_ttl_ms"]
	if !present {
		return 75 * time.Second
	}
	value := shared.Num(raw)
	if value <= 0 {
		return 0
	}
	return min(max(time.Duration(value)*time.Millisecond, 30*time.Second), MaxTicketTTL)
}

const (
	CaptchaPurposeInference      = "inference"
	CaptchaPurposeClaim          = "claim"
	CaptchaPurposeBatchClaim     = "batch_claim"
	CaptchaPurposeScheduledClaim = "scheduled_claim"
	CaptchaPrefetchDemandWindow  = 30 * time.Second
	MaxCaptchaPools              = 128
)

func CaptchaScopeChanged() *fault.Failure {
	return &fault.Failure{Kind: "server", Code: "captcha_config_changed", Message: "验证账号、用途或场景已更新，请重新验证", Retryable: true}
}

func CaptchaCredentialVersion(account shared.M, token string) string {
	if token == "" {
		return ""
	}
	return shared.Hash(shared.JSON([]any{account["id"], account["device_mid"], account["proxy_url"], token}))
}
