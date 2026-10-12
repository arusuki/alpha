package storage

import (
	"context"
	"fmt"
	"os"

	"project-alpha/internal/containerorigin"
	"project-alpha/internal/httpapi"
)

type rootlessSocketKey struct{}

// Docker retains the primary engine used by the scan helper, plus the optional
// managed rootless engine. All physical resources enter the same inode ledger.
func dockerEngines(metadata object) []object {
	engines := []object{}
	if len(metadata) != 0 {
		engines = append(engines, metadata)
	}
	if rootless, ok := metadata["rootless"].(map[string]any); ok {
		engines = append(engines, rootless)
	}
	return engines
}

func discoverStorage(ctx context.Context, c Config, command dockerCommand, progress func(object) error) (object, []Container, []Resource, []Warning, error) {
	metadata, containers, resources, warnings, err := discoverWithProgress(ctx, c.OwnerLabel, c.DockerTimeout, command, progress)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	socket, _ := ctx.Value(rootlessSocketKey{}).(string)
	if socket == "" {
		socket = containerorigin.DefaultSocket
	}
	if _, err := os.Stat(socket); os.IsNotExist(err) {
		return metadata, containers, resources, warnings, nil
	} else if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("读取 rootless Docker 管理套接字: %w", err)
	}
	endpoint := "unix://" + socket
	rootless, children, childResources, childWarnings, err := discoverEngine(ctx, c.OwnerLabel, c.DockerTimeout, command, progress, endpoint)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("发现 rootless Docker 磁盘资源: %w", err)
	}
	parents, auditErr := containerorigin.ReadParents(ctx, socket, containerIDs(children))
	if auditErr != nil {
		warnings = append(warnings, Warning{socket, "rootless Docker 创建来源审计不可读，未确认的归属保留为 rootless Docker：" + auditErr.Error()})
	}
	for i := range children {
		children[i].Rootless = true
		children[i].Owner = containerorigin.RootlessOwner
		children[i].OriginContainerID = parents[children[i].ID]
	}
	rootless["is_rootless"] = true
	if rootless["id"] == metadata["id"] {
		// The primary context may already address this daemon through another socket.
		metadata = rootless
		containers, resources = children, childResources
	} else {
		ids := map[string]bool{}
		for _, row := range containers {
			ids[row.ID] = true
		}
		for _, row := range children {
			if ids[row.ID] {
				return nil, nil, nil, nil, fmt.Errorf("不同 Docker 引擎返回相同容器 ID：%s", row.ID)
			}
		}
		metadata["rootless"] = rootless
		containers = append(containers, children...)
		resources = append(resources, childResources...)
	}
	warnings = append(warnings, childWarnings...)
	return metadata, containers, mergeResources(resources), warnings, nil
}

func containerIDs(rows []Container) []string {
	ids := make([]string, 0, len(rows))
	for _, c := range rows {
		ids = append(ids, c.ID)
	}
	return ids
}

func mergeResources(rows []Resource) []Resource {
	merged := []Resource{}
	index := map[string]int{}
	for _, r := range rows {
		i, ok := index[r.Path]
		if !ok {
			index[r.Path] = len(merged)
			merged = append(merged, r)
			continue
		}
		for _, k := range r.Kinds {
			merged[i].Kinds = appendUnique(merged[i].Kinds, k)
		}
		for _, id := range r.Containers {
			merged[i].Containers = appendUnique(merged[i].Containers, id)
		}
	}
	return merged
}

// ApplyContainerOwners resolves snapshots and inventory against current explicit ownership.
func ApplyContainerOwners(rows []Container, owners map[string]string) {
	parents := map[string]string{}
	for _, c := range rows {
		if c.Rootless {
			parents[c.ID] = c.OriginContainerID
		}
	}
	for i := range rows {
		c := &rows[i]
		if c.Rootless {
			c.Owner = containerorigin.RootlessOwner
			if owner := containerorigin.Resolve(c.ID, owners, parents); owner != "" {
				c.Owner = owner
			}
			continue
		}
		if owner, ok := owners[c.ID]; ok {
			c.Owner = owner
		}
	}
}

func snapshotDockerRoots(s *Snapshot) []string {
	roots := []string{}
	for _, engine := range dockerEngines(s.Docker) {
		for _, key := range []string{"root", "root_canonical"} {
			if p := httpapi.String(engine[key]); p != "" {
				roots = appendUnique(roots, p)
			}
		}
	}
	return roots
}
