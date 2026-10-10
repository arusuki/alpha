package updates

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPerNodeDeliveryPersistsAndAcknowledgesExactRelease(t *testing.T) {
	m := manager(t, "control")
	first := release("first", "v0.3.3")
	newer := release("newer", "v0.3.4")
	newer.Published = newer.Published.Add(time.Hour)
	preview := release("preview", "v0.3.5-rc1")
	preview.Prerelease = true
	preview.Published = newer.Published.Add(time.Hour)
	for _, r := range []Release{first, newer, preview} {
		if err := m.QueueDeliveries(r, []string{"online", "offline"}); err != nil {
			t.Fatal(err)
		}
	}
	// An old in-flight acknowledgement must not drop a newer obligation.
	if err := m.DeliveredToNode("offline", first); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Release{newer, preview} {
		if err := m.DeliveredToNode("online", r); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Release{first, newer} {
		if err := m.QueueDeliveries(r, []string{"online", "offline"}); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := New(m.directory, "control", m.id)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.PendingForNode("online")) != 0 {
		t.Fatal("acknowledged delivery requeued")
	}
	pending := restarted.PendingForNode("offline")
	if len(pending) != 2 || pending[0] != newer || pending[1] != preview {
		t.Fatal("lost channel or offline delivery", pending)
	}
	if err := restarted.QueueDeliveries(newer, []string{"online"}); err != nil {
		t.Fatal(err)
	}
	if len(restarted.PendingForNode("offline")) != 0 {
		t.Fatal("removed node retained")
	}
}
func TestInvalidAutomationJournalsPreserveData(t *testing.T) {
	for _, file := range []string{"update-retry.json", "update-deliveries.json"} {
		t.Run(file, func(t *testing.T) {
			m := manager(t, "control")
			path := filepath.Join(m.directory, file)
			raw := []byte(`{"format":999}`)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(m.directory, "control", m.id); err == nil {
				t.Fatal("invalid journal accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(raw) {
				t.Fatal("invalid journal changed", err)
			}
		})
	}
}
