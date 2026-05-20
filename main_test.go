package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheckSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	t.Setenv("LISTEN_ADDR", srv.Listener.Addr().String())

	if got := runHealthcheck(); got != 0 {
		t.Errorf("exit = %d, want 0", got)
	}
}

func TestHealthcheckFailureOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("LISTEN_ADDR", srv.Listener.Addr().String())

	if got := runHealthcheck(); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
}

func TestHealthcheckFailureOnUnreachable(t *testing.T) {
	// Port 1 is reserved and reliably refused; this exercises the dial-error path.
	t.Setenv("LISTEN_ADDR", "127.0.0.1:1")
	if got := runHealthcheck(); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
}

func TestHealthcheckFailureOnBadAddr(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "not-a-host-port")
	if got := runHealthcheck(); got != 1 {
		t.Errorf("exit = %d, want 1", got)
	}
}

func TestHealthcheckURLMapping(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080/healthz"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080/healthz"},
		{"[::]:8080", "http://[::1]:8080/healthz"},
		{"[::1]:8080", "http://[::1]:8080/healthz"},
		{"10.0.0.5:9090", "http://10.0.0.5:9090/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			got, err := healthcheckURL(tc.addr)
			if err != nil {
				t.Fatalf("healthcheckURL(%q): %v", tc.addr, err)
			}
			if got != tc.want {
				t.Errorf("healthcheckURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestHealthcheckURLError(t *testing.T) {
	if _, err := healthcheckURL("not-a-host-port"); err == nil {
		t.Error("expected error on malformed addr")
	}
}
