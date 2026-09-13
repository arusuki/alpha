package fsutil

import (
	"path/filepath"
)

func Canonical(path string) string {
	p, err := filepath.Abs(path)
	if err != nil {
		p = filepath.Clean(path)
	}
	// Resolve existing ancestors even when the final component does not exist.
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return resolved
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(Canonical(parent), filepath.Base(p))
}
