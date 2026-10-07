package updates

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"project-alpha/internal/httpapi"
)

func (m *Manager) Dispatch(w http.ResponseWriter, r *http.Request) (int, any, error) {
	switch {
	case r.URL.Path == Path+"/health" && r.Method == "GET":
		return 200, m.Health(), nil
	case r.URL.Path == Path+"/settings" && r.Method == "GET":
		return 200, m.Settings(), nil
	case r.URL.Path == Path+"/settings" && r.Method == "PUT":
		var input struct {
			Revision int    `json:"revision"`
			Config   Config `json:"config"`
		}
		if err := decodeMessage(w, r, &input); err != nil {
			return 0, nil, err
		}
		if err := m.Save(input.Revision, input.Config); err != nil {
			return 0, nil, err
		}
		return 200, m.Settings(), nil
	case r.URL.Path == Path+"/update" && r.Method == "POST":
		if err := m.Trigger(""); err != nil {
			return 0, nil, err
		}
		return 202, map[string]bool{"accepted": true}, nil
	case r.URL.Path == Path+"/release" && r.Method == "POST":
		var release Release
		// Stable management messages deliberately tolerate additive fields.
		if err := decodeMessage(w, r, &release); err != nil {
			return 0, nil, httpapi.NewError(400, "release 通知无效")
		}
		if err := m.Receive(release); err != nil {
			return 0, nil, err
		}
		if err := m.Automatic(); err != nil {
			return 0, nil, err
		}
		return 200, map[string]bool{"accepted": true}, nil
	default:
		return 0, nil, httpapi.NewError(404, "管理接口不存在")
	}
}

// Webhook acknowledges only after persisting the release. Hub's existing
// heartbeat transports pending notifications and retries after reconnection.
func (m *Manager) Webhook(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if r.Method != "POST" {
		return 0, nil, httpapi.NewError(405, "仅支持 POST")
	}
	m.mu.Lock()
	secret := m.state.Config.WebhookSecret
	repo := m.state.Config.Repo
	m.mu.Unlock()
	if secret == "" {
		return 0, nil, httpapi.NewError(404, "GitHub webhook 未启用")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return 0, nil, httpapi.NewError(413, "webhook 超过大小限制")
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	supplied, e := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	if e != nil || !strings.HasPrefix(signature, "sha256=") || !hmac.Equal(supplied, mac.Sum(nil)) {
		return 0, nil, httpapi.NewError(401, "GitHub webhook 签名无效")
	}
	if r.Header.Get("X-GitHub-Event") == "ping" {
		return 200, map[string]bool{"ok": true}, nil
	}
	if r.Header.Get("X-GitHub-Event") != "release" {
		return 200, map[string]bool{"ignored": true}, nil
	}
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Release struct {
			Tag        string    `json:"tag_name"`
			Draft      bool      `json:"draft"`
			Prerelease bool      `json:"prerelease"`
			Published  time.Time `json:"published_at"`
		} `json:"release"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0, nil, httpapi.NewError(400, "GitHub webhook JSON 无效")
	}
	if payload.Repository.FullName != repo {
		return 0, nil, httpapi.NewError(403, "webhook 仓库不匹配")
	}
	if payload.Action != "published" || payload.Release.Draft {
		return 200, map[string]bool{"ignored": true}, nil
	}
	notice := Release{Delivery: r.Header.Get("X-GitHub-Delivery"), Repo: repo, Tag: payload.Release.Tag, Prerelease: payload.Release.Prerelease, Published: payload.Release.Published}
	if err := m.Receive(notice); err != nil {
		return 0, nil, err
	}
	return 202, map[string]bool{"accepted": true}, nil
}

func decodeMessage(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := d.Decode(out); err != nil {
		return httpapi.NewError(400, "管理消息 JSON 无效或超过大小限制")
	}
	if d.Decode(new(any)) != io.EOF {
		return httpapi.NewError(400, "管理消息包含多余内容")
	}
	return nil
}
