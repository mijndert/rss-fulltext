package safehttp

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"0.0.0.0", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"100.64.0.1", false},
		{"169.254.0.1", false},
		{"192.0.0.1", false},
		{"192.0.2.1", false},
		{"198.18.0.1", false},
		{"198.51.100.1", false},
		{"203.0.113.1", false},
		{"::1", false},
		{"fc00::1", false},
		{"fd12::1", false},
		{"fe80::1", false},
		{"2606:4700:4700::1111", true},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("could not parse %q", tc.ip)
			}
			if got := IsPublicIP(ip); got != tc.want {
				t.Errorf("IsPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestIsPublicIPNil(t *testing.T) {
	if IsPublicIP(nil) {
		t.Error("IsPublicIP(nil) should be false")
	}
}

func TestValidateURLHostIPLiterals(t *testing.T) {
	ctx := context.Background()

	if err := ValidateURLHost(ctx, "8.8.8.8", false); err != nil {
		t.Errorf("public IP should pass: %v", err)
	}
	if err := ValidateURLHost(ctx, "127.0.0.1", false); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("loopback should be blocked, got %v", err)
	}
	if err := ValidateURLHost(ctx, "10.0.0.1", false); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("private IP should be blocked, got %v", err)
	}
	if err := ValidateURLHost(ctx, "10.0.0.1", true); err != nil {
		t.Errorf("with allowPrivate=true, private IP should pass: %v", err)
	}
	if err := ValidateURLHost(ctx, "", false); err == nil {
		t.Error("empty host should error")
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(0, Options{})
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.CheckRedirect == nil {
		t.Error("CheckRedirect should be set")
	}
	if c.Transport == nil {
		t.Error("Transport should be set")
	}
}
