package recovery

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	encryptedMagic      = "TAKOREC1"
	encryptedVersion    = byte(1)
	encryptionChunkSize = 1 << 20
	maxEncryptedRecord  = encryptionChunkSize + 32
)

// ParseKey accepts an out-of-band 256-bit base64 recovery key. The key is
// never stored in the bundle or object metadata.
func ParseKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("TAKO_RECOVERY_KEY must be standard base64 encoding of exactly 32 random bytes")
	}
	return key, nil
}

// CreateEncrypted streams tar/gzip output directly into authenticated
// encryption. No plaintext controller archive is ever written to disk.
func CreateEncrypted(destination, clusterID string, sources []Source, masterKey []byte) (*Result, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("recovery encryption requires a 256-bit key")
	}
	if strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("encrypted recovery destination is required")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".tako-encrypted-*.tmp")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = temporary.Close(); _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0600); err != nil {
		return nil, err
	}
	plainReader, plainWriter := io.Pipe()
	type archiveResult struct {
		manifest Manifest
		err      error
	}
	archiveDone := make(chan archiveResult, 1)
	go func() {
		manifest, writeErr := writeBundleArchive(plainWriter, clusterID, sources)
		_ = plainWriter.CloseWithError(writeErr)
		archiveDone <- archiveResult{manifest: manifest, err: writeErr}
	}()
	hash := sha256.New()
	encryptErr := encryptStream(plainReader, io.MultiWriter(temporary, hash), masterKey)
	if encryptErr != nil {
		_ = plainReader.CloseWithError(encryptErr)
	}
	archive := <-archiveDone
	if archive.err != nil {
		return nil, archive.err
	}
	if encryptErr != nil {
		return nil, encryptErr
	}
	if err := temporary.Sync(); err != nil {
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return nil, err
	}
	return &Result{Path: destination, SHA256: fmt.Sprintf("%x", hash.Sum(nil)), Manifest: archive.manifest}, nil
}

func VerifyEncrypted(path, expectedClusterID string, masterKey []byte) (*Manifest, error) {
	return verifyEncryptedTo(path, expectedClusterID, masterKey, "")
}

func verifyEncryptedTo(path, expectedClusterID string, masterKey []byte, destination string) (*Manifest, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("recovery decryption requires a 256-bit key")
	}
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	plainReader, plainWriter := io.Pipe()
	decryptDone := make(chan error, 1)
	go func() {
		decryptErr := decryptStream(in, plainWriter, masterKey)
		_ = plainWriter.CloseWithError(decryptErr)
		decryptDone <- decryptErr
	}()
	manifest, verifyErr := verifyBundleArchive(plainReader, expectedClusterID, destination)
	if verifyErr != nil {
		_ = plainReader.CloseWithError(verifyErr)
	}
	decryptErr := <-decryptDone
	if decryptErr != nil {
		return nil, fmt.Errorf("authenticate and decrypt recovery bundle: %w", decryptErr)
	}
	if verifyErr != nil {
		return nil, verifyErr
	}
	return manifest, nil
}

func encryptStream(reader io.Reader, writer io.Writer, masterKey []byte) error {
	salt, noncePrefix := make([]byte, 16), make([]byte, 4)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return err
	}
	if _, err := io.ReadFull(rand.Reader, noncePrefix); err != nil {
		return err
	}
	header := make([]byte, 0, len(encryptedMagic)+1+16+4+4)
	header = append(header, []byte(encryptedMagic)...)
	header = append(header, encryptedVersion)
	header = append(header, salt...)
	header = append(header, noncePrefix...)
	var chunkBytes [4]byte
	binary.BigEndian.PutUint32(chunkBytes[:], encryptionChunkSize)
	header = append(header, chunkBytes[:]...)
	if _, err := writer.Write(header); err != nil {
		return err
	}
	aead, err := recoveryAEAD(masterKey, salt)
	if err != nil {
		return err
	}
	buffer := make([]byte, encryptionChunkSize)
	counter := uint64(0)
	for {
		count, readErr := io.ReadFull(reader, buffer)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return readErr
		}
		final := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if count > 0 {
			if err := writeEncryptedRecord(writer, aead, header, noncePrefix, counter, buffer[:count], false); err != nil {
				return err
			}
			counter++
		}
		if final {
			return writeEncryptedRecord(writer, aead, header, noncePrefix, counter, nil, true)
		}
	}
}

