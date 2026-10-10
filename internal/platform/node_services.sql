CREATE TABLE node_service_mounts (
 container_id TEXT NOT NULL, endpoint TEXT NOT NULL, daemon TEXT NOT NULL,
 owner TEXT NOT NULL, name TEXT NOT NULL, socket_path TEXT NOT NULL,
 error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(endpoint,container_id,socket_path)
);
CREATE TRIGGER service_mount_owner BEFORE UPDATE OF owner ON managed_containers
 WHEN OLD.owner<>NEW.owner BEGIN
 SELECT RAISE(ABORT,'请先在节点服务中取消此容器的服务挂载，再修改使用者')
 WHERE EXISTS(SELECT 1 FROM node_service_mounts WHERE container_id=OLD.id);
END;
CREATE TRIGGER service_mount_release BEFORE DELETE ON managed_containers BEGIN
 SELECT RAISE(ABORT,'请先在节点服务中取消此容器的服务挂载，再删除或解除接管')
 WHERE EXISTS(SELECT 1 FROM node_service_mounts WHERE container_id=OLD.id);
END;
