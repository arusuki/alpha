package rootless

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type eventWatcher struct {
	cancel context.CancelFunc
	ready  bool // The target endpoint has completed its first reconciliation.
}

// Called with d.mu held. Only endpoints with saved bindings need a stream;
// the rootless stream detects socket replacement through connection EOF.
func (d *daemon) syncWatchers() {
	wanted := map[string]bool{}
	if d.ctx.Err() == nil {
		for _, b := range d.store.Bindings {
			wanted[b.Host] = true
		}
		if len(wanted) > 0 {
			wanted["unix://"+layout(d.user).Socket] = true
		}
	}
	for host, watcher := range d.watchers {
		if !wanted[host] {
			watcher.cancel()
			delete(d.watchers, host)
		}
	}
	if d.watchers == nil {
		d.watchers = map[string]*eventWatcher{}
	}
	for host := range wanted {
		if d.watchers[host] != nil {
			continue
		}
		ctx, cancel := context.WithCancel(d.ctx)
		watcher := &eventWatcher{cancel: cancel}
		d.watchers[host] = watcher
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.watchEvents(ctx, host, watcher)
		}()
	}
}

// Subscribe before inspecting current state, so events during reconciliation
// stay buffered in the stream. On reconnect, inspecting current state also
// covers any gap without replaying duplicate historical events.
func (d *daemon) watchEvents(ctx context.Context, host string, watcher *eventWatcher) {
	transport := unixTransport(strings.TrimPrefix(host, "unix://"))
	transport.ResponseHeaderTimeout = 10 * time.Second
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	rootless := host == "unix://"+layout(d.user).Socket
	filters := map[string][]string{"type": {"container"}, "event": {"start", "unpause", "die", "destroy"}}
	if rootless {
		// Only stream lifetime matters here; do not consume workload events.
		filters = map[string][]string{"type": {"daemon"}}
	}
	encoded, _ := json.Marshal(filters)
	endpoint := "http://docker/events?" + url.Values{"filters": {string(encoded)}}.Encode()
	for ctx.Err() == nil {
		err := d.consumeEvents(ctx, client, endpoint, host, rootless, watcher)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Docker 事件连接断开：", host, err)
		}
		// Retry only disconnected streams; no periodic state/health polling.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (d *daemon) consumeEvents(ctx context.Context, client *http.Client, endpoint, host string, rootless bool, watcher *eventWatcher) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	d.mu.Lock()
	if ctx.Err() == nil {
		if rootless {
			err = d.reconcileSocket()
		} else {
			err = d.reconcile(host, "")
		}
		watcher.ready = true
		if err != nil {
			fmt.Fprintln(os.Stderr, "事件连接后恢复挂载：", err)
		}
	}
	d.mu.Unlock()
	decoder := json.NewDecoder(response.Body)
	for {
		var event struct {
			Action string
			Actor  struct{ ID string }
		}
		if err = decoder.Decode(&event); err != nil {
			return err
		}
		if rootless || event.Actor.ID == "" {
			continue
		}
		switch event.Action {
		case "start", "unpause", "die", "destroy":
			d.mu.Lock()
			if ctx.Err() == nil {
				if err = d.reconcile(host, event.Actor.ID); err != nil {
					fmt.Fprintln(os.Stderr, "容器事件恢复挂载：", err)
				}
			}
			d.mu.Unlock()
		}
	}
}

func (d *daemon) reconcileSocket() error {
	source, err := socketIdentity(layout(d.user).Socket)
	if err != nil {
		return err
	}
	return d.reconcileBindings(func(b binding) bool {
		// The target subscription owns initial recovery. After that, only a
		// changed/missing receipt needs work on rootless reconnection. This
		// also avoids repeating a successful client-triggered restart.
		watcher := d.watchers[b.Host]
		return watcher != nil && watcher.ready && (b.Receipt == nil || b.Receipt.Device != uint64(source.Dev) || b.Receipt.Inode != source.Ino)
	})
}
