package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// The receipt is independent of the main binary version: an older installer
// may have installed the new worker without updating its service containers.
type serviceSync struct {
	Tag   string `json:"tag"`
	State string `json:"state"`
}

func ServicesAttempted(directory, tag string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(directory, "update-services.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s serviceSync
	if json.Unmarshal(raw, &s) != nil || ValidateTag(s.Tag) != nil || (s.State != "attempted" && s.State != "completed") {
		return false, fmt.Errorf("invalid service update receipt: inspect update-services.json; use a new data directory for incompatible formats")
	}
	return s.Tag == tag, nil
}

func MarkServicesAttempted(directory, tag string) error {
	return writeServiceSync(directory, tag, "attempted")
}

func servicesCompleted(directory, tag string) error {
	return writeServiceSync(directory, tag, "completed")
}

func writeServiceSync(directory, tag, state string) error {
	if err := ValidateTag(tag); err != nil {
		return err
	}
	return WriteJSON(filepath.Join(directory, "update-services.json"), serviceSync{Tag: tag, State: state})
}
