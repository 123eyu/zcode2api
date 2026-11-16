//go:build !windows

package process

import (
	"os"
	"strings"
)

func OSVersion() string {
	if b, e := os.ReadFile("/proc/sys/kernel/osrelease"); e == nil {
		return strings.TrimSpace(string(b))
	}
	return "unknown"
}
