// Package credentials encrypts service credentials using a private key in the data directory.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const Prefix = "v1:"

func readKey(directory, filename string) ([]byte, error) {
	path := filepath.Join(directory, filename)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("API key encryption key is missing (%s); restore it or use a new data directory: %w", filename, os.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("API key encryption key must be a regular file with permissions 0600: %s", path)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid API key encryption key in %s; restore it or use a new data directory", path)
	}
	return key, nil
}

func createKey(directory, filename string) ([]byte, error) {
	key, err := readKey(directory, filename)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		// The missing-file case is handled below; other errors must never
		// cause a replacement key to be generated.
		return key, err
	}
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(directory, "."+filename+"-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err != nil {
		temp.Close()
		return nil, err
	}
	if _, err = temp.Write(key); err != nil {
		temp.Close()
		return nil, err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return nil, err
	}
	if err = temp.Close(); err != nil {
		return nil, err
	}
	if err = os.Link(temp.Name(), filepath.Join(directory, filename)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readKey(directory, filename)
		}
		return nil, err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return nil, err
	}
	return key, nil
}

// Encrypt uses a separate key file and authentication context for each credential type.
func Encrypt(directory, filename, purpose, plaintext string) (string, error) {
	key, err := createKey(directory, filename)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(purpose))
	return Prefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt never recreates a missing key or accepts an unknown ciphertext format.
func Decrypt(directory, filename, purpose, encoded string) (string, error) {
	if !strings.HasPrefix(encoded, Prefix) {
		return "", fmt.Errorf("invalid encrypted API key format; use a new data directory")
	}
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encoded, Prefix))
	if err != nil {
		return "", fmt.Errorf("invalid encrypted API key format; use a new data directory")
	}
	key, err := readKey(directory, filename)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("invalid encrypted API key format; use a new data directory")
	}
	plaintext, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(purpose))
	if err != nil {
		return "", fmt.Errorf("cannot decrypt API key; restore %s or use a new data directory", filename)
	}
	return string(plaintext), nil
}
