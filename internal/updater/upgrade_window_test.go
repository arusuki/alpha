package updater

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/platform"
)

func TestBuiltUpdaterRejectsExpiredWindow(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "alpha-updater")
	writeFile(t, binary, buildTarget(t))
	for _, entry := range []string{"--database-only", "_migrate"} {
		for _, role := range []string{"control", "worker", "registry"} {
			for _, version := range []int{42, 43, 44} {
				t.Run(fmt.Sprintf("%s/%s/%d", entry, role, version), func(t *testing.T) {
					db := populatedOldDatabase(t, role)
					// An unsupported header must be rejected before any schema is interpreted.
					if _, err := db.SQL.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
						t.Fatal(err)
					}
					snapshot := func() map[string][]map[string]any {
						rows, err := platform.Rows(db.SQL, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
						if err != nil {
							t.Fatal(err)
						}
						result := map[string][]map[string]any{"schema": rows}
						for _, row := range rows {
							if row["type"] != "table" {
								continue
							}
							name := row["name"].(string)
							result[name], err = platform.Rows(db.SQL, `SELECT * FROM "`+name+`"`)
							if err != nil {
								t.Fatal(err)
							}
						}
						return result
					}
					before := snapshot()
					err := runScheduledMigration(t, binary, entry, role, db)
					if err == nil || !strings.Contains(err.Error(), "unsupported database version") || !strings.Contains(err.Error(), "existing data preserved") {
						t.Fatal("expected explicit nonzero rejection", err)
					}
					assertVersion(t, db, version)
					if !reflect.DeepEqual(before, snapshot()) {
						t.Fatal("rejected update changed existing data")
					}
				})
			}
		}
	}
}
