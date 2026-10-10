package process

import (
	"context"
	"errors"
	"io"
	"net"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DefaultSocket is the Unix socket the Tetragon agent listens on.
const DefaultSocket = "/var/run/tetragon/tetragon.sock"

// ErrNoBootstrap reports that a source can only deliver live events and cannot
// list the processes that already exist.
var ErrNoBootstrap = errors.New("source cannot list existing processes")

// Source delivers process lifecycle information. Bootstrap is optional: it
// returns ErrNoBootstrap when the source cannot enumerate running processes.
type Source interface {
	Bootstrap(ctx context.Context) ([]*tetragon.Process, error)
	Stream(ctx context.Context, handle func(*tetragon.GetEventsResponse) error) error
	Close() error
}

// Tetragon is a live source backed by the Tetragon gRPC API.
type Tetragon struct {
	conn   *grpc.ClientConn
	client tetragon.FineGuidanceSensorsClient
}

// Dial creates a lazy connection, even when the service has not created its
// socket yet. The resident watcher's existing reconnect loop covers later
// starts and socket replacement without restarting the worker.
func Dial(socket string) (*Tetragon, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	conn, err := grpc.NewClient("passthrough:///tetragon",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Tetragon{conn: conn, client: tetragon.NewFineGuidanceSensorsClient(conn)}, nil
}

// Bootstrap dumps the Tetragon process cache, which is what makes processes
// that started before collection visible in the first snapshot.
func (t *Tetragon) Bootstrap(ctx context.Context) ([]*tetragon.Process, error) {
	resp, err := t.client.GetDebug(ctx, &tetragon.GetDebugRequest{
		Flag: tetragon.ConfigFlag_CONFIG_FLAG_DUMP_PROCESS_CACHE,
		Arg: &tetragon.GetDebugRequest_Dump{Dump: &tetragon.DumpProcessCacheReqArgs{
			SkipZeroRefcnt: true,
		}},
	})
	if err != nil {
		return nil, err
	}
	processes := resp.GetProcesses().GetProcesses()
	out := make([]*tetragon.Process, 0, len(processes))
	for _, internal := range processes {
		if p := internal.GetProcess(); p != nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// Stream subscribes to process execution and exit events.
func (t *Tetragon) Stream(ctx context.Context, handle func(*tetragon.GetEventsResponse) error) error {
	stream, err := t.client.GetEvents(ctx, &tetragon.GetEventsRequest{
		AllowList: []*tetragon.Filter{{
			EventSet: []tetragon.EventType{tetragon.EventType_PROCESS_EXEC, tetragon.EventType_PROCESS_EXIT},
		}},
	})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := handle(resp); err != nil {
			return err
		}
	}
}

func (t *Tetragon) Close() error { return t.conn.Close() }

// Stats records what a collection run observed.
type Stats struct {
	BootstrapOK    bool
	Bootstrapped   int
	BootstrapError string
	Exec           int
	Exit           int
}

// bootstrapInto fills the builder from the source's process cache. A source
// that cannot list existing processes reports ErrNoBootstrap, which is not
// fatal; any other failure is recorded on the stats so a stream-only collection
// still runs.
func bootstrapInto(ctx context.Context, src Source, builder *Builder) Stats {
	var stats Stats
	processes, err := src.Bootstrap(ctx)
	switch {
	case errors.Is(err, ErrNoBootstrap):
	case err != nil:
		stats.BootstrapError = err.Error()
	default:
		stats.BootstrapOK = true
		stats.Bootstrapped = len(processes)
		builder.ObserveAll(processes)
	}
	return stats
}

// streamInto applies lifecycle events until the source ends the stream or
// fails. The caller supplies how one event reaches the tree, so a resident
// watcher can guard the builder while the CLI applies directly. A cancelled
// context ends a live stream cleanly rather than as an error.
func streamInto(ctx context.Context, src Source, apply func(*tetragon.GetEventsResponse), stats Stats) (Stats, error) {
	err := src.Stream(ctx, func(resp *tetragon.GetEventsResponse) error {
		if resp.GetProcessExec() != nil {
			stats.Exec++
		} else if resp.GetProcessExit() != nil {
			stats.Exit++
		}
		apply(resp)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		return stats, err
	}
	return stats, nil
}

// Collect bootstraps the builder from existing processes when the source
// supports it, then applies live lifecycle events. Cancelling ctx ends a live
// stream cleanly; a bootstrap failure is recorded but not fatal.
func Collect(ctx context.Context, src Source, builder *Builder) (Stats, error) {
	stats := bootstrapInto(ctx, src, builder)
	return streamInto(ctx, src, builder.Apply, stats)
}
