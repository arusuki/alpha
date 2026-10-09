package cluster

import (
	"context"
	"log"
	"sync"
	"time"

	"project-alpha/internal/platform"
)

// This is a work queue consumer, independent of browser sessions and health checks.
func (h *Control) initScheduledAnalysis() {
	ctx, cancel := context.WithCancel(context.Background())
	h.analysisCancel, h.analysisDone = cancel, make(chan struct{})
	go func() {
		defer close(h.analysisDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.consumeScheduledAnalyses(ctx)
			}
		}
	}()
}

func (h *Control) consumeScheduledAnalyses(ctx context.Context) {
	nodes, err := h.nodes("worker")
	if err != nil {
		log.Printf("scheduled analysis queue: %v", err)
		return
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, node := range nodes {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := h.consumeScheduledAnalysis(ctx, node); err != nil && ctx.Err() == nil {
				log.Printf("scheduled analysis %s: %v", node.ID, err)
			}
		}()
	}
	wg.Wait()
}

func (h *Control) consumeScheduledAnalysis(ctx context.Context, node Node) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var queue struct {
		Jobs []struct {
			ID     string `json:"id"`
			UserID string `json:"analysis_user_id"`
		} `json:"jobs"`
	}
	service := platform.User{ID: h.identity, Username: "scheduled-analysis", Role: "admin"}
	if err := h.call(ctx, node, "GET", "/api/worker/scheduled-analysis", nil, service, &queue); err != nil {
		return err
	}
	for _, job := range queue.Jobs {
		gate := h.nodeGate(node.ID)
		gate.Lock()
		done, err := h.advanceScheduledAnalysis(node, job.ID, job.UserID)
		gate.Unlock()
		if !done {
			return err
		}
		status, message := "completed", ""
		if err != nil {
			status, message = "failed", err.Error()
			if len(message) > 3900 {
				message = string([]rune(message)[:min(len([]rune(message)), 1000)])
			}
		}
		if err := h.call(ctx, node, "POST", "/api/worker/scheduled-analysis", map[string]any{"id": job.ID, "status": status, "error": message}, service, &map[string]any{}); err != nil {
			return err
		}
	}
	return nil
}

func (h *Control) advanceScheduledAnalysis(node Node, jobID, userID string) (bool, error) {
	var busy int
	if err := h.DB.SQL.QueryRow(`SELECT
(SELECT count(*) FROM agent_sessions WHERE node_id=? AND status IN ('queued','scanning','running','cancelling')) +
(SELECT count(*) FROM agent_cleanups c JOIN agent_reports r ON r.message_id=c.report_id JOIN agent_messages m ON m.id=r.message_id JOIN agent_sessions s ON s.id=m.session_id WHERE s.node_id=? AND c.status IN ('running','cancelling'))`, node.ID, node.ID).Scan(&busy); err != nil {
		return false, err
	}
	if busy > 0 {
		return false, nil
	}
	user, err := h.agentUser(node.ID, userID)
	if err != nil {
		return true, err
	}
	handler, err := h.agentHandler(node, user)
	if err != nil {
		return false, err
	}
	return handler.Manager.AdvanceScheduled(jobID, user.ID, user.Username)
}
