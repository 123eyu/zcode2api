package account

import (
	"encoding/base64"
	"strings"
	"zcode2api/internal/server/shared"
)

func JwtPayload(s string) shared.M {
	p := strings.Split(s, ".")
	if len(p) < 2 {
		return nil
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(p[1], "="))
	if e != nil {
		return nil
	}
	v := shared.Parse(string(b), nil)
	m, _ := v.(map[string]any)
	return m
}

func SecondsMS(v any) any {
	if !shared.Truth(v) {
		return nil
	}
	return shared.Integer(v) * 1000
}

func BalanceData(r shared.M) shared.M { return BalancePayload(shared.Parse(r["last_balance"], nil)) }

func BalancePayload(value any) shared.M {
	d := shared.Obj(value)
	if x, ok := d["data"].(map[string]any); ok {
		return x
	}
	return d
}

func LiveBalances(data shared.M, now int64) []any {
	plans := shared.Arr(data["plans"])
	active := map[string]bool{}
	for _, v := range plans {
		p := shared.Obj(v)
		if shared.Str(p["status"]) == "active" && (!shared.Truth(p["ends_at"]) || shared.Integer(p["ends_at"])*1000 > now) {
			active[shared.Str(p["user_plan_id"])] = true
			active[shared.Str(p["plan_id"])] = true
		}
	}
	out := []any{}
	for _, v := range shared.Arr(data["balances"]) {
		b := shared.Obj(v)
		end := shared.Integer(shared.First(b["expires_at"], b["period_end"]))
		if len(plans) > 0 && shared.Truth(b["user_plan_id"]) && !active[shared.Str(b["user_plan_id"])] {
			continue
		}
		if end > 0 && end*1000 <= now {
			continue
		}
		out = append(out, b)
	}
	return out
}

func RemainingBalance(data shared.M, now int64) float64 {
	remaining := float64(0)
	for _, value := range LiveBalances(data, now) {
		remaining += shared.Num(shared.Obj(value)["remaining_units"])
	}
	return remaining
}
