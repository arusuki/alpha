package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/platform"
)

// Members use the same read-only GPU overview as the management panel, including
// process ownership and history. The destination and operation remain fixed.
func (h *Control) memberGPU(r *http.Request, user platform.User) (int, any, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, nil, httpapi.NewError(400, "GPU 查询参数无效")
	}
	for key, values := range query {
		if len(values) != 1 || (key != "node_id" && key != "hours" && key != "step" && key != "end") {
			return 0, nil, httpapi.NewError(400, "GPU 查询参数无效")
		}
	}
	id := query.Get("node_id")
	if !identifier.MatchString(id) {
		return 0, nil, httpapi.NewError(400, "请选择有效的计算节点")
	}
	node, err := h.node(id)
	if err != nil {
		return 0, nil, err
	}
	if node.Kind != "worker" {
		return 0, nil, httpapi.NewError(400, "GPU 查询仅支持计算节点")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	info, err := h.probe(ctx, node)
	if err != nil {
		return 0, nil, httpapi.NewError(502, "节点不可达，暂时无法查询 GPU")
	}
	if info.ID != node.ID || info.Protocol != Protocol {
		return 0, nil, httpapi.NewError(409, "节点身份或业务协议不匹配，请联系管理员")
	}
	query.Del("node_id")
	var overview json.RawMessage
	if err := h.call(ctx, node, "GET", "/api/gpu/overview?"+query.Encode(), nil, user, &overview); err != nil {
		return 0, nil, err
	}
	return 200, overview, nil
}
