//go:build !windows

package update

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func windowsUpdateAvailable() bool { return false }

func SecureUpdateJob(string) error { return nil }

func lockDockerUpdater(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func replaceUpdateState(rename func() error) error { return rename() }

func StartWindowsUpdater(context.Context, string, string) error {
	return fmt.Errorf("仅 Windows 支持此更新器")
}

func applyWindowsUpdate(context.Context, string, UpdatePlan, UpdateState) error {
	return fmt.Errorf("仅 Windows 支持此更新器")
}
