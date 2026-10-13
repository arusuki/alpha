//go:build !linux

package chatcli

import (
	"context"
	"fmt"
)

func terminalPassword(context.Context) (string, error) {
	return "", fmt.Errorf("当前系统请使用 --password-stdin 输入密码")
}
