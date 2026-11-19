package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"zcode2api/internal/server/shared"

	"golang.org/x/sys/windows"
)

func windowsUpdateAvailable() bool {
	var session uint32
	return windows.ProcessIdToSessionId(uint32(os.Getpid()), &session) == nil && session != 0
}

func SecureUpdateJob(job string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(job, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func lockDockerUpdater(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func replaceUpdateState(rename func() error) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := rename()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func detachedWindowsCommand(binary string, args ...string) *exec.Cmd {
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS, HideWindow: true}
	return cmd
}

func StartWindowsUpdater(ctx context.Context, helper, planFile string) error {
	log, err := os.OpenFile(filepath.Join(filepath.Dir(planFile), "helper.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := detachedWindowsCommand(helper, "--apply-update", planFile)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return err
	}
	exit := make(chan error, 1)
	go func() { exit <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ready := filepath.Join(filepath.Dir(planFile), "ready")
	for ctx.Err() == nil {
		select {
		case <-exit:
			return fmt.Errorf("独立更新器无法启动，请查看 %s", log.Name())
		default:
		}
		if info, err := os.Stat(ready); err == nil && info.Mode().IsRegular() {
			return nil
		}
		if err := shared.Pause(ctx, 100*time.Millisecond); err != nil {
			break
		}
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("独立更新器未就绪，请查看 %s", log.Name())
}

func waitWindowsProcess(ctx context.Context, process windows.Handle) error {
	for ctx.Err() == nil {
		status, err := windows.WaitForSingleObject(process, 250)
		if err != nil {
			return err
		}
		if status == windows.WAIT_OBJECT_0 {
			return nil
		}
		if status != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("等待原进程退出失败")
		}
	}
	return ctx.Err()
}

func startWindowsVersion(plan UpdatePlan) (*exec.Cmd, <-chan struct{}, error) {
	stdout, err := os.OpenFile(filepath.Join(plan.Data, "service.stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(plan.Data, "service.stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, err
	}
	defer stderr.Close()
	cmd := detachedWindowsCommand(plan.Binary, plan.Args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = plan.Directory, stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return cmd, done, nil
}

func waitWindowsHealthy(ctx context.Context, plan UpdatePlan, version string, pid int, done <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := fmt.Errorf("新进程未能在一分钟内完成启动")
	for ctx.Err() == nil {
		var running struct {
			PID         int `json:"pid"`
			WebPort     int `json:"web_port"`
			GatewayPort int `json:"gateway_port"`
		}
		if ReadUpdateJSON(filepath.Join(plan.Data, "ports.json"), &running) == nil && running.PID == pid &&
			running.WebPort > 0 && running.WebPort <= 65535 && running.GatewayPort > 0 && running.GatewayPort <= 65535 {
			plan.WebPort, plan.GatewayPort = running.WebPort, running.GatewayPort
			err = waitUpdateHealthy(ctx, plan, version)
			break
		}
		if shared.Pause(ctx, 250*time.Millisecond) != nil {
			break
		}
	}
	select {
	case <-done:
		return fmt.Errorf("程序启动后退出，请查看 service.stderr.log")
	default:
		return err
	}
}

func applyWindowsUpdate(ctx context.Context, filename string, plan UpdatePlan, state UpdateState) (resultErr error) {
	var child *exec.Cmd
	var childDone <-chan struct{}
	stopped, replaced := false, false
	defer func() {
		if resultErr != nil {
			state.Phase, state.Message = "error", resultErr.Error()
			if stopped {
				recovery, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				err := restoreWindowsVersion(recovery, plan, replaced, child, childDone)
				if err == nil {
					state.Phase, state.Message = "rolled_back", "更新失败，已恢复原版本："+resultErr.Error()
				} else {
					state.Message = "更新失败，自动恢复未完成：" + err.Error() + "；备份位于 " + filepath.Dir(filename)
				}
			}
		}
		state.FinishedAt = shared.NowMS()
		if err := WriteUpdateJSON(plan.StateFile, state); resultErr == nil {
			resultErr = err
		}
	}()
	if err := validateUpdatePlan(filename, plan); err != nil {
		return err
	}
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(plan.PID))
	if err != nil {
		return fmt.Errorf("无法确认当前网关进程，请重新检查后更新")
	}
	defer windows.CloseHandle(parent)
	name := make([]uint16, 32768)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(parent, 0, &name[0], &size); err != nil {
		return err
	}
	original, err := os.Stat(windows.UTF16ToString(name[:size]))
	target, targetErr := os.Stat(plan.Binary)
	if err != nil || targetErr != nil || !os.SameFile(original, target) {
		return fmt.Errorf("当前网关进程与更新计划不一致")
	}
	ready := filepath.Join(filepath.Dir(filename), "ready")
	if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, 60*time.Second)
	err = waitWindowsProcess(wait, parent)
	cancel()
	if err != nil {
		return fmt.Errorf("等待当前网关正常退出超时")
	}
	stopped = true
	if err := replaceWindowsBinary(ctx, plan.Candidate, plan.Binary); err != nil {
		return err
	}
	replaced = true
	state.Phase, state.Message = "restarting", "正在启动新版本"
	if err := WriteUpdateJSON(plan.StateFile, state); err != nil {
		return err
	}
	child, childDone, err = startWindowsVersion(plan)
	if err != nil {
		return err
	}
	if err := waitWindowsHealthy(ctx, plan, plan.Version, child.Process.Pid, childDone); err != nil {
		return err
	}
	state.Phase, state.Message = "completed", "更新完成"
	return nil
}

func replaceWindowsBinary(ctx context.Context, source, target string) error {
	deadline := time.Now().Add(8 * time.Second)
	for {
		err := replaceUpdateBinary(source, target)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return err
		}
		if time.Now().After(deadline) || shared.Pause(ctx, 500*time.Millisecond) != nil {
			return err
		}
	}
}

func restoreWindowsVersion(ctx context.Context, plan UpdatePlan, replaced bool, child *exec.Cmd, done <-chan struct{}) error {
	if child != nil {
		if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if replaced {
		if err := replaceWindowsBinary(ctx, plan.Backup, plan.Binary); err != nil {
			return err
		}
	}
	old, oldDone, err := startWindowsVersion(plan)
	if err != nil {
		return err
	}
	return waitWindowsHealthy(ctx, plan, plan.PreviousVersion, old.Process.Pid, oldDone)
}
