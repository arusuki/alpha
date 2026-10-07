package platform

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// upgradeContainerChoices upgrades only database storage, atomically. Conflicting
// old ownership is reported for explicit correction; no data is dropped.
func upgradeContainerChoices(tx *sql.Tx) error {
	tables, err := Rows(tx, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	for _, r := range tables {
		exists[r["name"].(string)] = true
	}
	if !exists["users"] {
		return fmt.Errorf("缺少平台表，拒绝升级")
	}
	if exists["members"] {
		if _, err = tx.Exec("ALTER TABLE members ADD COLUMN registration_containers TEXT NOT NULL DEFAULT '[]'"); err != nil {
			return err
		}
	}
	if exists["member_node_resources"] {
		if _, err = tx.Exec(`ALTER TABLE member_node_resources ADD COLUMN mode TEXT NOT NULL DEFAULT 'create' CHECK(mode IN ('create','adopt'));
 ALTER TABLE member_node_resources ADD COLUMN target_id TEXT NOT NULL DEFAULT '';`); err != nil {
			return err
		}
		rows, e := Rows(tx, "SELECT member_id,node_id FROM member_node_resources ORDER BY node_id")
		if e != nil {
			return e
		}
		choices := map[string][]map[string]string{}
		for _, r := range rows {
			id := r["member_id"].(string)
			choices[id] = append(choices[id], map[string]string{"node_id": r["node_id"].(string), "mode": "create", "container_id": ""})
		}
		for id, value := range choices {
			raw, _ := json.Marshal(value)
			if _, err = tx.Exec("UPDATE members SET registration_containers=? WHERE id=?", string(raw), id); err != nil {
				return err
			}
		}
	}
	if exists["managed_containers"] {
		if _, err = tx.Exec(`ALTER TABLE member_container_slots ADD COLUMN mode TEXT NOT NULL DEFAULT 'create' CHECK(mode IN ('create','adopt'));
 ALTER TABLE member_container_slots ADD COLUMN container_id TEXT NOT NULL DEFAULT '';
 UPDATE member_container_slots SET container_id=COALESCE((SELECT id FROM managed_containers WHERE name='alpha-'||member_id),'');
 CREATE UNIQUE INDEX member_slot_container ON member_container_slots(container_id) WHERE deleted=0 AND container_id<>'';`); err != nil {
			return err
		}
		if exists["owners"] {
			if _, err = tx.Exec(`UPDATE managed_containers SET owner=(SELECT owner FROM owners WHERE container_id=managed_containers.id) WHERE EXISTS(SELECT 1 FROM owners WHERE container_id=managed_containers.id);
 INSERT INTO owners(container_id,owner) SELECT id,owner FROM managed_containers WHERE id NOT IN (SELECT container_id FROM owners);`); err != nil {
				return err
			}
		}
		duplicates, e := Rows(tx, "SELECT owner,count(*) AS count FROM managed_containers WHERE owner<>'' GROUP BY owner HAVING count(*)>1")
		if e != nil {
			return e
		}
		if len(duplicates) > 0 {
			return fmt.Errorf("每用户每节点最多一个容器；请先处理重复归属: %v", duplicates)
		}
		if _, err = tx.Exec("CREATE UNIQUE INDEX managed_container_owner ON managed_containers(owner) WHERE owner<>''"); err != nil {
			return err
		}
	}
	if exists["owners"] {
		duplicates, e := Rows(tx, "SELECT owner,count(*) AS count FROM owners WHERE owner<>'' GROUP BY owner HAVING count(*)>1")
		if e != nil {
			return e
		}
		if len(duplicates) > 0 {
			return fmt.Errorf("每用户每节点最多一个容器；请先处理重复归属: %v", duplicates)
		}
		if _, err = tx.Exec("CREATE UNIQUE INDEX owners_one_container ON owners(owner) WHERE owner<>''"); err != nil {
			return err
		}
	}
	return InstallContainerOwnership(tx)
}
