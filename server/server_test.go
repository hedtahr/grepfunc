package server

import (
	"strings"
	"testing"
)

func TestIsBannedPath(t *testing.T) {
	cases := []struct {
		path   string
		banned bool
	}{
		{"/home/user/.env", true},
		{"/home/user/.env.local", true},
		{"/home/user/.env_prod", true},
		{"/home/user/.netrc", true},
		{"/home/user/.ssh/id_rsa", true},
		{"/home/user/.ssh/id_ed25519", true},
		{"/home/user/.ssh/id_ecdsa", true},
		{"/home/user/.ssh/id_dsa", true},
		{"/home/user/.aws/credentials", true},
		{"/certs/server.pem", true},
		{"/certs/server.key", true},
		{"/certs/client.p12", true},
		{"/certs/client.pfx", true},
		{"/src/main.go", false},
		{"/config/settings.json", false},
		{"/home/user/.gitconfig", false},
		{"/home/user/envelope.go", false}, // "envelope" contains "env" but is not .env
	}
	for _, c := range cases {
		got := IsBannedPath(c.path)
		if got != c.banned {
			t.Errorf("IsBannedPath(%q) = %v, want %v", c.path, got, c.banned)
		}
	}
}

func TestCheckBanned(t *testing.T) {
	if err := CheckBanned("/etc/passwd"); err != nil {
		t.Errorf("unexpected ban for passwd: %v", err)
	}
	err := CheckBanned("/home/user/.env")
	if err == nil {
		t.Fatal("expected error for .env, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should say 'access denied': %v", err)
	}
}
