package chatcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type loginState struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

func readState(path string) (loginState, error) {
	var state loginState
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		return state, fmt.Errorf("登录文件必须为权限 0600 的普通文件: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if json.Unmarshal(raw, &state) != nil || !tokenPattern.MatchString(state.Token) || !usernamePattern.MatchString(state.Username) {
		return loginState{}, fmt.Errorf("登录文件格式无效，请删除该文件后重新登录: %s", path)
	}
	server, err := serverURL(state.Server)
	if err != nil || server != state.Server {
		return loginState{}, fmt.Errorf("登录文件中的服务器地址无效: %s", path)
	}
	return state, nil
}

func saveState(path string, state loginState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".alpha-chat-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
