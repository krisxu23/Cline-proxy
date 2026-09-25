package app

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// 5.1: schemes / empty host.
func TestOutboundRejectsBadSchemeAndEmptyHostFix(t *testing.T) {
	t.Setenv(AllowPrivateUpstreamEnv, "")
	for _, bad := range []string{
		"file:///etc/passwd",
		"FILE:///etc/passwd",
		"gopher://evil/x",
		"ftp://example.com/x",
		"FTP://EXAMPLE.COM/x",
		"https:///nohost",
		"http://",
		"",
		"   ",
	} {
		if err := validateOutboundURL(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// 5.1: metadata hostnames incl. case/trailing-dot variants.
func TestOutboundRejectsMetadataHostnamesFix(t *testing.T) {
	t.Setenv(AllowPrivateUpstreamEnv, "1") // even with opt-in
	for _, bad := range []string{
		"http://metadata/latest/",
		"http://METADATA/latest/",
		"http://metadata./latest/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://Metadata.Google.Internal./x",
		"http://metadata.goog/x",
		"http://instance-data/latest/",
		"http://INSTANCE-DATA/x",
	} {
		if err := validateOutboundURL(bad); err == nil {
			t.Errorf("%q must always be rejected", bad)
		}
	}
}

// 5.1: link-local literals incl. IPv6 zone form; private/loopback gated solely by env.
func TestOutboundLinkLocalAndPrivateGatingFix(t *testing.T) {
	t.Setenv(AllowPrivateUpstreamEnv, "1")
	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.10.20/v1",
		"http://[fe80::1]/v1",
		"http://[fe80::1%eth0]/v1", // zone must be stripped before parse
		"http://[FF02::1]/v1",
		"http://0.0.0.0:8080/v1",
		"http://[::]/v1",
		"http://100.64.1.1/v1",
	} {
		if err := validateOutboundURL(bad); err == nil {
			t.Errorf("%q must always be rejected", bad)
		}
	}
	// private/loopback gated SOLELY by env
	t.Setenv(AllowPrivateUpstreamEnv, "")
	for _, bad := range []string{
		"http://127.0.0.1:11434/v1",
		"http://10.0.0.5/v1",
		"http://192.168.1.10:8000/v1",
		"http://172.16.31.4/v1",
		"http://[::1]:11434/v1",
	} {
		if err := validateOutboundURL(bad); err == nil {
			t.Errorf("%q must be rejected by default", bad)
		}
	}
	t.Setenv(AllowPrivateUpstreamEnv, "1")
	for _, ok := range []string{
		"http://127.0.0.1:11434/v1",
		"http://10.0.0.5/v1",
		"http://192.168.1.10:8000/v1",
		"http://[::1]:11434/v1",
		"https://api.example.com/v1",
	} {
		if err := validateOutboundURL(ok); err != nil {
			t.Errorf("with opt-in %q must be accepted: %v", ok, err)
		}
	}
}

// 5.1: DNS->link-local via stubbed resolver (deterministic, no network).
func TestOutboundDNSLinkLocalFix(t *testing.T) {
	old := lookupIPHost
	lookupIPHost = func(host string) ([]net.IP, error) {
		if host == "evil.example.com" {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	defer func() { lookupIPHost = old }()
	if reason := resolvedLinkLocalReason("evil.example.com"); reason == "" {
		t.Fatal("hostname resolving to link-local must be rejected")
	}
	if err := validateOutboundURL("https://evil.example.com/v1"); err == nil {
		t.Fatal("URL whose host resolves to link-local must be rejected")
	}
	// NXDOMAIN stays allowed (config-time undecidable)
	if reason := resolvedLinkLocalReason("nxdomain.invalid"); reason != "" {
		t.Fatalf("unresolvable host must not be blocked, got %q", reason)
	}
}

// --- 5.2 dial guard ---

type fakeSSRFConn struct {
	remote string
	closed *bool
}

func (c *fakeSSRFConn) Read(b []byte) (int, error)         { return 0, fmt.Errorf("x") }
func (c *fakeSSRFConn) Write(b []byte) (int, error)        { return 0, fmt.Errorf("x") }
func (c *fakeSSRFConn) Close() error                       { *c.closed = true; return nil }
func (c *fakeSSRFConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *fakeSSRFConn) RemoteAddr() net.Addr               { return &net.TCPAddr{IP: net.ParseIP(stripZone(c.remote))} }
func (c *fakeSSRFConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeSSRFConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeSSRFConn) SetWriteDeadline(t time.Time) error { return nil }

func stripZone(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '%' {
			return s[:i]
		}
	}
	return s
}

// 5.2: pre-check blocks without dialing.
func TestDialGuardPreCheckBlocksWithoutDialFix(t *testing.T) {
	dialed := false
	stub := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = true
		return nil, fmt.Errorf("must not dial")
	}
	g := dialWithSSRFGuard(stub)
	for _, addr := range []string{
		"metadata.google.internal:80",
		"METADATA:80",
		"169.254.169.254:80",
		"[fe80::1]:80",
		"[fe80::1%eth0]:80", // zone stripped before parse
	} {
		dialed = false
		if _, err := g(context.Background(), "tcp", addr); err == nil {
			t.Errorf("addr %q must be blocked pre-dial", addr)
		}
		if dialed {
			t.Errorf("addr %q must not reach dial", addr)
		}
	}
	// ordinary + LAN addrs pass through to dial
	dialed = false
	closed := false
	stubOK := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = true
		return &fakeSSRFConn{remote: "93.184.216.34", closed: &closed}, nil
	}
	g2 := dialWithSSRFGuard(stubOK)
	for _, addr := range []string{"93.184.216.34:80", "127.0.0.1:8080", "10.0.0.5:80"} {
		dialed = false
		if _, err := g2(context.Background(), "tcp", addr); err != nil {
			t.Errorf("addr %q must pass pre-check: %v", addr, err)
		}
		if !dialed {
			t.Errorf("addr %q must reach dial", addr)
		}
	}
}

// 5.2: post-check closes conn on DNS-rebind to link-local.
func TestDialGuardPostCheckClosesReboundConnFix(t *testing.T) {
	closed := false
	stub := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return &fakeSSRFConn{remote: "169.254.169.254", closed: &closed}, nil
	}
	g := dialWithSSRFGuard(stub)
	if _, err := g(context.Background(), "tcp", "93.184.216.34:80"); err == nil {
		t.Fatal("rebound-to-link-local conn must error")
	}
	if !closed {
		t.Fatal("rebound-to-link-local conn must be closed")
	}
	// zone-form remote also caught
	closed = false
	stubZ := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return &fakeSSRFConn{remote: "fe80::1", closed: &closed}, nil
	}
	gz := dialWithSSRFGuard(stubZ)
	if _, err := gz(context.Background(), "tcp", "93.184.216.34:80"); err == nil {
		t.Fatal("rebound-to-fe80 conn must error")
	}
	if !closed {
		t.Fatal("rebound-to-fe80 conn must be closed")
	}
}
