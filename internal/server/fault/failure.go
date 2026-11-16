package fault

import (
	"time"
	"zcode2api/internal/server/shared"
)

type Failure struct {
	Status               int
	Code, Kind, Message  string
	Retryable, Committed bool
	ProxyFailover        bool
	VerifyCode           string
	Detail               shared.M
	RetryAfter           time.Duration
	Attempts             int
	CooldownRecorded     bool `json:"-"`
	LocalOnly            bool `json:"-"` // Admission refused without new upstream evidence.
}

func (e *Failure) Error() string { return e.Message }

func Invalid(s string) error {
	return &Failure{Status: 400, Code: "unsupported_request", Kind: "invalid", Message: s}
}
