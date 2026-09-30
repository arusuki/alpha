package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"

	"project-alpha/internal/httpapi"
)

type Invite struct {
	ID         string `json:"id"`
	URL        string `json:"inviteUrl"`
	Created    string `json:"created"`
	Accepted   bool   `json:"accepted"`
	MultiUse   bool   `json:"multiUse"`
	AcceptedBy struct {
		ID        json.Number `json:"id"`
		LoginName string      `json:"loginName"`
	} `json:"acceptedBy"`
}

var remoteID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (h *Handler) invoke(ctx context.Context, method, path string, body any, out any) error {
	_, ciphertext, err := h.settings()
	if err != nil {
		return err
	}
	if ciphertext == "" {
		return httpapi.NewError(409, "请先配置 Tailscale API Key")
	}
	token, err := h.decrypt(ciphertext)
	if err != nil {
		return err
	}
	var b []byte
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return httpapi.NewError(502, "Tailscale 请求未确认，请核对分享记录")
	}
	defer resp.Body.Close()
	if method == "DELETE" && resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &upstreamError{resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes || json.Unmarshal(raw, out) != nil {
		return httpapi.NewError(502, "Tailscale 响应无效，请核对分享记录")
	}
	return nil
}
func (h *Handler) Devices(ctx context.Context) ([]Device, error) {
	s, c, err := h.settings()
	if err != nil {
		return nil, err
	}
	if c == "" {
		return nil, httpapi.NewError(409, "请先保存 Tailscale API Key")
	}
	token, err := h.decrypt(c)
	if err != nil {
		return nil, err
	}
	return h.listDevices(ctx, s, token)
}
func (h *Handler) Invites(ctx context.Context, device string) ([]Invite, error) {
	if !remoteID.MatchString(device) {
		return nil, fmt.Errorf("无效的 Tailscale 节点 ID")
	}
	var out []Invite
	err := h.invoke(ctx, "GET", "/device/"+url.PathEscape(device)+"/device-invites", nil, &out)
	if err == nil && out == nil {
		err = fmt.Errorf("Tailscale 分享列表无效")
	}
	return out, err
}
func (h *Handler) CreateInvite(ctx context.Context, device string) (Invite, error) {
	if !remoteID.MatchString(device) {
		return Invite{}, fmt.Errorf("无效的 Tailscale 节点 ID")
	}
	var out []Invite
	err := h.invoke(ctx, "POST", "/device/"+url.PathEscape(device)+"/device-invites", []map[string]bool{{"multiUse": false, "allowExitNode": false}}, &out)
	if err != nil {
		return Invite{}, err
	}
	if len(out) != 1 || !ValidInvite(out[0]) {
		return Invite{}, fmt.Errorf("Tailscale 分享响应无效，请核对邀请")
	}
	return out[0], nil
}
func ValidInvite(v Invite) bool {
	u, e := url.Parse(v.URL)
	return remoteID.MatchString(v.ID) && !v.MultiUse && e == nil && u.Scheme == "https" && u.Host == "login.tailscale.com" && u.User == nil
}
func (h *Handler) DeleteInvite(ctx context.Context, id string) error {
	if !remoteID.MatchString(id) {
		return fmt.Errorf("无效的邀请 ID")
	}
	path := "/device-invites/" + url.PathEscape(id)
	err := h.invoke(ctx, "DELETE", path, nil, nil)
	var remote *upstreamError
	if !errors.As(err, &remote) || remote.status != http.StatusBadRequest {
		return err
	}
	// Revoking an invite in the console can make DELETE return 400. Only a
	// successful absence check makes that response safe to treat as completion.
	var invite Invite
	checkErr := h.invoke(ctx, "GET", path, nil, &invite)
	if errors.As(checkErr, &remote) && remote.status == http.StatusNotFound {
		return nil
	}
	if checkErr != nil {
		return fmt.Errorf("撤销邀请返回 HTTP 400，无法确认邀请已不存在: %w", checkErr)
	}
	return err
}

type upstreamError struct{ status int }

func (e *upstreamError) Error() string {
	return fmt.Sprintf("Tailscale API 返回 HTTP %d，请检查凭据、权限和分享记录", e.status)
}
func CreationRejected(err error) bool {
	var remote *upstreamError
	if errors.As(err, &remote) {
		return remote.status >= 400 && remote.status < 500
	}
	var api *httpapi.Error
	return errors.As(err, &api) && api.Status == 409
}
