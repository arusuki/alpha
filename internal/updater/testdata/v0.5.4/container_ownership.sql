-- Source: v0.5.4 internal/platform/container_ownership.sql (schema 37).
CREATE TRIGGER managed_owner_insert BEFORE INSERT ON managed_containers WHEN NEW.owner<>'' BEGIN
  SELECT RAISE(ABORT,'该使用者在此 node 已有容器') WHERE EXISTS(SELECT 1 FROM owners WHERE owner=NEW.owner AND container_id<>NEW.id);
 END;
 CREATE TRIGGER managed_owner_update BEFORE UPDATE OF owner ON managed_containers BEGIN
  SELECT RAISE(ABORT,'容器已领养，请先回收使用者资源') WHERE EXISTS(SELECT 1 FROM member_container_slots WHERE deleted=0 AND container_id=NEW.id AND username<>NEW.owner);
  SELECT RAISE(ABORT,'该使用者在此 node 已有容器') WHERE NEW.owner<>'' AND EXISTS(SELECT 1 FROM owners WHERE owner=NEW.owner AND container_id<>NEW.id);
 END;
 CREATE TRIGGER overlay_owner_insert BEFORE INSERT ON owners BEGIN
  SELECT RAISE(ABORT,'该使用者在此 node 已有容器') WHERE NEW.owner<>'' AND EXISTS(SELECT 1 FROM managed_containers WHERE owner=NEW.owner AND id<>NEW.container_id);
  SELECT RAISE(ABORT,'容器已领养，请先回收使用者资源') WHERE EXISTS(SELECT 1 FROM member_container_slots WHERE deleted=0 AND container_id=NEW.container_id AND username<>NEW.owner);
 END;
 CREATE TRIGGER overlay_owner_update BEFORE UPDATE OF owner ON owners BEGIN
  SELECT RAISE(ABORT,'该使用者在此 node 已有容器') WHERE NEW.owner<>'' AND EXISTS(SELECT 1 FROM managed_containers WHERE owner=NEW.owner AND id<>NEW.container_id);
  SELECT RAISE(ABORT,'容器已领养，请先回收使用者资源') WHERE EXISTS(SELECT 1 FROM member_container_slots WHERE deleted=0 AND container_id=NEW.container_id AND username<>NEW.owner);
 END;
 CREATE TRIGGER sync_overlay_insert AFTER INSERT ON owners BEGIN
  UPDATE managed_containers SET owner=NEW.owner WHERE id=NEW.container_id AND owner<>NEW.owner;
 END;
 CREATE TRIGGER sync_overlay_update AFTER UPDATE OF owner ON owners BEGIN
  UPDATE managed_containers SET owner=NEW.owner WHERE id=NEW.container_id AND owner<>NEW.owner;
 END;
 CREATE TRIGGER sync_managed_insert AFTER INSERT ON managed_containers BEGIN
  INSERT INTO owners(container_id,owner) VALUES(NEW.id,NEW.owner) ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner WHERE owner<>excluded.owner;
 END;
 CREATE TRIGGER sync_managed_update AFTER UPDATE OF owner ON managed_containers BEGIN
  INSERT INTO owners(container_id,owner) VALUES(NEW.id,NEW.owner) ON CONFLICT(container_id) DO UPDATE SET owner=excluded.owner WHERE owner<>excluded.owner;
 END;
