package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"zcode2api/internal/server/shared"
)

const DefaultDailyClaimTime = "19:02"

func DailyClaimClock(value string) (int, int) {
	hh, mm, found := strings.Cut(strings.TrimSpace(value), ":")
	h, e1 := strconv.Atoi(hh)
	m, e2 := strconv.Atoi(mm)
	if !found || e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 19, 2
	}
	return h, m
}

func validDailyClaimTime(value string) bool {
	h, m := DailyClaimClock(value)
	return fmt.Sprintf("%02d:%02d", h, m) == strings.TrimSpace(value)
}

const (
	defaultZCodeVersionCheckIntervalSec = 5 * 24 * 60 * 60
	minZCodeVersionCheckIntervalSec     = 60
	maxZCodeVersionCheckIntervalSec     = 365 * 24 * 60 * 60
)

func ZcodeVersionInterval(cfg shared.M) time.Duration {
	seconds := shared.Integer(cfg["zcode_version_check_interval_sec"])
	if seconds < minZCodeVersionCheckIntervalSec || seconds > maxZCodeVersionCheckIntervalSec {
		seconds = defaultZCodeVersionCheckIntervalSec
	}
	return time.Duration(seconds) * time.Second
}
