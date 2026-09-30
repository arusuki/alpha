package app

import (
	"fmt"
	"os"
	"strings"
)

func readSecret(path, environment string) (string, error) {
	if path == "" {
		return os.Getenv(environment), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", environment, err)
	}
	return strings.TrimSpace(string(raw)), nil
}
