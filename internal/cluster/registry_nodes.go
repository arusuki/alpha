package cluster

import (
	"context"
	"sync"

	"project-alpha/internal/registry"
)

type registryLink struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	status registry.LinkStatus
}

func (h *Control) initRegistryLinks() error {
	identity, err := h.DB.CheckMode("control")
	if err != nil {
		return err
	}
	h.identity = identity
	h.registryLinks = make(map[string]*registryLink)
	nodes, err := h.nodes("registry")
	if err != nil {
		return err
	}
	for _, n := range nodes {
		h.startRegistry(n)
	}
	return nil
}

// Node mutations and shutdown hold nodeMu. Status callbacks only lock their link.
func (h *Control) startRegistry(n Node) {
	h.stopRegistry(n.ID)
	ctx, cancel := context.WithCancel(context.Background())
	link := &registryLink{cancel: cancel, done: make(chan struct{}), status: registry.LinkStatus{State: "connecting"}}
	h.registryMu.Lock()
	h.registryLinks[n.ID] = link
	h.registryMu.Unlock()
	go func() {
		defer close(link.done)
		registry.Connect(ctx, registry.LinkConfig{Address: n.URL, Token: n.Token, ControlID: h.identity, RegistryID: n.ID}, h.RegistryDispatch, func(status registry.LinkStatus) {
			link.mu.Lock()
			link.status = status
			link.mu.Unlock()
			if status.State == "connected" {
				// Reuse the existing link heartbeat to retry offline token delivery.
				h.syncGitHubTokens(ctx)
			}
		})
	}()
}

func (h *Control) stopRegistry(id string) {
	h.registryMu.Lock()
	link := h.registryLinks[id]
	delete(h.registryLinks, id)
	h.registryMu.Unlock()
	if link != nil {
		link.cancel()
		<-link.done
	}
}

func (h *Control) registryStatus(id string) registry.LinkStatus {
	h.registryMu.Lock()
	link := h.registryLinks[id]
	h.registryMu.Unlock()
	if link == nil {
		return registry.LinkStatus{State: "disconnected"}
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	return link.status
}

func (h *Control) closeRegistryLinks() {
	h.nodeMu.Lock()
	defer h.nodeMu.Unlock()
	h.nodesClosed = true
	h.registryMu.Lock()
	ids := make([]string, 0, len(h.registryLinks))
	for id, link := range h.registryLinks {
		ids = append(ids, id)
		link.cancel()
	}
	h.registryMu.Unlock()
	for _, id := range ids {
		h.stopRegistry(id)
	}
}
