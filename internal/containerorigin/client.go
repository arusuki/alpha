package containerorigin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const RootlessOwner = "rootless Docker"
const DefaultSocket = "/run/rootless-docker/control.sock"

// ReadParents only trusts the host management socket, never client labels or UID.
// Return no partial attribution if any response is invalid or unavailable.
func ReadParents(ctx context.Context, socket string, ids []string) (map[string]string, error) {
	parents := map[string]string{}
	if len(ids) == 0 {
		return parents, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	unique := []string{}
	requested := map[string]bool{}
	for _, id := range ids {
		if !ValidID(id) {
			return nil, fmt.Errorf("容器 ID 无效")
		}
		if !requested[id] {
			unique = append(unique, id)
			requested[id] = true
		}
	}
	for start := 0; start < len(unique); start += 128 {
		batch := unique[start:min(start+128, len(unique))]
		query := url.Values{"container_id": batch}
		records, err := fetchOrigins(ctx, client, "http://rootless"+Endpoint+"?"+query.Encode())
		if err != nil {
			return nil, err
		}
		batchIDs := map[string]bool{}
		for _, id := range batch {
			batchIDs[id] = true
		}
		for _, r := range records {
			if !batchIDs[r.ContainerID] {
				return nil, fmt.Errorf("创建来源响应包含未请求的容器")
			}
			if r.Source.Error == "" && r.Source.PID > 0 && r.Source.StartTicks > 0 && ValidID(r.Source.ContainerID) {
				parents[r.ContainerID] = r.Source.ContainerID
			}
		}
	}
	return parents, nil
}

// Resolve follows trusted creation edges to current explicit ownership. Empty
// overrides stop inheritance, and missing/cyclic evidence never invents an owner.
func Resolve(id string, owners, parents map[string]string) string {
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		if owner, ok := owners[id]; ok {
			return owner
		}
		seen[id] = true
		id = parents[id]
	}
	return ""
}

func fetchOrigins(ctx context.Context, client *http.Client, endpoint string) ([]Record, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("管理接口返回 HTTP %d，请检查 rootless-docker 服务与审计记录", response.StatusCode)
	}
	var result Response
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err = decoder.Decode(&result); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || result.Version != Version || result.Origins == nil || len(result.Origins) > 128 {
		return nil, fmt.Errorf("创建来源响应格式无效")
	}
	seen := map[string]bool{}
	for _, record := range result.Origins {
		if record.Version != Version || !ValidID(record.ContainerID) || seen[record.ContainerID] || (record.Source.ContainerID != "" && !ValidID(record.Source.ContainerID)) {
			return nil, fmt.Errorf("创建来源字段无效")
		}
		seen[record.ContainerID] = true
	}
	return result.Origins, nil
}
