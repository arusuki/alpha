package chatcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"project-alpha/internal/chatconfig"
)

func readDefaults(executable string) (chatconfig.Defaults, error) {
	var defaults chatconfig.Defaults
	raw, err := os.ReadFile(executable + ".json")
	if os.IsNotExist(err) {
		return defaults, nil
	}
	if err != nil {
		return defaults, err
	}
	if len(raw) > 16384 {
		return defaults, fmt.Errorf("CLI 下发配置过大")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&defaults); err != nil {
		return defaults, fmt.Errorf("CLI 下发配置无效: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return defaults, fmt.Errorf("CLI 下发配置包含多余数据")
	}
	return defaults, defaults.Validate()
}
