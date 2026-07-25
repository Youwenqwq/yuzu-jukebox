package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// aesCipher 凭据落盘加密：AES-256-GCM，随机 nonce，
// 存储格式 "enc1:" + base64(nonce|ciphertext)。
const encPrefix = "enc1:"

type aesCipher struct {
	gcm cipher.AEAD
}

func newAESCipher(key []byte) (*aesCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("want 32-byte key, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &aesCipher{gcm: gcm}, nil
}

func (c *aesCipher) encrypt(plain string) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *aesCipher) decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil // 历史明文行
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("credential decode: %w", err)
	}
	if len(raw) < c.gcm.NonceSize() {
		return "", errors.New("credential too short")
	}
	plain, err := c.gcm.Open(nil, raw[:c.gcm.NonceSize()], raw[c.gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("credential decrypt (wrong key?): %w", err)
	}
	return string(plain), nil
}
