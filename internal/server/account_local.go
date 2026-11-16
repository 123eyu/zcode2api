package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

const clientKeyringService = "ZCode Credential Encryption"

func clientCredentialsPath(requested string) (string, error) {
	home, _ := os.UserHomeDir()
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if strings.HasPrefix(requested, "~/") || strings.HasPrefix(requested, `~\`) {
			if home == "" {
				return "", fmt.Errorf("无法确定当前用户目录，请填写完整的绝对路径")
			}
			requested = filepath.Join(home, requested[2:])
		}
		if !filepath.IsAbs(requested) {
			return "", fmt.Errorf("请填写运行服务的机器上的绝对路径")
		}
		if info, err := os.Stat(requested); err == nil && info.IsDir() {
			requested = filepath.Join(requested, "credentials.json")
		}
		return filepath.Clean(requested), nil
	}
	// Match the client's data directory precedence, including relocated installs.
	roots := []string{strings.TrimSpace(os.Getenv("HOME")), home}
	if root := strings.TrimSpace(os.Getenv("ZCODE_DATA_BASE_DIR")); root != "" {
		roots = []string{root}
	}
	searched := []string{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		path, err := filepath.Abs(filepath.Join(root, ".zcode", "v2", "credentials.json"))
		if err != nil {
			return "", fmt.Errorf("客户端数据目录无效")
		}
		if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
			return path, nil
		}
		if len(searched) == 0 || searched[len(searched)-1] != path {
			searched = append(searched, path)
		}
	}
	if len(searched) == 0 {
		return "", fmt.Errorf("无法确定当前用户目录，请指定客户端凭据路径")
	}
	return "", fmt.Errorf("未找到客户端凭据（%s）。请先在运行服务的机器登录 ZCode，或指定凭据路径", strings.Join(searched, "；"))
}

func readClientCredentialFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("路径不是可读取的文件")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, fmt.Errorf("文件过大")
	}
	return data, err
}

func clientManagedSecret(path string) (string, error) {
	secret, file := os.Getenv("ZCODE_CREDENTIAL_SECRET"), os.Getenv("ZCODE_CREDENTIAL_SECRET_FILE")
	if secret != "" && file != "" {
		return "", fmt.Errorf("ZCODE_CREDENTIAL_SECRET 与 ZCODE_CREDENTIAL_SECRET_FILE 不能同时设置")
	}
	if secret != "" {
		return secret, nil
	}
	if file == "" {
		file = filepath.Join(filepath.Dir(path), "credential-secret")
		info, err := os.Stat(file)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return "", fmt.Errorf("无法读取客户端密钥文件，或其权限不是仅当前用户可访问")
		}
	}
	data, err := readClientCredentialFile(file, 4096)
	if err != nil || len(data) < 32 {
		return "", fmt.Errorf("客户端密钥文件不可读取或长度无效，需要 32–4096 字节")
	}
	// Whitespace is part of the client's key material; do not trim file contents.
	return string(data), nil
}

type clientCredentialCipher struct {
	ctx  context.Context
	path string
	keys map[string][]byte
}

func (c *clientCredentialCipher) decrypt(value string) (string, error) {
	if strings.HasPrefix(value, "enc:v1:") {
		secret, err := clientManagedSecret(c.path)
		if err != nil {
			return "", err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法确定客户端旧版凭据的用户目录")
		}
		username := "unknown"
		if current, err := user.Current(); err == nil {
			username = current.Username
			if i := strings.LastIndex(username, `\`); i >= 0 {
				username = username[i+1:]
			}
		}
		platform := runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
		legacy := "zcode-credential-fallback:" + platform + ":" + home + ":" + username
		for _, candidate := range []string{secret, strings.TrimSpace(secret), legacy} {
			if candidate == "" {
				continue
			}
			if plain, err := decrypt(value, candidate); err == nil {
				return plain, nil
			}
		}
		return "", fmt.Errorf("旧版客户端凭据无法解密，请使用同一系统用户运行服务，并同步客户端的自定义密钥设置")
	}
	if !strings.HasPrefix(value, "enc:") {
		return value, nil
	}
	if !strings.HasPrefix(value, "enc:v2:") {
		return "", fmt.Errorf("不支持此客户端凭据版本，请使用账号授权")
	}
	parts := strings.Split(strings.TrimPrefix(value, "enc:v2:"), ":")
	if len(parts) != 3 || (parts[0] != "os" && parts[0] != "env") || len(parts[1]) != 24 {
		return "", fmt.Errorf("客户端凭据格式损坏，请在 ZCode 重新登录")
	}
	key := c.keys[parts[0]]
	if key == nil {
		if parts[0] == "os" {
			encoded, err := readClientKeyring(c.ctx, c.path)
			if err != nil {
				return "", err
			}
			key, err = base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || len(key) != 32 {
				return "", fmt.Errorf("系统凭据库中的客户端密钥无效，请在 ZCode 重新登录")
			}
		} else {
			secret, err := clientManagedSecret(c.path)
			if err != nil {
				return "", err
			}
			if secret == "" {
				return "", fmt.Errorf("客户端使用自定义密钥，请为服务配置相同的 ZCODE_CREDENTIAL_SECRET 或 ZCODE_CREDENTIAL_SECRET_FILE")
			}
			digest := sha256.Sum256([]byte(secret))
			key = digest[:]
		}
		c.keys[parts[0]] = key
	}
	if hash(string(key))[:24] != parts[1] {
		return "", fmt.Errorf("客户端凭据与密钥不匹配，请使用原始凭据路径和登录 ZCode 的同一系统用户，或改用账号授权")
	}
	plain, err := decryptCredentialPayload(parts[2], key)
	if err != nil {
		return "", fmt.Errorf("客户端凭据解密失败，请在 ZCode 重新登录后重试")
	}
	return plain, nil
}

func loadClientAccount(ctx context.Context, path, clientType string) (M, string, error) {
	data, err := readClientCredentialFile(path, 1<<20)
	if err != nil {
		return nil, "", fmt.Errorf("无法读取客户端凭据 %s，请检查路径和文件权限", path)
	}
	var record map[string]string
	if err := json.Unmarshal(data, &record); err != nil || len(record) == 0 {
		return nil, "", fmt.Errorf("客户端凭据文件无效，请选择 ZCode 的 credentials.json")
	}
	cipher := clientCredentialCipher{ctx: ctx, path: path, keys: map[string][]byte{}}
	token, err := cipher.decrypt(record["zcodejwttoken"])
	if err != nil {
		return nil, "", err
	}
	provider, err := cipher.decrypt(record["oauth:active_provider"])
	if err != nil {
		return nil, "", err
	}
	provider = strings.TrimSpace(provider)
	if provider == "" && token != "" && record["auth_token"] != "" {
		provider = "bigmodel"
	}
	if !validLoginProvider(provider) {
		return nil, "", fmt.Errorf("客户端登录渠道缺失或不受支持，请在 ZCode 重新登录，或使用账号授权")
	}
	values := M{}
	for _, field := range []string{"access_token", "refresh_token", "user_info"} {
		value := record["oauth:"+provider+":"+field]
		if provider == "bigmodel" && value == "" {
			if field == "access_token" {
				value = record["auth_token"]
			} else if field == "refresh_token" {
				value = record["refresh_token"]
			}
		}
		plain, err := cipher.decrypt(value)
		if err != nil {
			return nil, "", err
		}
		values[field] = plain
	}
	profile := obj(parse(values["user_info"], nil))
	tokens, identity, err := newLoginTokens(provider, token, profile)
	if err != nil {
		return nil, "", err
	}
	if revision, err := cipher.decrypt(record["oauth:session_revision"]); err != nil {
		return nil, "", err
	} else if revision != "" {
		tokens["session_revision"] = revision
	}
	access := str(values["access_token"])
	tokens["provider_access_token"], tokens["refresh_token"] = nullable(access), nullable(values["refresh_token"])
	switch {
	case provider == "zai_cn":
		tokens["provider_access_kind"] = "oauth"
	case provider == "zai" && clientType == "cli":
		// CLI and Desktop share the key name but not its token semantics.
		tokens["provider_access_kind"], tokens["oauth_access"] = "oauth", nullable(access)
	default:
		tokens["provider_access_kind"] = "business"
		if provider != "bigmodel" || access != token {
			tokens["business_token"] = nullable(access)
		}
		tokens["business_customer_id"] = nullable(first(profile["customerNumber"], jwtPayload(access)["customer_id"]))
	}
	// Optional linked credentials must never prevent importing a valid main login.
	if linked, err := cipher.decrypt(record["oauth:linked_bigmodel"]); err == nil {
		info := obj(parse(linked, nil))
		if str(info["userId"]) == identity {
			attachLinkedBigModel(tokens, identity, info)
		}
	}
	return tokens, identity, nil
}

func (a *App) importLocal(ctx context.Context, b M) (M, error) {
	if !a.localImportMu.TryLock() {
		return M{"ok": false, "error": "正在导入客户端账号，请稍候"}, nil
	}
	defer a.localImportMu.Unlock()
	path, err := clientCredentialsPath(str(b["credentials_path"]))
	if err != nil {
		return M{"ok": false, "error": err.Error()}, nil
	}
	clientType := str(fallback(b["client_type"], "desktop"))
	if clientType != "desktop" && clientType != "cli" {
		return M{"ok": false, "error": "客户端类型须为 desktop 或 cli"}, nil
	}
	tokens, identity, err := loadClientAccount(ctx, path, clientType)
	if err != nil {
		return M{"ok": false, "error": err.Error()}, nil
	}
	device := ""
	if data, err := readClientCredentialFile(filepath.Join(filepath.Dir(path), "telemetry-state.json"), 1<<20); err == nil {
		if mid := str(obj(parse(string(data), nil))["deviceMid"]); mid != "" && len(mid) <= 512 && !strings.ContainsAny(mid, "\r\n\x00") {
			device = mid
		}
	}
	account, updated, err := a.saveLoginAccount("", identity, M{"label": fallback(b["label"], "客户端-"+str(tokens["active_provider"])), "source": "local", "device_mid": device}, tokens)
	if err != nil {
		return nil, err
	}
	id := str(account["id"])
	result := a.verifyImportedAccount(ctx, account)
	imported := 1
	if updated {
		imported = 0
	}
	// Import success means credentials were saved; upstream verification is separate.
	return M{"ok": true, "id": id, "login_provider": tokens["active_provider"], "imported": imported, "updated": updated, "verified": truth(result["ok"]), "status": result["status"], "reason": result["reason"], "results": []any{result}}, nil
}
