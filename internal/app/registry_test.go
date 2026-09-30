package app

import (
	"context"
	"strings"
	"testing"
)

func TestRegistryOptionsRejectInvalidCombinations(t *testing.T) {
	t.Setenv("PROJECT_ALPHA_REGISTRY_TOKEN", strings.Repeat("s", 64))
	t.Setenv("REG_PASS", "Abcd1234")
	for _, args := range [][]string{
		{"--registry", "--control"}, {"--registry", "--worker"},
		{"--worker", "--registry-url", "https://registry.example.com"},
		{"--registry", "--registry-url", "https://registry.example.com"},
		{"--control", "--reg-pass-file", "/unused"},
		{"--control", "--registry-url", "http://public.example.com"},
	} {
		if err := Run(context.Background(), append(args, "--data-dir", t.TempDir())); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Setenv("REG_PASS", "short")
	if err := Run(context.Background(), []string{"--registry", "--data-dir", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "REG_PASS") {
		t.Fatalf("invalid pass: %v", err)
	}
}
