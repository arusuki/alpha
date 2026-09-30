package cluster

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"project-alpha/internal/agent"
	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

func Initialize(tx *sql.Tx) error {
	if err := agent.Initialize(tx); err != nil {
		return err
	}
	if err := members.Initialize(tx); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE cluster_nodes (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL UNIQUE,
 token TEXT NOT NULL, created_at REAL NOT NULL
 )`)
	if err != nil {
		return err
	}
	if err = initializeProvision(tx); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO service_identity VALUES(1,'control',?)", platform.RandomHex(16))
	return err
}

type Node struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	URL       string  `json:"url"`
	Token     string  `json:"-"`
	CreatedAt float64 `json:"created_at"`
}
type nodeInput struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

func decode(w http.ResponseWriter, r *http.Request, target any) error {
	value, err := httpapi.RequestBody(w, r)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return httpapi.NewError(400, "请求字段格式无效")
	}
	return nil
}
func (v *nodeInput) validate() error {
	v.Name = strings.TrimSpace(v.Name)
	if v.Name == "" || utf8.RuneCountInString(v.Name) > 80 || strings.ContainsAny(v.Name, "\x00\r\n") {
		return httpapi.NewError(400, "节点名称需为 1–80 个字符")
	}
	u, err := url.Parse(strings.TrimSpace(v.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return httpapi.NewError(400, "节点地址需为 http(s)://主机:端口，不含路径、凭据或查询参数")
	}
	u.Path = ""
	v.URL = u.String()
	if !ValidToken(v.Token) {
		return httpapi.NewError(400, "节点令牌需为 32–256 位无空白 ASCII 字符")
	}
	return nil
}
func (h *Control) nodes() ([]Node, error) {
	rows, err := h.DB.SQL.Query("SELECT id,name,url,token,created_at FROM cluster_nodes ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var n Node
		if err = rows.Scan(&n.ID, &n.Name, &n.URL, &n.Token, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func (h *Control) node(id string) (Node, error) {
	var n Node
	err := h.DB.SQL.QueryRow("SELECT id,name,url,token,created_at FROM cluster_nodes WHERE id=?", id).Scan(&n.ID, &n.Name, &n.URL, &n.Token, &n.CreatedAt)
	if err == sql.ErrNoRows {
		return n, httpapi.NewError(404, "节点不存在")
	}
	return n, err
}
