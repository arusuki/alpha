package httpapi

import (
	"net/http"
	"regexp"
)

var memberPage = regexp.MustCompile(`^/status/[a-z][a-z0-9_-]{2,31}/?$`)
var memberRead = regexp.MustCompile(`^/api/status/[a-z][a-z0-9_-]{2,31}(/gpu|/disk)?$`)
var memberWrite = regexp.MustCompile(`^/api/status/[a-z][a-z0-9_-]{2,31}/(login|logout|password|keys|containers)$`)

var memberAssistantSession = regexp.MustCompile(`^/api/member-assistant/sessions/[a-f0-9]{32}$`)
var memberAssistantMessage = regexp.MustCompile(`^/api/member-assistant/sessions/[a-f0-9]{32}/messages$`)

func MemberAssistantAllowed(r *http.Request) bool {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet:
		return p == "/api/member-assistant/me" || p == "/api/member-assistant/sessions" || p == "/api/member-assistant/memories" || memberAssistantSession.MatchString(p)
	case http.MethodPost:
		return p == "/api/member-assistant/logout" || p == "/api/member-assistant/sessions" || memberAssistantMessage.MatchString(p)
	case http.MethodDelete:
		return p == "/api/member-assistant/memories"
	}
	return false
}

// MemberEntranceAllowed is shared by the share proxy and control's Host guard.
// Keep this an explicit allowlist: a new management route must never become public
// through the member entrance, even when an administrator cookie is present.
func MemberEntranceAllowed(r *http.Request) bool {
	if MemberAssistantAllowed(r) {
		return true
	}
	if r.Method == http.MethodGet {
		if memberPage.MatchString(r.URL.Path) || memberRead.MatchString(r.URL.Path) {
			return true
		}
		switch r.URL.Path {
		case "/status.js", "/status.css", "/disk-capacity.js", "/disk-capacity.css", "/gpu.js", "/gpu.css", "/usage.js", "/member-disk.js", "/clipboard.js", "/api/members/registration-schema", "/api/members/me/resources":
			return true
		}
	}
	if r.Method == http.MethodPost {
		if memberWrite.MatchString(r.URL.Path) {
			return true
		}
		switch r.URL.Path {
		case "/api/members/register", "/api/members/registration-options", "/api/members/me/retry", "/api/members/me/containers":
			return true
		}
	}
	return false
}
