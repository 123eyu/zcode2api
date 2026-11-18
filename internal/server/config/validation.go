package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	captchasdk "zcode2api/internal/server/captcha"
	wire "zcode2api/internal/server/protocol"
	routing "zcode2api/internal/server/proxy"
	"zcode2api/internal/server/shared"
)

var VersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func ValidateSettings(b shared.M) error {
	if value, present := b["zcode_template_mode"]; present {
		mode, ok := value.(string)
		if !ok || (mode != wire.TemplateMain && mode != wire.TemplateSubagent) {
			return fmt.Errorf("模板模式仅支持 main 或 subagent")
		}
	}
	for _, key := range []string{"zcode_subagent_insert_enabled", "zcode_subagent_harness_enabled", "zcode_subagent_files_enabled", "zcode_subagent_comments_enabled"} {
		if value, present := b[key]; present {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s 必须为布尔值", key)
			}
		}
	}
	if value, present := b["zcode_subagent_template"]; present {
		custom, ok := value.(string)
		if !ok || len(custom) > wire.MaxCustomTemplateBytes {
			return fmt.Errorf("自定义模板必须为不超过 256 KiB 的文本")
		}
	}
	if value, present := b["zcode_version_check_interval_sec"]; present {
		if !shared.IntegerInRange(value, minZCodeVersionCheckIntervalSec, maxZCodeVersionCheckIntervalSec) {
			return fmt.Errorf("ZCode 版本检查间隔须为 1 分钟到 365 天之间的整数秒数")
		}
	}
	if value, present := b["zcode_version_auto_update"]; present {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("zcode_version_auto_update 必须为布尔值")
		}
	}
	if value, present := b["outbound_proxy"]; present {
		if err := routing.ValidateOutboundProxyFields(value); err != nil {
			return err
		}
	}
	if v, ok := b["app_version"]; ok && !VersionPattern.MatchString(shared.Str(v)) {
		return fmt.Errorf("额度与领卡 API 版本须为三段数字，例如 3.16.2")
	}
	if v, ok := b["inference_driver"]; ok && shared.Str(v) != "http" {
		return fmt.Errorf("网关仅支持 HTTP 直连")
	}
	if v, ok := b["http"]; ok {
		h, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("http 必须为配置对象")
		}
		for _, k := range []string{"first_token_timeout_ms", "idle_timeout_ms", "total_timeout_ms"} {
			if v, ok := h[k]; ok {
				_, isFloat := v.(float64)
				_, isJSONNumber := v.(json.Number)
				n, isNumber := shared.Num(v), isFloat || isJSONNumber
				minimum := float64(1)
				if k == "total_timeout_ms" {
					minimum = 0
				}
				if !isNumber || n < minimum {
					return fmt.Errorf("%s 超时设置无效", k)
				}
			}
		}
		if v, ok := h["client_version"]; ok && !VersionPattern.MatchString(shared.Str(v)) {
			return fmt.Errorf("推理 API 版本须为三段数字，例如 %s", DefaultHTTPClientVersion)
		}
		if v, ok := h["captcha"]; ok {
			if err := captchasdk.ValidateCaptchaOptions(v); err != nil {
				return err
			}
		}
	}
	for _, k := range []string{"poll_interval_sec", "poll_concurrency"} {
		if v, ok := b[k]; ok && shared.Num(v) < 1 {
			return fmt.Errorf("%s 必须大于等于 1", k)
		}
	}
	if enabled, present := b["daily_claim_enabled"]; present {
		if _, ok := enabled.(bool); !ok {
			return fmt.Errorf("daily_claim_enabled 必须为布尔值")
		}
	}
	if v, present := b["daily_claim_time"]; present && !validDailyClaimTime(shared.Str(v)) {
		return fmt.Errorf("daily_claim_time 须为 00:00–23:59 的 HH:MM 时间")
	}
	if v, present := b["conversation_affinity_minutes"]; present && (shared.Num(v) < 0 || shared.Num(v) > 10080) {
		return fmt.Errorf("conversation_affinity_minutes 须为 0 到 10080 的分钟数")
	}
	if v, ok := b["default_reasoning_level"]; ok && !ValidLevel(shared.Str(v)) {
		return fmt.Errorf("思考档位须为 low/high/max")
	}
	for _, k := range []string{"aliases", "retry"} {
		if v, ok := b[k]; ok {
			if _, ok := v.(map[string]any); !ok {
				return fmt.Errorf("%s 必须为配置对象", k)
			}
		}
	}
	return nil
}

func ValidLevel(s string) bool { return s == "low" || s == "high" || s == "max" }

func Effort(b shared.M) string {
	raw := shared.First(b["reasoning_effort"], b["thinking_level"], b["reasoningLevel"], shared.Obj(b["output_config"])["effort"])
	if raw == nil {
		if s, ok := b["reasoning"].(string); ok {
			raw = s
		} else {
			raw = shared.Obj(b["reasoning"])["effort"]
		}
	}
	if raw == nil {
		if s, ok := b["thinking"].(string); ok {
			raw = s
		} else {
			t := shared.Obj(b["thinking"])
			switch shared.Str(t["type"]) {
			case "disabled":
				return "low"
			case "enabled", "adaptive":
				n := shared.Num(t["budget_tokens"])
				if n >= 24000 {
					return "max"
				}
				if n >= 8000 || n == 0 {
					return "high"
				}
				return "low"
			}
		}
	}
	switch shared.Lower(raw) {
	case "low", "minimal", "off", "disabled", "none":
		return "low"
	case "max", "xhigh":
		return "max"
	case "high", "medium", "default", "enabled":
		return "high"
	}
	return ""
}
