package server

import (
	"context"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CREDENTIALW from wincred.h. ZCode's @napi-rs/keyring stores the key as a
// UTF-16 password under SHA256(path.resolve(credentialsPath)) + "." + service.
type clientWindowsCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func readClientKeyring(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target, err := windows.UTF16PtrFromString(hash(path) + "." + clientKeyringService)
	if err != nil {
		return "", fmt.Errorf("客户端凭据路径无效")
	}
	dll := windows.NewLazySystemDLL("advapi32.dll")
	read, free := dll.NewProc("CredReadW"), dll.NewProc("CredFree")
	var credential *clientWindowsCredential
	ok, _, _ := read.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&credential)))
	if ok == 0 || credential == nil {
		return "", fmt.Errorf("无法读取 ZCode 系统密钥，请使用登录客户端的同一 Windows 用户运行服务，并使用原始凭据路径；也可使用下方账号授权")
	}
	defer free.Call(uintptr(unsafe.Pointer(credential)))
	if credential.CredentialBlob == nil || credential.CredentialBlobSize != 86 {
		return "", fmt.Errorf("ZCode 系统密钥格式无效，请在客户端重新登录")
	}
	password := unsafe.Slice((*uint16)(unsafe.Pointer(credential.CredentialBlob)), 43)
	return windows.UTF16ToString(password), nil
}
