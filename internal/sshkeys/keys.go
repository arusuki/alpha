// Package sshkeys validates plain SSH public keys and manages member annotations.
package sshkeys

import (
	"crypto/elliptic"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var ID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Normalize accepts one key, never authorized_keys options, certificates or commands.
func Normalize(value string) (string, error) {
	bad := fmt.Errorf("请提供一行有效的 SSH 公钥（Ed25519、RSA 2048 位以上或 ECDSA），不接受授权选项")
	value = strings.TrimSpace(value)
	if len(value) > 16384 || strings.ContainsAny(value, "\r\n\x00") {
		return "", bad
	}
	parts := strings.Fields(value)
	if len(parts) < 2 {
		return "", bad
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return "", bad
	}
	read := func() []byte {
		if len(raw) < 4 {
			raw = nil
			return nil
		}
		n := int(binary.BigEndian.Uint32(raw))
		raw = raw[4:]
		if n > len(raw) {
			raw = nil
			return nil
		}
		v := raw[:n]
		raw = raw[n:]
		return v
	}
	if string(read()) != parts[0] {
		return "", bad
	}
	switch parts[0] {
	case "ssh-ed25519":
		if len(read()) != 32 {
			return "", bad
		}
	case "ssh-rsa":
		e, n := read(), read()
		if len(e) > 4 || !positiveMPInt(e) || !positiveMPInt(n) {
			return "", bad
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		bits := new(big.Int).SetBytes(n).BitLen()
		if exponent < 3 || exponent%2 == 0 || bits < 2048 || bits > 8192 {
			return "", bad
		}
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		name := string(read())
		if parts[0] != "ecdsa-sha2-"+name {
			return "", bad
		}
		curves := map[string]elliptic.Curve{"nistp256": elliptic.P256(), "nistp384": elliptic.P384(), "nistp521": elliptic.P521()}
		curve := curves[name]
		if curve == nil {
			return "", bad
		}
		if x, _ := elliptic.Unmarshal(curve, read()); x == nil {
			return "", bad
		}
	default:
		return "", bad
	}
	if len(raw) != 0 {
		return "", bad
	}
	return parts[0] + " " + parts[1], nil
}

// SSH mpints are signed and must use the shortest representation. Reject
// alternate encodings so a key has one identity in the shared key pool.
func positiveMPInt(v []byte) bool {
	return len(v) > 0 && v[0]&128 == 0 && (v[0] != 0 || len(v) > 1 && v[1]&128 != 0)
}

// NormalizeList accepts one public key per nonempty line and deduplicates by key
// material. The canonical persisted representation is newline-separated keys.
func NormalizeList(value string) (string, error) {
	if len(value) > 32768 {
		return "", fmt.Errorf("SSH 公钥总长度不能超过 32768 字节")
	}
	keys := []string{}
	seen := map[string]bool{}
	for i, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, err := Normalize(line)
		if err != nil {
			return "", fmt.Errorf("第 %d 行：%w", i+1, err)
		}
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("请至少提供一个 SSH 公钥，一行一个")
	}
	if len(keys) > 64 {
		return "", fmt.Errorf("最多提供 64 个 SSH 公钥")
	}
	return strings.Join(keys, "\n"), nil
}

func Marker(id string) string { return "project-alpha:member:" + id }

// Rewrite preserves unrelated lines byte-for-byte, including identical keys
// belonging to another member. The marker is both a # comment and key comment.
func Rewrite(old []byte, id, key string) ([]byte, error) {
	if !ID.MatchString(id) {
		return nil, fmt.Errorf("invalid member ID")
	}
	if key != "" {
		var err error
		key, err = NormalizeList(key)
		if err != nil {
			return nil, err
		}
	}
	marker := Marker(id)
	var out strings.Builder
	for _, line := range strings.SplitAfter(string(old), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "# "+marker || strings.HasSuffix(trimmed, " "+marker) {
			continue
		}
		out.WriteString(line)
	}
	if key != "" {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString("# " + marker + "\n")
		for _, line := range strings.Split(key, "\n") {
			out.WriteString(line + " " + marker + "\n")
		}
	}
	return []byte(out.String()), nil
}
