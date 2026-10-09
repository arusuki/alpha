package platform

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
)

// Regenerate before builds after fetching tags. Only actual ancestor tags can
// occupy the three released update slots; working-tree changes remain pending.
//
//go:generate python3 ../../scripts/upgrade-history.py --write
//go:embed upgrade_history.json
var upgradeHistoryJSON []byte

const retainedDatabaseUpdates = 3

type taggedDatabaseUpdate struct {
	Tag  string `json:"tag"`
	From int    `json:"from"`
	To   int    `json:"to"`
}
type databaseUpgradeHistory struct {
	BaseSchema int                    `json:"base_schema"`
	Updates    []taggedDatabaseUpdate `json:"updates"`
}
type databaseUpgrade struct {
	From, To int
	Apply    func(*sql.Tx) error
}

// Register schema changes here, including pending development changes. A tag
// groups all steps since the preceding changed tag into ONE update. Steps below
// the generated BaseSchema can be removed when that retention window advances.
var databaseUpgrades = []databaseUpgrade{
	{From: 35, To: 36, Apply: upgradeGPUHistory},
	{From: 36, To: 37, Apply: upgradeScanSchedule},
	{From: 37, To: 38, Apply: installMihomo},
	{From: 38, To: 39, Apply: upgradeScheduledAgent},
}

func databaseUpgradePlan(version int) ([]databaseUpgrade, error) {
	var history databaseUpgradeHistory
	if err := json.Unmarshal(upgradeHistoryJSON, &history); err != nil {
		return nil, fmt.Errorf("invalid embedded database upgrade history: %w", err)
	}
	return planDatabaseUpgrade(history, databaseUpgrades, DatabaseVersion, version)
}

func planDatabaseUpgrade(history databaseUpgradeHistory, steps []databaseUpgrade, current, version int) ([]databaseUpgrade, error) {
	if history.BaseSchema <= 0 || len(history.Updates) > retainedDatabaseUpdates {
		return nil, fmt.Errorf("invalid database upgrade history; retain at most %d tagged updates", retainedDatabaseUpdates)
	}
	last := history.BaseSchema
	tags := map[string]bool{}
	boundaries := map[int]bool{last: true}
	for _, update := range history.Updates {
		if update.Tag == "" || tags[update.Tag] || update.From != last || update.To <= update.From {
			return nil, fmt.Errorf("invalid tagged database update: %+v", update)
		}
		tags[update.Tag] = true
		boundaries[update.To] = true
		last = update.To
	}
	if last > current {
		return nil, fmt.Errorf("tagged database history is newer than schema %d", current)
	}
	if version < history.BaseSchema || version > current {
		return nil, fmt.Errorf("unsupported database version %d; retained upgrade window is schema %d through %d (latest %d tagged updates with changes); existing data preserved; upgrade through an intermediate tagged release", version, history.BaseSchema, current, retainedDatabaseUpdates)
	}
	cursor := history.BaseSchema
	known := map[int]bool{cursor: true}
	var plan []databaseUpgrade
	for _, step := range steps {
		if step.To <= history.BaseSchema {
			continue
		}
		if step.From != cursor || step.To <= step.From || step.To > current || step.Apply == nil {
			return nil, fmt.Errorf("missing or invalid database upgrade step at schema %d; regenerate upgrade history and maintain the retained migration chain", cursor)
		}
		if step.From >= version {
			plan = append(plan, step)
		}
		cursor = step.To
		known[cursor] = true
	}
	if cursor != current || !known[version] {
		return nil, fmt.Errorf("no complete database upgrade path from %d to %d", version, current)
	}
	for boundary := range boundaries {
		if !known[boundary] {
			return nil, fmt.Errorf("missing tagged database boundary %d", boundary)
		}
	}
	return plan, nil
}
