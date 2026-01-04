package crypto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTokenEncryptor(t *testing.T) {
	tests := []struct {
		name    string
		hexKey  string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid 32-byte key",
			hexKey:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr: false,
		},
		{
			name:    "empty key",
			hexKey:  "",
			wantErr: true,
			errMsg:  "encryption key is required",
		},
		{
			name:    "invalid hex",
			hexKey:  "not-hex-string",
			wantErr: true,
			errMsg:  "invalid encryption key: must be hex-encoded",
		},
		{
			name:    "too short key",
			hexKey:  "0123456789abcdef",
			wantErr: true,
			errMsg:  "must be 32 bytes",
		},
		{
			name:    "too long key",
			hexKey:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			wantErr: true,
			errMsg:  "must be 32 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := NewTokenEncryptor(tt.hexKey)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				assert.Nil(t, enc)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, enc)
			}
		})
	}
}

func TestEncryptDecrypt(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc, err := NewTokenEncryptor(hexKey)
	require.NoError(t, err)

	tests := []struct {
		name      string
		plaintext string
	}{
		{
			name:      "simple token",
			plaintext: "ya29.access-token-here",
		},
		{
			name:      "long token",
			plaintext: "ya29.a0AfH6SMBx1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-1234567890",
		},
		{
			name:      "token with special chars",
			plaintext: "token/with+special=chars&more",
		},
		{
			name:      "unicode content",
			plaintext: "token-with-unicode-\u00e9\u00e0\u00fc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Encrypt
			ciphertext, nonce, err := enc.Encrypt(tt.plaintext)
			require.NoError(t, err)
			assert.NotEmpty(t, ciphertext)
			assert.NotEmpty(t, nonce)

			// Ciphertext should be different from plaintext
			assert.NotEqual(t, []byte(tt.plaintext), ciphertext)

			// Decrypt
			decrypted, err := enc.Decrypt(ciphertext, nonce)
			require.NoError(t, err)
			assert.Equal(t, tt.plaintext, decrypted)
		})
	}
}

func TestEncryptProducesDifferentCiphertexts(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc, err := NewTokenEncryptor(hexKey)
	require.NoError(t, err)

	plaintext := "same-token-value"

	// Encrypt the same plaintext twice
	ciphertext1, nonce1, err := enc.Encrypt(plaintext)
	require.NoError(t, err)

	ciphertext2, nonce2, err := enc.Encrypt(plaintext)
	require.NoError(t, err)

	// Nonces should be different (random)
	assert.NotEqual(t, nonce1, nonce2)

	// Ciphertexts should be different (due to different nonces)
	assert.NotEqual(t, ciphertext1, ciphertext2)

	// Both should decrypt to the same plaintext
	decrypted1, err := enc.Decrypt(ciphertext1, nonce1)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted1)

	decrypted2, err := enc.Decrypt(ciphertext2, nonce2)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted2)
}

func TestDecryptWithWrongNonce(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc, err := NewTokenEncryptor(hexKey)
	require.NoError(t, err)

	plaintext := "secret-token"
	ciphertext, _, err := enc.Encrypt(plaintext)
	require.NoError(t, err)

	// Generate a different nonce
	_, wrongNonce, err := enc.Encrypt("different-plaintext")
	require.NoError(t, err)

	// Decryption with wrong nonce should fail
	_, err = enc.Decrypt(ciphertext, wrongNonce)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt")
}

func TestDecryptWithWrongKey(t *testing.T) {
	hexKey1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hexKey2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	enc1, err := NewTokenEncryptor(hexKey1)
	require.NoError(t, err)

	enc2, err := NewTokenEncryptor(hexKey2)
	require.NoError(t, err)

	plaintext := "secret-token"
	ciphertext, nonce, err := enc1.Encrypt(plaintext)
	require.NoError(t, err)

	// Decryption with different key should fail
	_, err = enc2.Decrypt(ciphertext, nonce)
	require.Error(t, err)
}

func TestEncryptEmptyPlaintext(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc, err := NewTokenEncryptor(hexKey)
	require.NoError(t, err)

	_, _, err = enc.Encrypt("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext cannot be empty")
}

func TestDecryptEmptyCiphertext(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc, err := NewTokenEncryptor(hexKey)
	require.NoError(t, err)

	nonce := make([]byte, 12) // GCM standard nonce size

	_, err = enc.Decrypt(nil, nonce)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ciphertext cannot be empty")

	_, err = enc.Decrypt([]byte{}, nonce)
	require.Error(t, err)
}

func TestDecryptEmptyNonce(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	enc, err := NewTokenEncryptor(hexKey)
	require.NoError(t, err)

	ciphertext := []byte("some-ciphertext")

	_, err = enc.Decrypt(ciphertext, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce cannot be empty")

	_, err = enc.Decrypt(ciphertext, []byte{})
	require.Error(t, err)
}

func TestGenerateKey(t *testing.T) {
	key1, err := GenerateKey()
	require.NoError(t, err)
	assert.Len(t, key1, 64) // 32 bytes = 64 hex chars

	key2, err := GenerateKey()
	require.NoError(t, err)
	assert.Len(t, key2, 64)

	// Keys should be different (random)
	assert.NotEqual(t, key1, key2)

	// Keys should be valid for creating encryptors
	enc, err := NewTokenEncryptor(key1)
	require.NoError(t, err)
	assert.NotNil(t, enc)
}
