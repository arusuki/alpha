package updater

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type version struct {
	numbers [3]uint64
	pre     string
}

func parseVersion(tag string) (version, error) {
	var v version
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return v, fmt.Errorf("invalid release version %q; expected v0.x.y", tag)
	}
	for i := range v.numbers {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return v, err
		}
		v.numbers[i] = n
	}
	v.pre = m[4]
	for _, part := range strings.Split(v.pre, ".") {
		if numeric(part) && len(part) > 1 && part[0] == '0' {
			return v, fmt.Errorf("invalid prerelease version %q", tag)
		}
	}
	return v, nil
}
func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func (v version) compare(other version) int {
	for i, n := range v.numbers {
		if n < other.numbers[i] {
			return -1
		}
		if n > other.numbers[i] {
			return 1
		}
	}
	if v.pre == other.pre {
		return 0
	}
	if v.pre == "" {
		return 1
	}
	if other.pre == "" {
		return -1
	}
	a, b := strings.Split(v.pre, "."), strings.Split(other.pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		an, bn := numeric(a[i]), numeric(b[i])
		if an != bn {
			if an {
				return -1
			}
			return 1
		}
		if an && len(a[i]) != len(b[i]) {
			if len(a[i]) < len(b[i]) {
				return -1
			}
			return 1
		}
		return strings.Compare(a[i], b[i])
	}
	if len(a) < len(b) {
		return -1
	}
	return 1
}
func supportedVersion(tag string) (version, error) {
	v, err := parseVersion(tag)
	if err != nil {
		return v, err
	}
	minimum, _ := parseVersion("v0.3.1")
	if v.numbers[0] != 0 || v.compare(minimum) < 0 {
		return v, fmt.Errorf("unsupported release %s; supported range is v0.3.1 <= version < v1.0.0", tag)
	}
	return v, nil
}
