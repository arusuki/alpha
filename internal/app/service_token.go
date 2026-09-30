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

func readServiceToken(directory, mode string) (string, error) {
	path := filepath.Join(directory, mode+"-token")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", fmt.Errorf("%s token must be a regular file with permissions 0600: %s", mode, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if !cluster.ValidToken(token) {
		return "", fmt.Errorf("invalid saved %s token in %s; restore the token file", mode, path)
	}
	return token, nil
}

// Keep the generated credential with the node's persistent identity. Publish a
// complete file atomically, without overwriting an existing or concurrent token.
func loadServiceToken(directory, mode string) (string, error) {
	token, err := readServiceToken(directory, mode)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return token, err
	}
	token = platform.RandomHex(32)
	temp, err := os.CreateTemp(directory, "."+mode+"-token-*")
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
	if err = os.Link(temp.Name(), filepath.Join(directory, mode+"-token")); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readServiceToken(directory, mode)
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
