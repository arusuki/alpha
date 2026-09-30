package platform

import (
	"bytes"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	web "project-alpha/dist"
)

func TestEmbeddedBrowserAssets(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	server := NewServer(db, nil, web.Assets, nil, false)
	server.Control = true
	entries, err := web.Assets.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name(), func(t *testing.T) {
			path := "/" + entry.Name()
			if entry.Name() == "index.html" {
				path = "/"
			}
			if entry.Name() == "status.html" {
				path = "/status/0123456789abcdef0123456789abcdef"
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status %d, body %s", path, response.Code, response.Body.String())
			}
			if got, want := response.Header().Get("Content-Type"), mime.TypeByExtension(filepath.Ext(entry.Name())); got != want {
				t.Fatalf("GET %s: Content-Type %q, want %q", path, got, want)
			}
			body, err := web.Assets.ReadFile(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(response.Body.Bytes(), body) {
				t.Fatalf("GET %s: response differs from embedded asset", path)
			}
		})
	}
}
