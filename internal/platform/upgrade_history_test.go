package platform

import (
	"database/sql"
	"strings"
	"testing"
)

func TestUpgradeWindowCountsTaggedGroupsNotSteps(t *testing.T) {
	history := databaseUpgradeHistory{BaseSchema: 34, Updates: []taggedDatabaseUpdate{
		{Tag: "v0.4.0", From: 34, To: 36},
		{Tag: "v0.5.0", From: 36, To: 38},
		{Tag: "v0.6.0", From: 38, To: 39},
	}}
	// 39 -> 40 -> 41 is still untagged. It must not evict v0.4.0.
	var steps []databaseUpgrade
	for from := 33; from < 41; from++ {
		steps = append(steps, databaseUpgrade{from, from + 1, func(*sql.Tx) error { return nil }})
	}
	for _, start := range []int{34, 35, 36, 38, 39, 40, 41} {
		plan, err := planDatabaseUpgrade(history, steps, 41, start)
		if err != nil || len(plan) != 41-start {
			t.Fatalf("start %d: plan=%v err=%v", start, plan, err)
		}
	}
	if _, err := planDatabaseUpgrade(history, steps, 41, 33); err == nil || !strings.Contains(err.Error(), "retained upgrade window") {
		t.Fatal("oldest tagged update was not expired", err)
	}
	// A new changed tag folds the pending steps into ONE update and expires only
	// the oldest tagged group. An unchanged tag never enters the generated list.
	history = databaseUpgradeHistory{BaseSchema: 36, Updates: []taggedDatabaseUpdate{
		{Tag: "v0.5.0", From: 36, To: 38}, {Tag: "v0.6.0", From: 38, To: 39}, {Tag: "v0.7.0", From: 39, To: 41},
	}}
	if _, err := planDatabaseUpgrade(history, steps, 41, 34); err == nil {
		t.Fatal("fourth update retained")
	}
	if plan, err := planDatabaseUpgrade(history, steps, 41, 36); err != nil || len(plan) != 5 {
		t.Fatal(plan, err)
	}
	// Old source implementations can be deleted without breaking the window.
	if plan, err := planDatabaseUpgrade(history, steps[3:], 41, 36); err != nil || len(plan) != 5 {
		t.Fatal(plan, err)
	}
}

func TestUpgradeHistoryRejectsIncompleteOrInvalidPaths(t *testing.T) {
	noop := func(*sql.Tx) error { return nil }
	steps := []databaseUpgrade{{33, 34, noop}, {34, 35, noop}}
	for _, history := range []databaseUpgradeHistory{
		{BaseSchema: 33, Updates: []taggedDatabaseUpdate{{Tag: "", From: 33, To: 34}}},
		{BaseSchema: 33, Updates: []taggedDatabaseUpdate{{Tag: "v0.4.0", From: 33, To: 33}}},
		{BaseSchema: 33, Updates: []taggedDatabaseUpdate{{Tag: "v0.4.0", From: 34, To: 35}}},
		{BaseSchema: 33, Updates: []taggedDatabaseUpdate{{Tag: "v0.4.0", From: 33, To: 34}, {Tag: "v0.4.0", From: 34, To: 35}}},
	} {
		if _, err := planDatabaseUpgrade(history, steps, 35, 33); err == nil {
			t.Fatal("invalid history accepted", history)
		}
	}
	if _, err := planDatabaseUpgrade(databaseUpgradeHistory{BaseSchema: 33}, steps[1:], 35, 33); err == nil {
		t.Fatal("migration gap accepted")
	}
	if _, err := planDatabaseUpgrade(databaseUpgradeHistory{BaseSchema: 33}, steps, 35, 36); err == nil {
		t.Fatal("newer database accepted")
	}
}

func TestCurrentUpgradeHistoryHasCompletePath(t *testing.T) {
	if _, err := databaseUpgradePlan(DatabaseVersion); err != nil {
		t.Fatal(err)
	}
}
