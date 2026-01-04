package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// TokenEncryptor handles AES-256-GCM encryption/decryption of OAuth tokens
type TokenEncryptor struct {
	key []byte
}

// NewTokenEncryptor creates a new encryptor from a 32-byte hex-encoded key
func NewTokenEncryptor(hexKey string) (*TokenEncryptor, error) {
	if hexKey == "" {
		return nil, errors.New("encryption key is required")
	}

	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption key: must be hex-encoded: %w", err)
	}

	if len(key) != 32 {
		return nil, fmt.Errorf("invalid encryption key: must be 32 bytes (64 hex chars), got %d bytes", len(key))
	}

	return &TokenEncryptor{key: key}, nil
}

// Encrypt encrypts plaintext using AES-256-GCM
// Returns the ciphertext and nonce (both needed for decryption)
func (e *TokenEncryptor) Encrypt(plaintext string) (ciphertext, nonce []byte, err error) {
	if plaintext == "" {
		return nil, nil, errors.New("plaintext cannot be empty")
	}

	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create GCM: %w", err)
	}

	// Generate random nonce
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Encrypt the plaintext
	ciphertext = gcm.Seal(nil, nonce, []byte(plaintext), nil)

	return ciphertext, nonce, nil
}

// Decrypt decrypts ciphertext using AES-256-GCM
func (e *TokenEncryptor) Decrypt(ciphertext, nonce []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", errors.New("ciphertext cannot be empty")
	}
	if len(nonce) == 0 {
		return "", errors.New("nonce cannot be empty")
	}

	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	if len(nonce) != gcm.NonceSize() {
		return "", fmt.Errorf("invalid nonce size: expected %d, got %d", gcm.NonceSize(), len(nonce))
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// GenerateKey generates a new random 32-byte encryption key and returns it as a hex string
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return hex.EncodeToString(key), nil
}
