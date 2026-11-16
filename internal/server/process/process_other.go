//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

func HideChildWindow(command *exec.Cmd) {}

func PrepareBrowserProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func KillBrowserProcess(command *exec.Cmd) { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
