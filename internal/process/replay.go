package process

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"google.golang.org/protobuf/encoding/protojson"
)

// Replay feeds a captured Tetragon event dump through the same builder the
// live agent uses. It exists so the forest logic can be exercised, and
// regressions reproduced, without a running agent.
//
// The dump is either newline-delimited JSON objects, which is what
// `tetra getevents -o json` writes, or a single JSON array of those objects.
// Field names follow protojson, so the agent's own output parses directly.
type Replay struct {
	data []byte
}

// OpenReplay reads a dump from a file, or from standard input when path is "-".
func OpenReplay(path string) (*Replay, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		return &Replay{data: data}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &Replay{data: data}, nil
}

func (r *Replay) Bootstrap(context.Context) ([]*tetragon.Process, error) {
	return nil, ErrNoBootstrap
}

func (r *Replay) Stream(ctx context.Context, handle func(*tetragon.GetEventsResponse) error) error {
	records, err := splitEvents(r.data)
	if err != nil {
		return err
	}
	options := protojson.UnmarshalOptions{DiscardUnknown: true}
	for i, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		var resp tetragon.GetEventsResponse
		if err := options.Unmarshal(record, &resp); err != nil {
			return fmt.Errorf("event %d: %w", i+1, err)
		}
		if err := handle(&resp); err != nil {
			return err
		}
	}
	return nil
}

func (r *Replay) Close() error { return nil }

func splitEvents(data []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var array []json.RawMessage
		if err := json.Unmarshal(trimmed, &array); err != nil {
			return nil, fmt.Errorf("parse event array: %w", err)
		}
		return array, nil
	}
	records := []json.RawMessage{}
	for _, line := range bytes.Split(trimmed, []byte{'\n'}) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		records = append(records, json.RawMessage(line))
	}
	return records, nil
}
