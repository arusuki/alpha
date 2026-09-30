package containers

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestImportDryRunAndRepeat(t *testing.T) {
	h, f, cfg := fixture(t)
	var out bytes.Buffer
	if err := h.Import(context.Background(), ImportOptions{DryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.records()
	if len(rows) != 0 || !strings.Contains(out.String(), "可导入 alice") || strings.Contains(out.String(), "legacy-secret") {
		t.Fatalf("dry run: %s %+v", &out, rows)
	}
	for _, query := range []string{"SELECT COUNT(*) FROM audit", "SELECT COUNT(*) FROM managed_containers"} {
		var n int
		if err := h.db.SQL.QueryRow(query).Scan(&n); err != nil || n != 0 {
			t.Fatalf("dry run wrote data: %d %v", n, err)
		}
	}
	if err := h.Import(context.Background(), ImportOptions{Containers: []string{"alice", f.c.ID}}, &out); err != nil {
		t.Fatal(err)
	}
	if err := h.Import(context.Background(), ImportOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	rows, _ = h.records()
	if len(rows) != 1 || rows[0].Owner != "alice" || !strings.Contains(out.String(), "已登记跳过 1") {
		t.Fatalf("repeat import: %s %+v", &out, rows)
	}
	current, _ := h.config()
	if current != cfg {
		t.Fatal("unexpected settings change")
	}
}

func TestImportContinuesAfterRejectedContainer(t *testing.T) {
	h, f, _ := fixture(t)
	badID := strings.Repeat("b", 64)
	h.run = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
		if args[0] == "ps" {
			return badID + "\n" + f.c.ID, nil
		}
		if args[0] == "container" && args[2] == badID {
			return "", fmt.Errorf("container disappeared")
		}
		return f.run(ctx, endpoint, args, input)
	}
	var out bytes.Buffer
	err := h.Import(context.Background(), ImportOptions{}, &out)
	rows, _ := h.records()
	if err == nil || len(rows) != 1 || !strings.Contains(out.String(), "container disappeared") || !strings.Contains(out.String(), "导入 1") {
		t.Fatalf("partial import: %v %s %+v", err, &out, rows)
	}
}

func TestImportRejectsChangesDuringScan(t *testing.T) {
	for _, change := range []string{"daemon", "config", "stopped", "replacement"} {
		t.Run(change, func(t *testing.T) {
			h, f, _ := fixture(t)
			inspections := 0
			h.run = func(ctx context.Context, endpoint string, args []string, input string) (string, error) {
				if args[0] == "container" {
					inspections++
				}
				result, err := f.run(ctx, endpoint, args, input)
				if args[0] == "container" && inspections == 1 {
					switch change {
					case "daemon":
						f.daemonID = "changed"
					case "config":
						f.c.Config.Image = "changed"
					case "stopped":
						f.c.State.Running = false
					case "replacement":
						f.c.ID = strings.Repeat("b", 64)
					}
				}
				return result, err
			}
			if err := h.Import(context.Background(), ImportOptions{}, io.Discard); err == nil {
				t.Fatal("accepted changed container")
			}
			rows, _ := h.records()
			if len(rows) != 0 {
				t.Fatal("registered changed container")
			}
		})
	}
}

func TestImportRegistrationRollback(t *testing.T) {
	h, _, cfg := fixture(t)
	h.Owner = func(tx *sql.Tx, id, owner string) error { return fmt.Errorf("ownership failure") }
	err := h.Import(context.Background(), ImportOptions{Endpoint: "unix:///tmp/other.sock"}, io.Discard)
	rows, _ := h.records()
	current, _ := h.config()
	var audits int
	if e := h.db.SQL.QueryRow("SELECT COUNT(*) FROM audit").Scan(&audits); e != nil {
		t.Fatal(e)
	}
	if err == nil || !strings.Contains(err.Error(), "ownership failure") || len(rows) != 0 || current != cfg || audits != 0 {
		t.Fatalf("non-atomic registration: %v %+v %+v audits=%d", err, rows, current, audits)
	}
}

func TestImportConnectionAndListFailure(t *testing.T) {
	for _, action := range []string{"info", "ps"} {
		t.Run(action, func(t *testing.T) {
			h, f, _ := fixture(t)
			f.failAction = action
			if err := h.Import(context.Background(), ImportOptions{}, io.Discard); err == nil {
				t.Fatal("missing Docker error")
			}
			rows, _ := h.records()
			if len(rows) != 0 {
				t.Fatal("registered after failure")
			}
		})
	}
}
