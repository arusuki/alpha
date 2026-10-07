package sshkeys

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"

// Existing public fixtures; these tests never generate private keys.
const ecdsaTestKey = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBIvR3cOir2XFsX4NiA4QO1JKQ7c87emaiV0rBXS3fiseEt0seHFTvuv2Tl0Zz5jQJS1Ko0oVLFAQZ4BtLtn6hKg="
const rsaTestKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCe6jMoy1xCQgiZkZJ7gi6NLj4uRqz2OaUGK/OJYZTfBqK+SlS9iymAluHu9K+cc4+0qxx0gn7dRTJWINSgzvca6ayYe995EKgD1hE5krh9BH0bRrXB+hGqyslcZOgLNO+v8jYojClQbRtET2tS+xb4k33GCuL5wgla2790ZgOQgs7huQUjG0S8c1W+EYt6fI4cWE/DeEBnv9sqryS8rOb0PbM6WUd7XBadwySFWYQUX0ei56GNt12Z4gADEGlFQV/OnV0PvnTcAMGUl0rfToPgJ4jgogWKoTVWuZ9wyA/x+2LRLRvgm2a969ig937/AH0i0Wq+FzqfK7EXQ99Yf5K/"

func TestNormalizeRejectsInjectedOptionsAndMalformedWire(t *testing.T) {
	for _, v := range []string{"", `command="id" ` + testKey, testKey + "\n" + testKey, "ssh-rsa " + strings.Fields(testKey)[1], "ssh-ed25519 AAAA", testKey + "\x00"} {
		if _, e := Normalize(v); e == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	if v, e := Normalize(testKey + " comment"); e != nil || v != testKey {
		t.Fatalf("%q %v", v, e)
	}
}

func TestNormalizeRejectsAlternateBase64Identity(t *testing.T) {
	if normal, err := Normalize(ecdsaTestKey + " existing fixture"); err != nil || normal != ecdsaTestKey {
		t.Fatalf("valid ECDSA key: %q %v", normal, err)
	}
	canonical, err := base64.StdEncoding.DecodeString(strings.Fields(ecdsaTestKey)[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, ending := range []string{"h=", "i=", "j="} {
		alternate := strings.TrimSuffix(ecdsaTestKey, "g=") + ending
		raw, err := base64.StdEncoding.DecodeString(strings.Fields(alternate)[1])
		if err != nil || !bytes.Equal(raw, canonical) {
			t.Fatalf("fixture must represent the same key: %v", err)
		}
		if _, err := Normalize(alternate); err == nil {
			t.Fatal("accepted alternate encoding of the same public key")
		}
	}
}

func TestNormalizeRejectsNonCanonicalRSAMPInts(t *testing.T) {
	if normal, err := Normalize(rsaTestKey); err != nil || normal != rsaTestKey {
		t.Fatalf("valid RSA key: %q %v", normal, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Fields(rsaTestKey)[1])
	if err != nil {
		t.Fatal(err)
	}
	fields := [][]byte{}
	for len(raw) > 0 {
		n := int(binary.BigEndian.Uint32(raw[:4]))
		fields = append(fields, raw[4:4+n])
		raw = raw[4+n:]
	}
	for _, kind := range []string{"negative-exponent", "padded-exponent", "padded-modulus", "negative-modulus"} {
		t.Run(kind, func(t *testing.T) {
			e, n := bytes.Clone(fields[1]), bytes.Clone(fields[2])
			switch kind {
			case "negative-exponent":
				e[0] |= 128
			case "padded-exponent":
				e = append([]byte{0}, e...)
			case "padded-modulus":
				n = append([]byte{0}, n...)
			case "negative-modulus":
				n = n[1:]
			}
			var wire bytes.Buffer
			for _, field := range [][]byte{fields[0], e, n} {
				if err := binary.Write(&wire, binary.BigEndian, uint32(len(field))); err != nil {
					t.Fatal(err)
				}
				wire.Write(field)
			}
			if _, err := Normalize("ssh-rsa " + base64.StdEncoding.EncodeToString(wire.Bytes())); err == nil {
				t.Fatalf("accepted %s RSA encoding", kind)
			}
		})
	}
}
func TestMemberCommentsPreserveOtherKeys(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	initial := []byte("# existing\n" + testKey + " unrelated\n")
	one, e := Rewrite(initial, a, testKey)
	if e != nil {
		t.Fatal(e)
	}
	both, e := Rewrite(one, b, testKey)
	if e != nil {
		t.Fatal(e)
	}
	again, e := Rewrite(both, a, testKey)
	if e != nil {
		t.Fatal(e)
	}
	removed, e := Rewrite(again, a, "")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(string(removed), string(initial)) || strings.Contains(string(removed), Marker(a)) || !strings.Contains(string(removed), Marker(b)) {
		t.Fatalf("wrong cleanup %s", removed)
	}
	final, e := Rewrite(removed, b, "")
	if e != nil || string(final) != string(initial) {
		t.Fatalf("modified unrelated key %q %v", final, e)
	}
}

func TestMultipleKeysNormalizeAndRewrite(t *testing.T) {
	keys, err := NormalizeList("\n" + testKey + " laptop\r\n" + ecdsaTestKey + " desktop\n" + testKey + " duplicate\n")
	if err != nil || keys != testKey+"\n"+ecdsaTestKey {
		t.Fatalf("normalize: %q %v", keys, err)
	}
	for _, bad := range []string{" \n", testKey + "\ncommand=\"id\" " + ecdsaTestKey, strings.Repeat(" ", 65537)} {
		if _, err := NormalizeList(bad); err == nil {
			t.Fatal("accepted invalid list")
		}
	}
	if _, err := NormalizeList(testKey + "\ninvalid"); err == nil || !strings.Contains(err.Error(), "第 2 行") {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	unrelated := "# unrelated\n" + testKey + " external\n"
	old, err := Rewrite([]byte(unrelated), id, keys)
	if err != nil || strings.Count(string(old), Marker(id)) != 3 {
		t.Fatalf("rewrite: %s %v", old, err)
	}
	replaced, err := Rewrite(old, id, ecdsaTestKey)
	if err != nil || string(replaced) != unrelated+"# "+Marker(id)+"\n"+ecdsaTestKey+" "+Marker(id)+"\n" {
		t.Fatalf("replace: %s %v", replaced, err)
	}
	removed, err := Rewrite(old, id, "")
	if err != nil || string(removed) != unrelated {
		t.Fatalf("remove: %s %v", removed, err)
	}
}
