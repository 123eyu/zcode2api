//go:build !windows

package server

import (
	"context"
	"fmt"
)

func readClientKeyring(ctx context.Context, path string) (string, error) {
	return "", fmt.Errorf("暂不支持读取此系统的 ZCode 系统凭据库，请使用账号授权；自定义密钥凭据可通过 ZCODE_CREDENTIAL_SECRET 或 ZCODE_CREDENTIAL_SECRET_FILE 导入")
}
