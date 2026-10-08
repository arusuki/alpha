package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"project-alpha/internal/platform"
	"project-alpha/internal/storage"
)

func scheduleWorker(t *testing.T, f *fixture, id, root string, conflict bool) (*platform.Database, *httptest.Server) {
	t.Helper()
	db, err := platform.OpenDatabase(t.TempDir(), storage.Initialize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.SQL.Close() })
	if _, err = db.SQL.Exec("UPDATE settings SET value=json_set(value,'$.root',json(?),'$.exclude',json(?),'$.scan_mode','fast')", `["`+root+`"]`, `["`+root+`/skip"]`); err != nil {
		t.Fatal(err)
	}
	// No scheduling loop is needed for settings writes.
	handler := storage.NewHandler(storage.NewStore(db), &storage.Manager{})
	module := moduleFunc(func(w http.ResponseWriter, r *http.Request, u platform.User) (int, any, error) {
		if conflict && r.Method == "PUT" {
			if _, err := db.SQL.Exec("UPDATE settings SET revision=revision+1,value=json_set(value,'$.max_depth',9)"); err != nil {
				return 0, nil, err
			}
		}
		return handler.Dispatch(w, r, u)
	})
	worker, server := worker(t, id, Inventory{}, module)
	add(t, f, worker, server, root)
	return db, server
}

func readScanSettings(t *testing.T, db *platform.Database) storage.Settings {
	t.Helper()
	var raw string
	var settings storage.Settings
	if err := db.SQL.QueryRow("SELECT value,revision FROM settings WHERE id=1").Scan(&raw, &settings.Revision); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &settings.Value); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestDistributeScanSchedule(t *testing.T) {
	f := setup(t)
	a, b, c := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	db1, _ := scheduleWorker(t, f, a, "/first", false)
	db2, s2 := scheduleWorker(t, f, b, "/second", false)
	db3, _ := scheduleWorker(t, f, c, "/third", true)
	before1, before2 := readScanSettings(t, db1), readScanSettings(t, db2)
	plan := storage.Schedule{Mode: "calendar", Times: []string{"02:00", "14:30"}, Weekdays: []int{1, 3, 5}, Timezone: "Asia/Shanghai", RetainRecords: 7}
	send := func(ids []string) []scheduleResult {
		t.Helper()
		response := f.request(t, "POST", "/api/cluster/scan-schedule", map[string]any{"node_ids": ids, "schedule": plan})
		requireStatus(t, response, 200)
		var result struct {
			Results []scheduleResult `json:"results"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Results
	}
	results := send([]string{a, b})
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("results: %+v", results)
	}
	plan.Apply(&before1.Value)
	plan.Apply(&before2.Value)
	for i, pair := range [][2]storage.Settings{{before1, readScanSettings(t, db1)}, {before2, readScanSettings(t, db2)}} {
		if !reflect.DeepEqual(pair[0].Value, pair[1].Value) || pair[1].Revision != pair[0].Revision+1 {
			t.Fatalf("worker %d lost settings: %+v", i, pair)
		}
	}
	// One offline worker and a conflicting edit must not prevent other writes.
	s2.Close()
	plan.Mode = "off"
	results = send([]string{a, b, c})
	if len(results) != 3 || !results[0].OK || results[1].OK || results[1].Error == "" || results[2].OK || !strings.Contains(results[2].Error, "其他管理员") {
		t.Fatalf("partial results: %+v", results)
	}
	if got := readScanSettings(t, db3); got.Value.MaxDepth != 9 || got.Value.ScheduleMode != "off" || got.Value.RetainRecords != 0 {
		t.Fatalf("conflicting edit overwritten: %+v", got)
	}
	// A selected subset and repeated delivery preserve node-specific settings.
	revision := readScanSettings(t, db2).Revision
	results = send([]string{a})
	if !results[0].OK || readScanSettings(t, db2).Revision != revision {
		t.Fatal("unselected worker changed")
	}
	got := readScanSettings(t, db1)
	plan.Apply(&before1.Value)
	if !reflect.DeepEqual(got.Value, before1.Value) {
		t.Fatalf("repeat delivery changed settings: %+v", got)
	}
}

func TestDistributeScanScheduleValidationAndAuthorization(t *testing.T) {
	f := setup(t)
	id := strings.Repeat("a", 32)
	db, _ := scheduleWorker(t, f, id, "/first", false)
	before := readScanSettings(t, db)
	plan := map[string]any{"schedule_mode": "calendar", "interval_minutes": 0, "schedule_times": []string{"02:00"}, "schedule_weekdays": []int{1}, "schedule_timezone": "UTC", "retain_records": 0}
	registryID := strings.Repeat("b", 32)
	if _, err := f.db.SQL.Exec("INSERT INTO cluster_nodes VALUES(?,?,?,?,?,?,?)", registryID, "registry", "http://registry.invalid", strings.Repeat("s", 32), platform.Now(), "registry", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		ids    []string
		status int
	}{
		{"empty", []string{}, 400}, {"duplicate", []string{id, id}, 400}, {"invalid", []string{id, "bad"}, 400},
		{"missing", []string{id, strings.Repeat("c", 32)}, 404}, {"registry", []string{id, registryID}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", map[string]any{"node_ids": tc.ids, "schedule": plan}), tc.status)
		})
	}
	for key, value := range map[string]any{"schedule_times": []string{"25:00"}, "schedule_timezone": "Local", "schedule_weekdays": []int{}, "retain_records": -1, "interval_minutes": 10081, "schedule_mode": "bad"} {
		original := plan[key]
		plan[key] = value
		requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", map[string]any{"node_ids": []string{id}, "schedule": plan}), 400)
		plan[key] = original
	}
	for _, key := range []string{"retain_records", "schedule_times"} {
		original := plan[key]
		delete(plan, key)
		requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", map[string]any{"node_ids": []string{id}, "schedule": plan}), 400)
		plan[key] = nil
		requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", map[string]any{"node_ids": []string{id}, "schedule": plan}), 400)
		plan[key] = original
	}
	plan["root"] = []string{"/override"}
	requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", map[string]any{"node_ids": []string{id}, "schedule": plan}), 400)
	delete(plan, "root")
	body := map[string]any{"node_ids": []string{id}, "schedule": plan}
	csrf := f.csrf
	f.csrf = "wrong"
	requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", body), 403)
	f.csrf = csrf
	if _, err := f.db.SQL.Exec("UPDATE users SET role='viewer' WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.request(t, "POST", "/api/cluster/scan-schedule", body), 403)
	if got := readScanSettings(t, db); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected request changed settings: %+v", got)
	}
}
