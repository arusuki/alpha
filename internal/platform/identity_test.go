package platform

import (
	"strings"
	"testing"
)

func TestMissingServiceIdentityIsNotRecreated(t *testing.T) {
	db, err := OpenDatabase(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.SQL.Close()
	if _, err := db.CheckMode("worker"); err == nil || !strings.Contains(err.Error(), "new data directory") {
		t.Fatalf("missing identity was accepted: %v", err)
	}
	var count int
	if err := db.SQL.QueryRow("SELECT count(*) FROM service_identity").Scan(&count); err != nil || count != 0 {
		t.Fatalf("identity was silently recreated: %d %v", count, err)
	}
}
