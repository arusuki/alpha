package gpu

import (
	"context"

	"project-alpha/internal/containerorigin"
)

func readContainerOrigins(ctx context.Context, devices []Device, owners map[string]string, socket string) string {
	processes := map[string][]*Process{}
	ids := []string{}
	for i := range devices {
		for j := range devices[i].Processes {
			p := &devices[i].Processes[j]
			if p.Owner != rootlessOwner || !containerorigin.ValidID(p.ContainerID) {
				continue
			}
			if len(processes[p.ContainerID]) == 0 {
				ids = append(ids, p.ContainerID)
			}
			processes[p.ContainerID] = append(processes[p.ContainerID], p)
		}
	}
	parents, err := containerorigin.ReadParents(ctx, socket, ids)
	if err != nil {
		return "rootless Docker 创建来源审计不可读，未确认的归属保留为 rootless Docker：" + err.Error()
	}
	for id, list := range processes {
		if owner := containerorigin.Resolve(id, owners, parents); owner != "" {
			for _, p := range list {
				p.Owner = owner
			}
		}
	}
	return ""
}
