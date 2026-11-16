package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"zcode2api/internal/server/shared"
	updater "zcode2api/internal/server/update"
)

type updateManager struct {
	once     sync.Once
	mu       sync.Mutex
	checkMu  sync.Mutex
	cache    updater.UpdateReleaseCache
	support  updater.UpdateSupport
	running  bool
	checking bool
}

func (a *App) updateDirectory() string {
	directory, _ := filepath.Abs(filepath.Join(a.data, "updates"))
	return directory
}

func (a *App) loadUpdater() {
	a.updates.once.Do(func() {
		a.updates.support = updater.DetectUpdateSupport(a.version)
		_ = updater.ReadUpdateJSON(filepath.Join(a.updateDirectory(), "release.json"), &a.updates.cache)
		cached := a.updates.cache.Release
		if cached.URL != "https://github.com/"+updater.UpdateRepository+"/releases/tag/"+cached.Tag {
			a.updates.cache = updater.UpdateReleaseCache{}
		}
		var state updater.UpdateState
		filename := filepath.Join(a.updateDirectory(), "state.json")
		if updater.ReadUpdateJSON(filename, &state) == nil && (state.Phase == "downloading" || state.Phase == "preparing") {
			state.Phase, state.Message, state.FinishedAt = "error", "服务已重启，上次更新已中断，可以重新检查更新", shared.NowMS()
			_ = updater.WriteUpdateJSON(filename, state)
		}
	})
}

func (a *App) updateStatus() shared.M {
	a.loadUpdater()
	a.updates.mu.Lock()
	cache, support := a.updates.cache, a.updates.support
	running, checking := a.updates.running, a.updates.checking
	a.updates.mu.Unlock()
	state := updater.UpdateState{Phase: "idle"}
	_ = updater.ReadUpdateJSON(filepath.Join(a.updateDirectory(), "state.json"), &state)
	if !running && updater.UpdateBusy(state.Phase) && state.StartedAt < shared.NowMS()-int64((20*time.Minute)/time.Millisecond) {
		state.Phase, state.Message, state.FinishedAt = "error", "更新任务已超时，请检查服务状态后重试", shared.NowMS()
		_ = updater.WriteUpdateJSON(filepath.Join(a.updateDirectory(), "state.json"), state)
	}
	available := updater.NewerUpdate(a.version, cache.Release.Tag)
	assetOK, assetReason := false, ""
	if cache.Release.Tag != "" {
		_, _, err := updater.UpdateAssets(cache.Release)
		assetOK = err == nil
		if err != nil {
			assetReason = err.Error()
		}
	}
	return shared.M{"current_version": a.version, "latest_version": cache.Release.Tag, "repository": updater.UpdateRepository,
		"repository_url": "https://github.com/" + updater.UpdateRepository, "release_url": cache.Release.URL,
		"release_notes": cache.Release.Body, "published_at": cache.Release.PublishedAt, "checked_at": cache.CheckedAt,
		"available": available, "supported": support.Supported, "reason": support.Reason, "deployment": support.Mode,
		"asset_ready": assetOK, "asset_reason": assetReason, "busy": running || updater.UpdateBusy(state.Phase), "checking": checking, "state": state}
}

func (a *App) updateClient(ctx context.Context) (*http.Client, func(), error) {
	ctx, release, err := a.pinOutboundProxy(ctx, nil)
	if err != nil {
		return nil, func() {}, fmt.Errorf("更新网络连接不可用：%w", err)
	}
	client, err := a.client(ctx)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return &http.Client{Transport: client.Transport, CheckRedirect: updater.UpdateRedirect}, release, nil
}

