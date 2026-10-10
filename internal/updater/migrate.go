package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"project-alpha/internal/buildinfo"
	"project-alpha/internal/platform"
)

// The installer owns container changes and rollback. This child only validates
// the deployment before committing the database: callers cannot recover Go error
// identities from a failed subprocess and may restart after a database rollback.
func migrateRelease(ctx context.Context, role, directory, binDir string, lock *os.File, docker dockerCommand, out io.Writer) error {
	return platform.UpgradeDatabase(directory, role, lock, func() error {
		if role != "worker" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(binDir, ".alpha-update-pending")); os.IsNotExist(err) {
			// Direct database maintenance has no release installation to complete.
			return nil
		} else if err != nil {
			return err
		}
		stage, err := migrationPayload(binDir, buildinfo.Version)
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		db, err := platform.OpenExistingDatabase(directory, true)
		if err != nil {
			return err
		}
		defer db.SQL.Close()
		// Image preparation belongs to the installer too. Validation after
		// shutdown must neither build images nor access a registry.
		inspectDocker := func(ctx context.Context, endpoint string, args []string, input []byte) ([]byte, error) {
			var ref string
			switch args[0] {
			case "build":
				index := slices.Index(args, "--tag")
				if index < 0 || index+1 >= len(args) {
					return nil, errors.New("missing release image tag")
				}
				ref = args[index+1]
			case "pull":
				ref = args[1]
			}
			if ref != "" {
				if _, err := docker(ctx, endpoint, []string{"image", "inspect", ref}, nil); err != nil {
					return nil, fmt.Errorf("release service image is unavailable; bootstrap the target release's alpha-updater and retry the full update: %w", err)
				}
				return nil, nil
			}
			return docker(ctx, endpoint, args, input)
		}
		plan, err := prepareContainers(ctx, db, stage, inspectDocker, out)
		if err != nil {
			return err
		}
		if len(plan.Updates) != 0 {
			return errors.New("worker service containers do not match the target release; bootstrap the target release's alpha-updater and retry the full update")
		}
		return nil
	})
}

// Locate the release by its installed binary hashes, never by stage age or a
// service-plan format. This also works for CLI installs. Extraction uses a new
// private directory; the caller's archive and prepared files remain untouched.
func migrationPayload(binDir, tag string) (string, error) {
	if err := ValidateTag(tag); err != nil {
		return "", err
	}
	hashes := map[string]string{}
	for _, file := range binaries("worker") {
		hash, err := fileHash(filepath.Join(binDir, file.destination))
		if err != nil {
			return "", err
		}
		hashes[file.source] = hash
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return "", err
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".alpha-stage-") {
			continue
		}
		archive := filepath.Join(binDir, entry.Name(), "release.tar.gz")
		stage, err := extractMigrationPayload(binDir, archive, tag, hashes)
		if err == nil {
			return stage, nil
		}
		failures = append(failures, err)
	}
	return "", fmt.Errorf("cannot find the installed release's container payload in the staged archive: %w", errors.Join(append([]error{errors.New("matching release package required")}, failures...)...))
}

func extractMigrationPayload(binDir, archive, tag string, hashes map[string]string) (stage string, err error) {
	info, err := os.Lstat(archive)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxArchive {
		return "", fmt.Errorf("invalid staged release archive: %s", archive)
	}
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stage, err = os.MkdirTemp(binDir, ".alpha-migrate-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(stage)
		}
	}()
	var names []string
	for _, file := range releaseFiles("worker") {
		names = append(names, file.source)
	}
	if err = extract(f, stage, "project-alpha_"+tag+"_linux_"+runtime.GOARCH, names); err != nil {
		return stage, err
	}
	p := PreparedUpdate{Stage: stage, Hashes: hashes}
	if err = p.verify(binaries("worker")); err != nil {
		return stage, fmt.Errorf("staged archive does not match installed release: %w", err)
	}
	return stage, nil
}
