package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	// PassphraseHeader marks passphrase-encrypted data
	PassphraseHeader = "TAKO_PASSPHRASE_V1:"

	// Argon2id parameters
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MB
	argonThreads = 4
	argonSaltLen = 16
)

// PassphraseEncryptedData represents passphrase-encrypted content
type PassphraseEncryptedData struct {
	Salt       string `json:"salt"`       // Base64 encoded salt
	Nonce      string `json:"nonce"`      // Base64 encoded nonce
	Ciphertext string `json:"ciphertext"` // Base64 encoded encrypted data
	Time       uint32 `json:"time"`       // Argon2id time parameter
	Memory     uint32 `json:"memory"`     // Argon2id memory parameter (KiB)
	Threads    uint8  `json:"threads"`    // Argon2id threads parameter
}

// EncryptWithPassphrase encrypts plaintext using a passphrase with Argon2id KDF + AES-256-GCM
func EncryptWithPassphrase(plaintext []byte, passphrase string) ([]byte, error) {
	// Generate random salt
	salt := make([]byte, argonSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	// Derive key using Argon2id
	key := argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, keySize)

	// Create AES-256-GCM cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate random nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Encrypt
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Build output structure
	encData := PassphraseEncryptedData{
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		Time:       argonTime,
		Memory:     argonMemory,
		Threads:    argonThreads,
	}

	jsonData, err := json.Marshal(encData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal encrypted data: %w", err)
	}

	return append([]byte(PassphraseHeader), jsonData...), nil
}

// DecryptWithPassphrase decrypts data that was encrypted with EncryptWithPassphrase
func DecryptWithPassphrase(data []byte, passphrase string) ([]byte, error) {
	if !IsPassphraseEncrypted(data) {
		return nil, fmt.Errorf("data is not passphrase-encrypted (missing header)")
	}

	// Remove header
	jsonData := data[len(PassphraseHeader):]

	var encData PassphraseEncryptedData
	if err := json.Unmarshal(jsonData, &encData); err != nil {
		return nil, fmt.Errorf("failed to parse encrypted data: %w", err)
	}

	salt, err := base64.StdEncoding.DecodeString(encData.Salt)
	if err != nil {
		return nil, fmt.Errorf("failed to decode salt: %w", err)
	}

	nonce, err := base64.StdEncoding.DecodeString(encData.Nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to decode nonce: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encData.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	// Use stored Argon2id params for forward compatibility
	time := encData.Time
	memory := encData.Memory
	threads := encData.Threads
	if time == 0 {
		time = argonTime
	}
	if memory == 0 {
		memory = argonMemory
	}
	if threads == 0 {
		threads = argonThreads
	}

	// Derive key using Argon2id with stored params
	key := argon2.IDKey([]byte(passphrase), salt, time, memory, threads, keySize)

	// Decrypt
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt (wrong passphrase or corrupted data): %w", err)
	}

	return plaintext, nil
}

// IsPassphraseEncrypted checks if data has the passphrase encryption header
func IsPassphraseEncrypted(data []byte) bool {
	return len(data) > len(PassphraseHeader) &&
		strings.HasPrefix(string(data), PassphraseHeader)
}
