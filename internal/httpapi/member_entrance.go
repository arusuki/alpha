package httpapi

import (
	"net/http"
	"regexp"
)

var memberPage = regexp.MustCompile(`^/status/[a-z][a-z0-9_-]{2,31}/?$`)
var memberRead = regexp.MustCompile(`^/api/status/[a-z][a-z0-9_-]{2,31}(/gpu)?$`)
var memberWrite = regexp.MustCompile(`^/api/status/[a-z][a-z0-9_-]{2,31}/(login|logout|password|keys|containers)$`)

// MemberEntranceAllowed is shared by the share proxy and control's Host guard.
// Keep this an explicit allowlist: a new management route must never become public
// through the member entrance, even when an administrator cookie is present.
func MemberEntranceAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet {
		if memberPage.MatchString(r.URL.Path) || memberRead.MatchString(r.URL.Path) {
			return true
		}
		switch r.URL.Path {
		case "/status.js", "/status.css", "/gpu.js", "/gpu.css", "/clipboard.js", "/api/members/registration-schema", "/api/members/me/resources":
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
