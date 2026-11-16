package process

import (
	"os/exec"
	"strconv"
	"syscall"
)

func HideChildWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func PrepareBrowserProcess(command *exec.Cmd) { HideChildWindow(command) }

func KillBrowserProcess(command *exec.Cmd) {
	kill := exec.Command("taskkill", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
	HideChildWindow(kill)
	_ = kill.Run()
}
