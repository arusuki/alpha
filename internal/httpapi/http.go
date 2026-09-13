package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"unicode/utf8"
)

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string                  { return e.Message }
func NewError(status int, message string) error { return &Error{status, message} }

func RequestBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, error) {
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" {
		return nil, NewError(415, "请使用 application/json 请求")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	var max *http.MaxBytesError
	if errors.As(err, &max) || len(raw) == 0 {
		return nil, NewError(413, "请求体需在 1–65536 字节之间")
	}
	if err != nil {
		return nil, NewError(400, "无法读取请求体")
	}
	var value map[string]json.RawMessage
	if !utf8.Valid(raw) || json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, NewError(400, "请求体必须是有效的 JSON 对象")
	}
	return value, nil
}
func WriteJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		status = 500
		body = []byte(`{"error":"服务内部错误"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}
func SessionToken(r *http.Request) string {
	c, err := r.Cookie("project_alpha_session")
	if err != nil {
		return ""
	}
	return c.Value
}
func FieldString(value map[string]json.RawMessage, key string) string {
	var out string
	if json.Unmarshal(value[key], &out) != nil {
		return ""
	}
	return out
}
func JSONText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func String(v any) string { s, _ := v.(string); return s }
