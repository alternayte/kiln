package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// A registry token is written to kiln.db, and a token in the clear there is a
// token every reader of that file holds. The sealing key lives in
// config.json, which is the operator's alone, so a copy of the database on
// its own carries nothing usable.
//
// SealedPrefix marks a value this package wrote. A value without it was
// written before the key existed, and the caller decides what that means.
const SealedPrefix = "k1:"

// KeySize is the length of the sealing key, in bytes.
const KeySize = 32

// ErrNoKey reports that a sealed value cannot be read because the host holds
// no key. Losing the key loses every value sealed with it.
var ErrNoKey = errors.New("store: this host has no sealing key")

// Seal encrypts one value with the sealing key. The nonce is random per call
// and travels with the ciphertext.
func Seal(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("store: nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return SealedPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Unseal decrypts one value. A value that was never sealed is returned
// unchanged, so a host that gains a key later still reads what it wrote
// before.
func Unseal(key []byte, value string) (string, error) {
	if len(value) < len(SealedPrefix) || value[:len(SealedPrefix)] != SealedPrefix {
		return value, nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.RawStdEncoding.DecodeString(value[len(SealedPrefix):])
	if err != nil {
		return "", fmt.Errorf("store: sealed value: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("store: sealed value is too short")
	}
	plaintext, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		// A wrong key and a tampered value are the same failure here, and
		// neither is recoverable.
		return "", fmt.Errorf("store: the sealed value does not open with this key")
	}
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) == 0 {
		return nil, ErrNoKey
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("store: the sealing key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewKey returns a fresh sealing key.
func NewKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("store: key: %w", err)
	}
	return key, nil
}
