// Package containerorigin describes trusted Docker creation provenance.
package containerorigin

import "regexp"

const Version = 1
const Endpoint = "/_rootless/origins"

var idPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

// Source describes the connecting process as observed by the host. It is not
// supplied by a Docker client, image label, environment variable or username.
type Source struct {
	PID         int32  `json:"pid"`
	UID         uint32 `json:"uid"`
	StartTicks  uint64 `json:"start_ticks"`
	ContainerID string `json:"container_id"`
	Error       string `json:"error"`
}

type Record struct {
	Version     int    `json:"version"`
	ContainerID string `json:"container_id"`
	CreatedAt   string `json:"created_at"`
	BootID      string `json:"boot_id"`
	Source      Source `json:"source"`
}

type Response struct {
	Version int      `json:"version"`
	Origins []Record `json:"origins"`
}
