package assistant

import (
	"database/sql"
	"net/http"

	"project-alpha/internal/httpapi"
	"project-alpha/internal/members"
	"project-alpha/internal/platform"
)

type rowQuerier interface{ QueryRow(string, ...any) *sql.Row }

// Table names and model identity are chosen by the server, never the client.
type access struct {
	user                    platform.User
	key, sessions, memories string
	authorize               func(rowQuerier) error
}

func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, user platform.User) (int, any, error) {
	a := access{user: user, key: user.ID, sessions: "assistant_sessions", memories: "assistant_memories"}
	a.authorize = func(q rowQuerier) error {
		var count int
		if err := q.QueryRow("SELECT count(*) FROM users WHERE id=? AND role='admin' AND enabled=1", user.ID).Scan(&count); err != nil {
			return err
		}
		if user.Role != "admin" || count != 1 {
			return httpapi.NewError(403, "运维助理需要有效的总控管理员权限")
		}
		return nil
	}
	return h.dispatch(w, r, a)
}

// DispatchMember authenticates the request and captures the token only in Go.
// Every tool call and the final transaction recheck expiry and revocation.
func (h *Handler) DispatchMember(w http.ResponseWriter, r *http.Request, token string) (int, any, error) {
	u, err := members.SessionUser(h.db.SQL, token)
	if err != nil {
		return 0, nil, err
	}
	a := access{user: u, key: "member:" + u.ID, sessions: "member_assistant_sessions", memories: "member_assistant_memories"}
	a.authorize = func(q rowQuerier) error {
		current, err := members.SessionUser(q, token)
		if err != nil {
			return err
		}
		if current != u {
			return httpapi.NewError(401, "登录身份已变化，请重新登录")
		}
		return nil
	}
	return h.dispatch(w, r, a)
}