func (a *App) checkUpdate(parent context.Context) error {
	a.loadUpdater()
	if !a.updates.checkMu.TryLock() {
		return fmt.Errorf("正在检查更新，请稍候")
	}
	defer a.updates.checkMu.Unlock()
	a.updates.mu.Lock()
	cache := a.updates.cache
	if a.updates.running {
		a.updates.mu.Unlock()
		return fmt.Errorf("更新正在进行中")
	}
	if cache.CheckedAt > shared.NowMS()-10000 {
		a.updates.mu.Unlock()
		return nil
	}
	a.updates.checking = true
	a.updates.mu.Unlock()
	defer func() { a.updates.mu.Lock(); a.updates.checking = false; a.updates.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	client, release, err := a.updateClient(ctx)
	if err != nil {
		return err
	}
	defer release()
	cache, err = updater.FetchUpdateRelease(ctx, client, cache)
	if err != nil {
		return err
	}
	if err = updater.WriteUpdateJSON(filepath.Join(a.updateDirectory(), "release.json"), cache); err != nil {
		return fmt.Errorf("无法保存版本信息，请检查数据目录权限")
	}
	a.updates.mu.Lock()
	a.updates.cache = cache
	a.updates.mu.Unlock()
	return nil
}

func (a *App) startUpdate(version string) error {
	a.loadUpdater()
	a.updates.mu.Lock()
	defer a.updates.mu.Unlock()
	if a.updates.running || a.updates.checking {
		return fmt.Errorf("已有更新或版本检查正在进行")
	}
	var previous updater.UpdateState
	_ = updater.ReadUpdateJSON(filepath.Join(a.updateDirectory(), "state.json"), &previous)
	if updater.UpdateBusy(previous.Phase) {
		return fmt.Errorf("已有更新正在进行")
	}
	if !a.updates.support.Supported {
		return fmt.Errorf("%s", a.updates.support.Reason)
	}
	cache := a.updates.cache
	if version == "" || version != cache.Release.Tag || cache.CheckedAt < shared.NowMS()-int64((30*time.Minute)/time.Millisecond) {
		return fmt.Errorf("请先检查最新版本，再提交更新")
	}
	if !updater.NewerUpdate(a.version, version) {
		return fmt.Errorf("当前已是最新发行版本，不执行降级或覆盖本地修复版")
	}
	if _, _, err := updater.UpdateAssets(cache.Release); err != nil {
		return err
	}
	state := updater.UpdateState{Phase: "downloading", Version: version, Message: "正在下载发行包", StartedAt: shared.NowMS()}
	if err := updater.WriteUpdateJSON(filepath.Join(a.updateDirectory(), "state.json"), state); err != nil {
		return fmt.Errorf("无法创建更新任务，请检查数据目录权限")
	}
	a.updates.running = true
	a.background(func() { a.runUpdate(cache, state) })
	return nil
}

func (a *App) runUpdate(cache updater.UpdateReleaseCache, state updater.UpdateState) {
	defer func() { a.updates.mu.Lock(); a.updates.running = false; a.updates.mu.Unlock() }()
	stateFile := filepath.Join(a.updateDirectory(), "state.json")
	err := a.prepareUpdate(cache, &state)
	if err != nil {
		state.Phase, state.Message, state.FinishedAt = "error", err.Error(), shared.NowMS()
		_ = updater.WriteUpdateJSON(stateFile, state)
		a.log.Printf("更新失败: %v", err)
		a.audit("system", "update.failed", shared.M{"version": state.Version, "error": state.Message}, "")
	}
}

func (a *App) prepareUpdate(cache updater.UpdateReleaseCache, state *updater.UpdateState) error {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()
	client, release, err := a.updateClient(ctx)
	if err != nil {
		return err
	}
	defer release()
	fresh, err := updater.FetchUpdateRelease(ctx, client, cache)
	if err != nil {
		return err
	}
	if fresh.Release.Tag != state.Version {
		return fmt.Errorf("最新版本已变化，请重新检查后更新")
	}
	asset, checksumAsset, err := updater.UpdateAssets(fresh.Release)
	if err != nil {
		return err
	}
	response, err := updater.UpdateGET(ctx, client, checksumAsset.URL, "")
	if err != nil {
		return err
	}
	checksums, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	response.Body.Close()
	if err != nil || len(checksums) > 1<<20 {
		return fmt.Errorf("无法读取发行包校验文件")
	}
	expected := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./") == asset.Name {
			if expected != "" {
				return fmt.Errorf("发行包校验记录重复")
			}
			expected = strings.ToLower(fields[0])
		}
	}
	if decoded, err := hex.DecodeString(expected); err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("发行包缺少有效的 SHA256 校验值")
	}
	if asset.Digest != "" && asset.Digest != "sha256:"+expected {
		return fmt.Errorf("GitHub 摘要与 SHA256SUMS 不一致")
	}
	job, err := os.MkdirTemp(a.updateDirectory(), "job-*")
	if err != nil {
		return fmt.Errorf("无法创建更新目录")
	}
	archive, err := os.OpenFile(filepath.Join(job, "release.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	response, err = updater.UpdateGET(ctx, client, asset.URL, "")
	if err != nil {
		archive.Close()
		return err
	}
	state.Total = asset.Size
	lastProgress := time.Time{}
	progress := &updater.UpdateProgress{OnProgress: func(count int64) {
		state.Downloaded = count
		if time.Since(lastProgress) >= 500*time.Millisecond {
			lastProgress = time.Now()
			_ = updater.WriteUpdateJSON(filepath.Join(a.updateDirectory(), "state.json"), state)
		}
	}}
	digest := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(archive, digest, progress), io.LimitReader(response.Body, updater.UpdateArchiveLimit+1))
	response.Body.Close()
	closeErr := archive.Close()
	if copyErr != nil || closeErr != nil || size != asset.Size || size > updater.UpdateArchiveLimit {
		return fmt.Errorf("发行包下载不完整，请检查服务器网络后重试")
	}
	if hex.EncodeToString(digest.Sum(nil)) != expected {
		return fmt.Errorf("发行包 SHA256 校验失败，未替换当前程序")
	}
	state.Phase, state.Message = "preparing", "校验通过，正在准备备份"
	if err = updater.WriteUpdateJSON(filepath.Join(a.updateDirectory(), "state.json"), state); err != nil {
		return err
	}
	binary := filepath.Join(job, "new-binary")
	if err = updater.ExtractUpdateBinary(archive.Name(), asset.Name, binary); err != nil {
		return err
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	command := exec.CommandContext(probeCtx, binary, "--version")
	output, err := command.Output()
	probeCancel()
	if err != nil || strings.TrimSpace(string(output)) != "zcode2api "+state.Version {
		return fmt.Errorf("发行包内程序版本不匹配或无法运行")
	}
	return a.launchUpdateHelper(ctx, job, binary, state)
}

func (a *App) launchUpdateHelper(ctx context.Context, job, binary string, state *updater.UpdateState) error {
	support := updater.DetectUpdateSupport(a.version)
	if !support.Supported {
		return fmt.Errorf("%s", support.Reason)
	}
	data, err := filepath.Abs(a.data)
	if err != nil {
		return err
	}
	plan := updater.UpdatePlan{Service: support.Service, Binary: support.Binary, Candidate: binary,
		Backup: filepath.Join(job, "previous-binary"), Data: data, StateFile: filepath.Join(a.updateDirectory(), "state.json"),
		Version: state.Version, PreviousVersion: a.version, CreatedAt: shared.NowMS(), PID: os.Getpid(), WebPort: a.webPort, GatewayPort: a.gatewayPort}
	if err = updater.CopyUpdateFile(support.Binary, plan.Backup, 0700); err != nil {
		return fmt.Errorf("无法备份当前程序：%w", err)
	}
	plan.OldSHA256, err = updater.UpdateFileHash(plan.Backup)
	if err != nil {
		return err
	}
	plan.NewSHA256, err = updater.UpdateFileHash(binary)
	if err != nil {
		return err
	}
	// SQLite creates a consistent backup while existing requests can finish.
	if _, err = a.db.ExecContext(ctx, "VACUUM INTO ?", filepath.Join(job, "database-before.db")); err != nil {
		return fmt.Errorf("数据库备份失败，未替换当前程序")
	}
	if err = updater.CopyUpdateFile(filepath.Join(a.root, "config.json"), filepath.Join(job, "config-before.json"), 0600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("配置文件备份失败，未替换当前程序")
	}
	helper := filepath.Join(job, "update-helper")
	if err = updater.CopyUpdateFile(plan.Backup, helper, 0700); err != nil {
		return err
	}
	planFile := filepath.Join(job, "plan.json")
	if err = updater.WriteUpdateJSON(planFile, plan); err != nil {
		return err
	}
	state.Phase, state.Message = "applying", "备份完成，即将重启服务"
	if err = updater.WriteUpdateJSON(plan.StateFile, state); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, support.SystemdRun, "--quiet", "--collect", "--unit=zcode2api-update-"+shared.Random(6),
		"--property=Type=exec", "--property=RuntimeMaxSec=10min", "--property=TimeoutStopSec=15s", "--", helper, "--apply-update", planFile)
	if err = command.Run(); err != nil {
		return fmt.Errorf("无法启动独立更新进程，当前程序未替换")
	}
	a.audit("system", "update.started", shared.M{"version": state.Version, "backup": job}, "")
	return nil
}
