package rootless

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--internal-worker" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}
func TestConfigurationWorkerReexec(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("configuration workers must run as a non-root account")
	}
	_, u := testManager(t)
	// Execute the real re-exec protocol with the current unprivileged account.
	// Production starts this same worker with docker-rootless credentials.
	if err := runWorker(workerRequest{Action: "prepare", User: u}, false); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, u)
	if cfg["data-root"] != layout(u).Data {
		t.Fatal(cfg)
	}
	if err := runWorker(workerRequest{Action: "show-proxy", User: u}, false); err != nil {
		t.Fatal(err)
	}
	bad := u
	bad.UID++
	if err := runWorker(workerRequest{Action: "prepare", User: bad}, false); err == nil {
		t.Fatal("worker accepted mismatched credentials")
	}
	if err := runWorker(workerRequest{Action: "unknown", User: u}, false); err == nil {
		t.Fatal("worker accepted unknown operation")
	}
}
