package process

import (
	"context"
	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"google.golang.org/grpc"
	"net"
	"path/filepath"
	"testing"
	"time"
)

type delayedTetragon struct {
	tetragon.UnimplementedFineGuidanceSensorsServer
}

func (delayedTetragon) GetDebug(context.Context, *tetragon.GetDebugRequest) (*tetragon.GetDebugResponse, error) {
	return &tetragon.GetDebugResponse{}, nil
}
func TestTetragonCanStartAfterWorker(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "tetragon.sock")
	source, err := Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = source.Bootstrap(ctx); err == nil {
		t.Fatal("absent socket succeeded")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	tetragon.RegisterFineGuidanceSensorsServer(server, delayedTetragon{})
	go server.Serve(listener)
	defer server.Stop()
	for {
		if _, err = source.Bootstrap(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("late service never connected", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
