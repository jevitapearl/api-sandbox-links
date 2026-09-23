// Package secret provides authenticated encryption for values that must be
// stored at rest in Postgres: GitHub OAuth tokens, sandbox env vars, and
// sidecar database connection strings. It uses AES-256-GCM from the standard
// library (chosen deliberately — no third-party dependency to audit).
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// Box wraps an encryption key. Keys are 32 bytes (AES-256).
type Box struct {
	aead cipher.AEAD
}

// NewBox builds an encryption box from a 32-byte key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret: encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: creating gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Encrypt seals plaintext using AES-256-GCM. The output carries a random
// 12-byte nonce prefixed to the ciphertext, hex-encoded for storage in text
// columns. No effort is made to hide plaintext length; that is not a threat
// model requirement for a bachelor's project.
func (b *Box) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: generating nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, plaintext, nil)
	dst := make([]byte, hex.EncodedLen(len(sealed)))
	hex.Encode(dst, sealed)
	return dst, nil
}

// Decrypt reverses Encrypt: it decodes the hex ciphertext, extracts the
// nonce, and authenticates + decrypts the payload. Any tampering yields an
// error rather than garbage.
func (b *Box) Decrypt(encoded []byte) ([]byte, error) {
	sealed := make([]byte, hex.DecodedLen(len(encoded)))
	if _, err := hex.Decode(sealed, encoded); err != nil {
		return nil, fmt.Errorf("secret: decoding ciphertext: %w", err)
	}
	if len(sealed) < b.aead.NonceSize() {
		return nil, fmt.Errorf("secret: ciphertext too short")
	}
	nonce, ciphertext := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secret: decrypting (authentication failed): %w", err)
	}
	return plaintext, nil
}