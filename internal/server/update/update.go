package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"zcode2api/internal/server/shared"
)

const UpdateRepository = "7sen1ot/zcode2api"

const UpdateArchiveLimit = 128 << 20

const updateBinaryLimit = 128 << 20

var updateVersionPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+][0-9A-Za-z.-]+)?$`)

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type githubRelease struct {
	Tag         string         `json:"tag_name"`
	URL         string         `json:"html_url"`
	Body        string         `json:"body"`
	PublishedAt string         `json:"published_at"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	Assets      []releaseAsset `json:"assets"`
}

type UpdateReleaseCache struct {
	Release   githubRelease `json:"release"`
	CheckedAt int64         `json:"checked_at"`
	ETag      string        `json:"etag"`
}

type UpdateState struct {
	Phase      string `json:"phase"`
	Version    string `json:"version,omitempty"`
	Message    string `json:"message,omitempty"`
	Downloaded int64  `json:"downloaded,omitempty"`
	Total      int64  `json:"total,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

func ReadUpdateJSON(filename string, target any) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(target)
}

func WriteUpdateJSON(filename string, value any) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = json.NewEncoder(file).Encode(value); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filename)
}

// Compare release numbers so a local hotfix never gets replaced by the same
// published base version. Only a newer stable release is offered.
func updateVersionNumbers(version string) ([3]uint64, bool) {
	var numbers [3]uint64
	parts := updateVersionPattern.FindStringSubmatch(version)
	if len(parts) != 4 {
		return numbers, false
	}
	for index := range numbers {
		n, err := strconv.ParseUint(parts[index+1], 10, 64)
		if err != nil {
			return numbers, false
		}
		numbers[index] = n
	}
	return numbers, true
}

func NewerUpdate(current, latest string) bool {
	old, oldOK := updateVersionNumbers(current)
	next, nextOK := updateVersionNumbers(latest)
	if !oldOK || !nextOK {
		return false
	}
	for index := range old {
		if next[index] != old[index] {
			return next[index] > old[index]
		}
	}
	return false
}

func UpdateAssets(release githubRelease) (releaseAsset, releaseAsset, error) {
	name := fmt.Sprintf("zcode2api_%s_%s_%s.tar.gz", release.Tag, runtime.GOOS, runtime.GOARCH)
	var binary, checksums releaseAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case name:
			if binary.Name != "" {
				return binary, checksums, fmt.Errorf("发行包名称重复")
			}
			binary = asset
		case "SHA256SUMS":
			if checksums.Name != "" {
				return binary, checksums, fmt.Errorf("校验文件名称重复")
			}
			checksums = asset
		}
	}
	if binary.Name == "" || checksums.Name == "" || binary.Size <= 0 || binary.Size > UpdateArchiveLimit || checksums.Size > 1<<20 {
		return binary, checksums, fmt.Errorf("该版本缺少当前平台发行包或 SHA256SUMS")
	}
	for _, asset := range []releaseAsset{binary, checksums} {
		expected := "https://github.com/" + UpdateRepository + "/releases/download/" + release.Tag + "/" + asset.Name
		if asset.URL != expected {
			return binary, checksums, fmt.Errorf("发行包下载地址与官方仓库不一致")
		}
	}
	return binary, checksums, nil
}

func UpdateBusy(phase string) bool {
	return phase == "downloading" || phase == "preparing" || phase == "applying" || phase == "restarting"
}

func UpdateRedirect(request *http.Request, via []*http.Request) error {
	host := request.URL.Hostname()
	if len(via) > 5 || request.URL.Scheme != "https" || request.URL.User != nil || request.URL.Port() != "" ||
		(host != "api.github.com" && host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com") {
		return fmt.Errorf("更新下载重定向地址无效")
	}
	return nil
}

func UpdateGET(ctx context.Context, client *http.Client, address, etag string) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || (u.Host != "api.github.com" && u.Host != "github.com") {
		return nil, fmt.Errorf("更新地址无效")
	}
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "zcode2api-updater")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("无法连接 GitHub，请检查服务器网络或出站代理")
	}
	if response.StatusCode != 200 && response.StatusCode != 304 {
		response.Body.Close()
		switch response.StatusCode {
		case 403, 429:
			return nil, fmt.Errorf("GitHub 请求频率受限，请稍后重试")
		case 404:
			return nil, fmt.Errorf("仓库尚未发布正式版本或发行文件不存在")
		default:
			return nil, fmt.Errorf("GitHub 返回 HTTP %d", response.StatusCode)
		}
	}
	return response, nil
}

func FetchUpdateRelease(ctx context.Context, client *http.Client, cache UpdateReleaseCache) (UpdateReleaseCache, error) {
	response, err := UpdateGET(ctx, client, "https://api.github.com/repos/"+UpdateRepository+"/releases/latest", cache.ETag)
	if err != nil {
		return cache, err
	}
	defer response.Body.Close()
	if response.StatusCode == 304 {
		if cache.Release.Tag == "" {
			return cache, fmt.Errorf("GitHub 版本缓存无效，请重新检查")
		}
		cache.CheckedAt = shared.NowMS()
		return cache, nil
	}
	var release githubRelease
	if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release); err != nil {
		return cache, fmt.Errorf("GitHub 版本信息格式无效")
	}
	if _, valid := updateVersionNumbers(release.Tag); !valid || strings.ContainsAny(release.Tag, "-+") || release.Draft || release.Prerelease ||
		release.URL != "https://github.com/"+UpdateRepository+"/releases/tag/"+release.Tag {
		return cache, fmt.Errorf("GitHub 未返回有效的正式版本")
	}
	return UpdateReleaseCache{Release: release, CheckedAt: shared.NowMS(), ETag: response.Header.Get("ETag")}, nil
}

type UpdateProgress struct {
	count      int64
	OnProgress func(int64) `json:"-"`
}

func (progress *UpdateProgress) Write(data []byte) (int, error) {
	progress.count += int64(len(data))
	progress.OnProgress(progress.count)
	return len(data), nil
}

func ExtractUpdateBinary(archiveName, assetName, output string) error {
	archive, err := os.Open(archiveName)
	if err != nil {
		return err
	}
	defer archive.Close()
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("发行包压缩格式无效")
	}
	defer compressed.Close()
	reader := tar.NewReader(io.LimitReader(compressed, 256<<20))
	expected := strings.TrimSuffix(assetName, ".tar.gz") + "/zcode2api"
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("发行包内容无效")
		}
		if header.Name != expected {
			continue
		}
		if found || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > updateBinaryLimit {
			return fmt.Errorf("发行包内的可执行文件无效或重复")
		}
		found = true
		file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return err
		}
		n, err := io.Copy(file, reader)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil || closeErr != nil || n != header.Size {
			return fmt.Errorf("发行包内程序解压不完整")
		}
	}
	if !found {
		return fmt.Errorf("发行包中未找到当前平台程序")
	}
	return nil
}
