package process

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Docker names are independent of disk scans. Cache the inexpensive list across
// readers so each browser's process polling does not spawn a Docker command.
type containerNames struct {
	mu     sync.Mutex
	next   time.Time
	values map[string]string
	read   func(context.Context) (map[string]string, error)
}

func readDockerNames(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}")
	cmd.WaitDelay = 100 * time.Millisecond
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			names[fields[0]] = fields[1]
		}
	}
	return names, nil
}

func (n *containerNames) apply(ctx context.Context, containers []*Container) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if time.Now().After(n.next) {
		if names, err := n.read(ctx); err == nil {
			n.values = names
		}
		n.next = time.Now().Add(30 * time.Second)
	}
	for _, container := range containers {
		id := container.ID
		if _, value, ok := strings.Cut(id, "://"); ok {
			id = value
		}
		if name := n.values[id]; name != "" {
			container.Name = name
			continue
		}
		// Tetragon can report a truncated Docker ID. Never guess if that prefix
		// identifies more than one container, even when their names coincide.
		name, matches := "", 0
		if len(id) < 12 {
			continue
		}
		for full, candidate := range n.values {
			if strings.HasPrefix(full, id) {
				name, matches = candidate, matches+1
			}
		}
		if matches == 1 {
			container.Name = name
		}
	}
}
