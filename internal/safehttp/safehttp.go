package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

var (
	ErrBlockedAddress   = errors.New("safehttp: refused to dial non-public address")
	ErrBlockedPort      = errors.New("safehttp: refused to dial non-allowed port")
	ErrBlockedScheme    = errors.New("safehttp: blocked scheme on redirect")
	ErrTooManyRedirects = errors.New("safehttp: too many redirects")
)

type Options struct {
	AllowPrivateAddresses  bool
	AllowedPorts           []int
	MaxRedirects           int
	DialTimeout            time.Duration
	TLSHandshakeTimeout    time.Duration
	ResponseHeaderTimeout  time.Duration
	IdleConnTimeout        time.Duration
	MaxIdleConnsPerHost    int
	MaxResponseHeaderBytes int64
}

func NewClient(timeout time.Duration, opts Options) *http.Client {
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = 5
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 5 * time.Second
	}
	if opts.TLSHandshakeTimeout <= 0 {
		opts.TLSHandshakeTimeout = 5 * time.Second
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = 8 * time.Second
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = 30 * time.Second
	}
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 4
	}
	if opts.MaxResponseHeaderBytes <= 0 {
		opts.MaxResponseHeaderBytes = 64 << 10
	}
	if len(opts.AllowedPorts) == 0 {
		opts.AllowedPorts = []int{80, 443}
	}

	dialer := &net.Dialer{
		Timeout:   opts.DialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   makeControl(opts.AllowPrivateAddresses, opts.AllowedPorts),
	}

	tr := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  opts.ResponseHeaderTimeout,
		IdleConnTimeout:        opts.IdleConnTimeout,
		MaxIdleConnsPerHost:    opts.MaxIdleConnsPerHost,
		MaxResponseHeaderBytes: opts.MaxResponseHeaderBytes,
		ForceAttemptHTTP2:      true,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= opts.MaxRedirects {
				return ErrTooManyRedirects
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: %s", ErrBlockedScheme, req.URL.Scheme)
			}
			return ValidateURLHost(req.Context(), req.URL.Hostname(), opts.AllowPrivateAddresses)
		},
	}
}

func makeControl(allowPrivate bool, ports []int) func(network, address string, c syscall.RawConn) error {
	allowed := make(map[int]bool, len(ports))
	for _, p := range ports {
		allowed[p] = true
	}
	return func(_, address string, _ syscall.RawConn) error {
		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("safehttp: split: %w", err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("safehttp: bad port %q: %w", portStr, err)
		}
		if !allowed[port] {
			return fmt.Errorf("%w: %s", ErrBlockedPort, address)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("safehttp: dial target is not IP literal: %q", host)
		}
		if !allowPrivate && !IsPublicIP(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, ip.String())
		}
		return nil
	}
}

func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] < 128 {
			return false
		}
		if v4[0] == 0 {
			return false
		}
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
			return false
		}
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 2 {
			return false
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return false
		}
		if v4[0] == 198 && v4[1] == 51 && v4[2] == 100 {
			return false
		}
		if v4[0] == 203 && v4[1] == 0 && v4[2] == 113 {
			return false
		}
	}
	if len(ip) == net.IPv6len && ip.To4() == nil && (ip[0]&0xfe) == 0xfc {
		return false
	}
	return true
}

func ValidateURLHost(ctx context.Context, host string, allowPrivate bool) error {
	if host == "" {
		return errors.New("safehttp: empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && !IsPublicIP(ip) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, ip.String())
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("safehttp lookup %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("safehttp: no addresses for host %s", host)
	}
	if !allowPrivate {
		for _, ip := range ips {
			if !IsPublicIP(ip) {
				return fmt.Errorf("%w: %s -> %s", ErrBlockedAddress, host, ip.String())
			}
		}
	}
	return nil
}
