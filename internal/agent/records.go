package agent

import (
	"context"
	"encoding/json"
)

// Records is the capability boundary. Model configuration and conversations
// never enter the storage service; tools cannot access its database or worker internals.
type Records interface {
	Wait(ctx context.Context, id string, check func() error) error
	StartOverview(actor string) (map[string]any, error)
	Job(id string) (map[string]any, error)
	Cancel(id, actor string) (map[string]any, error)
	Query(id, operation string, fields map[string]json.RawMessage) (map[string]any, error)
	Explore(actor, id, path string, revision int64, depth int) (map[string]any, error)
}
