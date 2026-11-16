package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"zcode2api/internal/server/shared"
)

func LoadSecret(data string) (string, error) {
	p := filepath.Join(data, "secret.key")
	b, e := os.ReadFile(p)
	if e == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	s := shared.Random(32)
	return s, os.WriteFile(p, []byte(s), 0600)
}

func Encrypt(s, secret string) (string, error) {
	k := sha256.Sum256([]byte(secret))
	b, e := aes.NewCipher(k[:])
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return "", e
	}
	iv := make([]byte, 12)
	if _, e = rand.Read(iv); e != nil {
		return "", e
	}
	v := g.Seal(nil, iv, []byte(s), nil)
	enc := base64.RawURLEncoding.EncodeToString
	return "enc:v1:" + enc(iv) + "." + enc(v[len(v)-16:]) + "." + enc(v[:len(v)-16]), nil
}

func Decrypt(s, secret string) (string, error) {
	if !strings.HasPrefix(s, "enc:v1:") {
		return s, nil
	}
	k := sha256.Sum256([]byte(secret))
	return decryptCredentialPayload(strings.TrimPrefix(s, "enc:v1:"), k[:])
}

func decryptCredentialPayload(payload string, key []byte) (string, error) {
	parts := strings.Split(payload, ".")
	if len(parts) != 3 {
		return "", errors.New("凭据格式无效")
	}
	decode := base64.RawURLEncoding.DecodeString
	iv, e := decode(parts[0])
	if e != nil {
		return "", e
	}
	tag, e := decode(parts[1])
	if e != nil {
		return "", e
	}
	ct, e := decode(parts[2])
	if e != nil {
		return "", e
	}
	if len(iv) != 12 || len(tag) != 16 {
		return "", fmt.Errorf("凭据长度无效")
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return "", e
	}
	v, e := g.Open(nil, iv, append(ct, tag...), nil)
	return string(v), e
}