func writeEncryptedRecord(writer io.Writer, aead cipher.AEAD, header, noncePrefix []byte, counter uint64, plaintext []byte, final bool) error {
	nonce := recordNonce(noncePrefix, counter)
	aad := recordAAD(header, counter, len(plaintext), final)
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	var prefix [5]byte
	if final {
		prefix[0] = 1
	}
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(ciphertext)))
	if _, err := writer.Write(prefix[:]); err != nil {
		return err
	}
	_, err := writer.Write(ciphertext)
	return err
}

func decryptStream(reader io.Reader, writer io.Writer, masterKey []byte) error {
	header := make([]byte, len(encryptedMagic)+1+16+4+4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if string(header[:len(encryptedMagic)]) != encryptedMagic || header[len(encryptedMagic)] != encryptedVersion {
		return fmt.Errorf("encrypted recovery header is invalid")
	}
	offset := len(encryptedMagic) + 1
	salt := header[offset : offset+16]
	offset += 16
	noncePrefix := header[offset : offset+4]
	offset += 4
	if binary.BigEndian.Uint32(header[offset:]) != encryptionChunkSize {
		return fmt.Errorf("encrypted recovery chunk size is invalid")
	}
	aead, err := recoveryAEAD(masterKey, salt)
	if err != nil {
		return err
	}
	for counter := uint64(0); ; counter++ {
		var prefix [5]byte
		if _, err := io.ReadFull(reader, prefix[:]); err != nil {
			return fmt.Errorf("encrypted recovery is truncated: %w", err)
		}
		final := prefix[0] == 1
		if prefix[0] > 1 {
			return fmt.Errorf("encrypted recovery record flag is invalid")
		}
		size := int(binary.BigEndian.Uint32(prefix[1:]))
		if size < aead.Overhead() || size > maxEncryptedRecord {
			return fmt.Errorf("encrypted recovery record size is invalid")
		}
		ciphertext := make([]byte, size)
		if _, err := io.ReadFull(reader, ciphertext); err != nil {
			return fmt.Errorf("encrypted recovery is truncated: %w", err)
		}
		plainLen := size - aead.Overhead()
		if final && plainLen != 0 {
			return fmt.Errorf("encrypted recovery final record is invalid")
		}
		plaintext, err := aead.Open(nil, recordNonce(noncePrefix, counter), ciphertext, recordAAD(header, counter, plainLen, final))
		if err != nil {
			return fmt.Errorf("encrypted recovery authentication failed")
		}
		if final {
			var extra [1]byte
			if count, err := reader.Read(extra[:]); err != io.EOF || count != 0 {
				return fmt.Errorf("encrypted recovery has trailing data")
			}
			return nil
		}
		if _, err := writer.Write(plaintext); err != nil {
			return err
		}
	}
}

func recoveryAEAD(masterKey, salt []byte) (cipher.AEAD, error) {
	derived := hkdfSHA256(masterKey, salt, []byte("tako-platform-recovery-v1"), 32)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)
	result, previous := make([]byte, 0, length), []byte(nil)
	for counter := byte(1); len(result) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(previous)
		_, _ = expand.Write(info)
		_, _ = expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		result = append(result, previous...)
	}
	return result[:length]
}

func recordNonce(prefix []byte, counter uint64) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func recordAAD(header []byte, counter uint64, plainLen int, final bool) []byte {
	aad := append([]byte(nil), header...)
	var suffix [13]byte
	binary.BigEndian.PutUint64(suffix[:8], counter)
	binary.BigEndian.PutUint32(suffix[8:12], uint32(plainLen))
	if final {
		suffix[12] = 1
	}
	return append(aad, suffix[:]...)
}
