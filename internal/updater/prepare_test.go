package updater

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestPrepareRunningServiceAndInstallOffline(t *testing.T) {
	target := buildTarget(t)
	for _, tamper := range []bool{false, true} {
		t.Run(fmt.Sprint(tamper), func(t *testing.T) {
			db := oldDatabase(t, "registry")
			bin := t.TempDir()
			original := script("project-alpha", "v0.4.0")
			writeFile(t, filepath.Join(bin, "project-alpha"), original)
			lock, err := db.LockService()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			var prepared *PreparedUpdate
			o := options{role: "registry", directory: db.Directory, binDir: bin, prepare: func(p *PreparedUpdate) error { prepared = p; return nil }}
			if err := run(context.Background(), o, releaseServer(t, target, false), io.Discard); err != nil {
				t.Fatal(err)
			}
			if prepared == nil {
				t.Fatal("missing staged release")
			}
			assertVersion(t, db, 34)
			got, _ := os.ReadFile(filepath.Join(bin, "project-alpha"))
			if string(got) != string(original) {
				t.Fatal("preparation replaced running program")
			}
			backups, _ := filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
			if len(backups) != 0 {
				t.Fatal("database backed up before shutdown")
			}
			// A write during preparation must be included in the eventual offline backup.
			if _, err := db.SQL.Exec("INSERT INTO users VALUES('late','late','late-hash','admin',1,124)"); err != nil {
				t.Fatal(err)
			}
			lock.Close()
			if tamper {
				writeFile(t, filepath.Join(prepared.Stage, "project-alpha"), script("project-alpha", "v0.3.9"))
			}
			o.prepare = nil
			o.prepared = prepared
			// runConfigured uses no HTTP client at all for the installation phase.
			err = runConfigured(context.Background(), o, io.Discard)
			if tamper {
				if err == nil || !strings.Contains(err.Error(), "checksum changed") {
					t.Fatal(err)
				}
				assertVersion(t, db, 34)
				got, _ := os.ReadFile(filepath.Join(bin, "project-alpha"))
				if string(got) != string(original) {
					t.Fatal("tampered update modified installation")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertVersion(t, db, platform.DatabaseVersion)
				backups, _ = filepath.Glob(filepath.Join(db.Directory, "platform.sqlite3.backup-*"))
				if len(backups) != 1 {
					t.Fatal(backups)
				}
				backup, err := sql.Open("sqlite3", backups[0])
				if err != nil {
					t.Fatal(err)
				}
				defer backup.Close()
				var hash string
				if err := backup.QueryRow("SELECT password_hash FROM users WHERE id='late'").Scan(&hash); err != nil || hash != "late-hash" {
					t.Fatal("backup missed online writes", hash, err)
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublishedNoticeDownloadsWithoutAPI(t *testing.T) {
	db := oldDatabase(t, "registry")
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "project-alpha"), script("project-alpha", "v0.4.0"))
	g := releaseServer(t, script("alpha-updater", "v0.5.4"), false)
	transport := g.client.Transport
	requests := 0
	g.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "github.com" || !strings.HasPrefix(r.URL.Path, "/arusuki/alpha/releases/download/v0.5.4/") {
			t.Errorf("unexpected API request: %s", r.URL)
			return nil, fmt.Errorf("API blocked")
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("public request included a token")
		}
		name := "archive"
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			name = "sums"
		}
		forwarded, _ := http.NewRequestWithContext(r.Context(), "GET", g.base+"/assets/"+name, nil)
		return transport.RoundTrip(forwarded)
	})}
	o := options{role: "registry", directory: db.Directory, binDir: bin, tag: "v0.5.4", published: true, prepare: func(p *PreparedUpdate) error { return nil }}
	if err := run(context.Background(), o, g, io.Discard); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want only checksum and archive", requests)
	}
}

func TestGitHubErrorsDistinguishRateLimitAndPermissions(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if limited {
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Limit", "60")
					w.Header().Set("X-RateLimit-Reset", "1800000000")
				}
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Access denied"}`)
			}))
			defer server.Close()
			g := github{server.Client(), server.URL, "arusuki/alpha", ""}
			_, err := g.byTag(context.Background(), "v0.4.1", false)
			if err == nil || !strings.Contains(err.Error(), "Access denied") || !strings.Contains(err.Error(), "authentication=none") {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "rate limit exceeded") != limited {
				t.Fatal(err)
			}
			if limited && !strings.Contains(err.Error(), "X-RateLimit-Remaining=0") {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatal("retried forbidden request", requests)
			}
		})
	}
}
