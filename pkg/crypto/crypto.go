package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

const (
	// EncryptionHeader marks encrypted files
	EncryptionHeader = "TAKO_ENCRYPTED_V1:"

	// Default key derivation parameters
	pbkdf2Iterations = 100000
	saltSize         = 32
	keySize          = 32 // AES-256
	nonceSize        = 12 // GCM nonce size
)

// EncryptedData represents the structure of encrypted content
type EncryptedData struct {
	Salt       string `json:"salt"`       // Base64 encoded salt
	Nonce      string `json:"nonce"`      // Base64 encoded nonce
	Ciphertext string `json:"ciphertext"` // Base64 encoded encrypted data
}

// Encryptor handles encryption/decryption operations
type Encryptor struct {
	key []byte
}

// NewEncryptor creates a new Encryptor with the given passphrase
// The passphrase is derived into a secure key using PBKDF2
func NewEncryptor(passphrase string) *Encryptor {
	return &Encryptor{
		key: []byte(passphrase),
	}
}

// NewEncryptorFromKeyFile creates an Encryptor using a key file
// If the key file doesn't exist, it generates a new random key
func NewEncryptorFromKeyFile(keyPath string) (*Encryptor, error) {
	// Ensure directory exists
	dir := filepath.Dir(keyPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	// Check if key file exists
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		// Generate new random key
		key := make([]byte, keySize)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("failed to generate key: %w", err)
		}

		// Save key to file with secure permissions
		encoded := base64.StdEncoding.EncodeToString(key)
		if err := os.WriteFile(keyPath, []byte(encoded), 0600); err != nil {
			return nil, fmt.Errorf("failed to save key: %w", err)
		}

		return &Encryptor{key: key}, nil
	}

	// Read existing key
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to decode key: %w", err)
	}

	return &Encryptor{key: key}, nil
}

// Encrypt encrypts plaintext data and returns encrypted data with header
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	// Generate random salt
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	// Derive key using PBKDF2
	derivedKey := pbkdf2.Key(e.key, salt, pbkdf2Iterations, keySize, sha256.New)

	// Create AES cipher
	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate random nonce
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Encrypt data
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Create encrypted data structure
	encData := EncryptedData{
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(encData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal encrypted data: %w", err)
	}

	// Add header
	result := append([]byte(EncryptionHeader), jsonData...)
	return result, nil
}

// Decrypt decrypts data that was encrypted with Encrypt
func (e *Encryptor) Decrypt(data []byte) ([]byte, error) {
	// Check for encryption header
	if !IsEncrypted(data) {
		return nil, fmt.Errorf("data is not encrypted (missing header)")
	}

	// Remove header
	jsonData := data[len(EncryptionHeader):]

	// Parse encrypted data structure
	var encData EncryptedData
	if err := json.Unmarshal(jsonData, &encData); err != nil {
		return nil, fmt.Errorf("failed to parse encrypted data: %w", err)
	}

	// Decode base64 fields
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

	// Derive key using same parameters
	derivedKey := pbkdf2.Key(e.key, salt, pbkdf2Iterations, keySize, sha256.New)

	// Create AES cipher
	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Decrypt data
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt (wrong key or corrupted data): %w", err)
	}

	return plaintext, nil
}

// EncryptString encrypts a string and returns base64-encoded result
func (e *Encryptor) EncryptString(plaintext string) (string, error) {
	encrypted, err := e.Encrypt([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return string(encrypted), nil
}

// DecryptString decrypts a string that was encrypted with EncryptString
func (e *Encryptor) DecryptString(encrypted string) (string, error) {
	decrypted, err := e.Decrypt([]byte(encrypted))
	if err != nil {
		return "", err
	}
	return string(decrypted), nil
}

// EncryptFile encrypts a file in place
func (e *Encryptor) EncryptFile(filePath string) error {
	// Read file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Skip if already encrypted
	if IsEncrypted(data) {
		return nil // Already encrypted
	}

	// Encrypt data
	encrypted, err := e.Encrypt(data)
	if err != nil {
		return fmt.Errorf("failed to encrypt: %w", err)
	}

	// Get original file permissions
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	// Write encrypted data back
	if err := os.WriteFile(filePath, encrypted, info.Mode()); err != nil {
		return fmt.Errorf("failed to write encrypted file: %w", err)
	}

	return nil
}

// DecryptFile decrypts a file in place
func (e *Encryptor) DecryptFile(filePath string) error {
	// Read file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Skip if not encrypted
	if !IsEncrypted(data) {
		return nil // Not encrypted
	}

	// Decrypt data
	decrypted, err := e.Decrypt(data)
	if err != nil {
		return fmt.Errorf("failed to decrypt: %w", err)
	}

	// Get original file permissions
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	// Write decrypted data back
	if err := os.WriteFile(filePath, decrypted, info.Mode()); err != nil {
		return fmt.Errorf("failed to write decrypted file: %w", err)
	}

	return nil
}

// ReadEncryptedFile reads and decrypts a file, returning plaintext
// If file is not encrypted, returns contents as-is
func (e *Encryptor) ReadEncryptedFile(filePath string) ([]byte, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	if IsEncrypted(data) {
		return e.Decrypt(data)
	}

	return data, nil
}

// WriteEncryptedFile encrypts and writes data to a file
func (e *Encryptor) WriteEncryptedFile(filePath string, data []byte, perm os.FileMode) error {
	encrypted, err := e.Encrypt(data)
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, encrypted, perm)
}

// IsEncrypted checks if data has the encryption header
func IsEncrypted(data []byte) bool {
	return len(data) > len(EncryptionHeader) &&
		string(data[:len(EncryptionHeader)]) == EncryptionHeader
}

// IsFileEncrypted checks if a file is encrypted
func IsFileEncrypted(filePath string) (bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}
	return IsEncrypted(data), nil
}

// GetDefaultKeyPath returns the default encryption key path
func GetDefaultKeyPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".tako", "encryption.key")
}

// GetProjectKeyPath returns the project-specific encryption key path
func GetProjectKeyPath(projectDir string) string {
	return filepath.Join(projectDir, ".tako", "encryption.key")
}
