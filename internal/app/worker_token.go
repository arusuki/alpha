package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"project-alpha/internal/cluster"
	"project-alpha/internal/platform"
)

const workerTokenName = "worker-token"

func readWorkerToken(directory string) (string, error) {
	path := filepath.Join(directory, workerTokenName)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", fmt.Errorf("worker token must be a regular file with permissions 0600: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if !cluster.ValidToken(token) {
		return "", fmt.Errorf("invalid saved worker token in %s; restore the token file", path)
	}
	return token, nil
}

// Keep the generated credential with the node's persistent identity. Publish a
// complete file atomically, without overwriting an existing or concurrent token.
func loadWorkerToken(directory string) (string, error) {
	token, err := readWorkerToken(directory)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return token, err
	}
	token = platform.RandomHex(32)
	temp, err := os.CreateTemp(directory, ".worker-token-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err = temp.Chmod(0600); err != nil {
		return "", err
	}
	if _, err = temp.WriteString(token + "\n"); err != nil {
		return "", err
	}
	if err = temp.Sync(); err != nil {
		return "", err
	}
	if err = temp.Close(); err != nil {
		return "", err
	}
	if err = os.Link(temp.Name(), filepath.Join(directory, workerTokenName)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readWorkerToken(directory)
		}
		return "", err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return token, nil
}
