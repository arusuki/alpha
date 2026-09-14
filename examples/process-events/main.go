// Generate the Tetragon process-event fixture that exercises the container
// process forest. The output is newline-delimited protojson, the same shape
// `tetra getevents -o json` prints, so it can be replayed with
// `project-alpha process --events-file`.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	tetragon "github.com/cilium/tetragon/api/v1/tetragon"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	node       = "gpu-node-01"
	containerA = "3f2a9c8b1d4e"
	containerB = "b7c1e0f2a9d3"
)

var base = time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)

type process struct {
	label, binary, arguments, container string
	pid                                 uint32
	offset                              time.Duration
	parent                              *process
}

func (p *process) execID() string {
	return fmt.Sprintf("%s:%d:%d", node, p.pid, base.Add(p.offset).UnixNano())
}

func (p *process) proto() *tetragon.Process {
	parentExec := ""
	if p.parent != nil {
		parentExec = p.parent.execID()
	}
	return &tetragon.Process{
		ExecId:       p.execID(),
		Pid:          wrapperspb.UInt32(p.pid),
		Uid:          wrapperspb.UInt32(0),
		Cwd:          "/workspace",
		Binary:       p.binary,
		Arguments:    p.arguments,
		StartTime:    timestamppb.New(base.Add(p.offset)),
		ParentExecId: parentExec,
		Docker:       p.container,
	}
}

func execEvent(p *process) *tetragon.GetEventsResponse {
	return &tetragon.GetEventsResponse{Event: &tetragon.GetEventsResponse_ProcessExec{ProcessExec: &tetragon.ProcessExec{Process: p.proto()}}}
}

func exitEvent(p *process, status uint32) *tetragon.GetEventsResponse {
	return &tetragon.GetEventsResponse{Event: &tetragon.GetEventsResponse_ProcessExit{ProcessExit: &tetragon.ProcessExit{Process: p.proto(), Status: status}}}
}

func main() {
	output := flag.String("output", "tests/fixtures/tetragon-events.jsonl", "Fixture output")
	flag.Parse()

	// The host-side container runtime is the parent of each container's init
	// process, exactly as Tetragon observes it.
	dockerd := &process{label: "dockerd", binary: "/usr/bin/dockerd", arguments: "-H fd://", pid: 1200}
	shim := &process{label: "shim", binary: "/usr/bin/containerd-shim-runc-v2", arguments: "-namespace moby", pid: 1300, offset: time.Second, parent: dockerd}

	train := &process{label: "train-init", binary: "/opt/nvidia/nvidia_entrypoint.sh", container: containerA, pid: 1, offset: 2 * time.Second, parent: shim}
	python := &process{label: "python", binary: "/usr/bin/python3", arguments: "train.py --epochs 20", container: containerA, pid: 42, offset: 3 * time.Second, parent: train}
	smi := &process{label: "nvidia-smi", binary: "/usr/local/bin/nvidia-smi", arguments: "-q -d MEMORY", container: containerA, pid: 77, offset: 4 * time.Second, parent: python}
	apt := &process{label: "apt", binary: "/bin/sh", arguments: "-c 'apt-get update'", container: containerA, pid: 91, offset: 5 * time.Second, parent: train}

	notebook := &process{label: "nb-init", binary: "/bin/bash", arguments: "-l", container: containerB, pid: 1, offset: 2500 * time.Millisecond, parent: shim}
	sshd := &process{label: "sshd", binary: "/usr/sbin/sshd", arguments: "-D -e", container: containerB, pid: 33, offset: 6 * time.Second, parent: notebook}

	// The shell exits during the window, so it must not appear in the forest.
	events := []*tetragon.GetEventsResponse{
		execEvent(dockerd),
		execEvent(shim),
		execEvent(train),
		execEvent(python),
		execEvent(smi),
		execEvent(apt),
		exitEvent(apt, 0),
		execEvent(notebook),
		execEvent(sshd),
	}
	body := []byte{}
	for _, event := range events {
		raw, err := protojson.Marshal(event)
		if err != nil {
			log.Fatal(err)
		}
		body = append(body, raw...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(*output, body, 0600); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d events to %s", len(events), *output)
}
