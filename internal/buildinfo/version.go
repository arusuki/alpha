// Package buildinfo reports the version embedded in a built executable.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Release builds set these values with -ldflags -X.
var Version = "dev"
var Revision string

func String(name string) string {
	revision, dirty := Revision, false
	if info, ok := debug.ReadBuildInfo(); ok && revision == "" {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty && revision != "" {
		revision += "+dirty"
	}
	if revision != "" {
		revision = "commit " + revision + ", "
	}
	return fmt.Sprintf("%s %s (%s%s, %s/%s)", name, Version, revision, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
