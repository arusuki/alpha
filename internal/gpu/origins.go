package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"project-alpha/internal/containerorigin"
)

// Creation provenance comes only from the root-owned management socket. Client
// labels, environment variables and the shared daemon UID are never owners.
func readContainerOrigins(ctx context.Context, devices []Device, owners map[string]string, socket string) string {
	processes := map[string][]*Process{}
	ids := []string{}
	for i := range devices {
		for j := range devices[i].Processes {
			p := &devices[i].Processes[j]
			if p.Owner != rootlessOwner || !containerorigin.ValidID(p.ContainerID) {
				continue
			}
			if len(processes[p.ContainerID]) == 0 {
				ids = append(ids, p.ContainerID)
			}
			processes[p.ContainerID] = append(processes[p.ContainerID], p)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for start := 0; start < len(ids); start += 128 {
		end := min(start+128, len(ids))
		query := url.Values{"container_id": ids[start:end]}
		records, err := fetchOrigins(ctx, client, "http://rootless"+containerorigin.Endpoint+"?"+query.Encode())
		if err != nil {
			return "rootless Docker 创建来源审计不可读，未确认的归属保留为 rootless Docker：" + err.Error()
		}
		for _, record := range records {
			if record.Source.Error != "" || record.Source.PID <= 0 || record.Source.StartTicks == 0 {
				continue
			}
			if owner := owners[record.Source.ContainerID]; owner != "" {
				for _, p := range processes[record.ContainerID] {
					p.Owner = owner
				}
			}
		}
	}
	return ""
}

func fetchOrigins(ctx context.Context, client *http.Client, endpoint string) ([]containerorigin.Record, error) {
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
	var result containerorigin.Response
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err = decoder.Decode(&result); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || result.Version != containerorigin.Version || result.Origins == nil || len(result.Origins) > 128 {
		return nil, fmt.Errorf("创建来源响应格式无效")
	}
	seen := map[string]bool{}
	for _, record := range result.Origins {
		if record.Version != containerorigin.Version || !containerorigin.ValidID(record.ContainerID) || seen[record.ContainerID] || (record.Source.ContainerID != "" && !containerorigin.ValidID(record.Source.ContainerID)) {
			return nil, fmt.Errorf("创建来源字段无效")
		}
		seen[record.ContainerID] = true
	}
	return result.Origins, nil
}
