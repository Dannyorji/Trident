package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsBlockedWebhookIPSpecialPurposeRanges(t *testing.T) {
	blocked := []string{
		"0.1.2.3", "100.64.0.1", "100.127.255.254", "192.0.0.8", "192.0.2.1",
		"198.18.0.1", "198.19.255.255", "198.51.100.7", "203.0.113.9",
		"224.0.0.1", "239.255.255.255", "240.0.0.1", "255.255.255.255",
		"64:ff9b::7f00:1", "100::1", "2001:db8::1", "2002::1", "fd12:3456::1",
		"fe80::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
	}
	for _, s := range blocked {
		if !isBlockedWebhookIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "100.63.255.255", "172.32.0.1", "198.20.0.1"} {
		if isBlockedWebhookIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestWebhookGuardValidateTargetURL(t *testing.T) {
	g := &webhookGuard{lookup: func(_ context.Context, host string) ([]net.IP, error) {
		if host == "rebind.example" {
			return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.5")}, nil
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}}
	for _, raw := range []string{"https://rebind.example/h", "https://100.64.0.1/", "https://u:p@example.com/", "https://[::1]/"} {
		if err := g.validateTargetURL(context.Background(), raw); err == nil {
			t.Errorf("%q should be rejected", raw)
		}
	}
	if err := g.validateTargetURL(context.Background(), "https://example.com/hook"); err != nil {
		t.Errorf("public https target rejected: %v", err)
	}
}

// A resolver that answers with a public address for the validation lookup and
// loopback afterwards must never produce a connection to loopback.
func TestWebhookClientNeverConnectsToLoopbackOnDNSRebind(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	var calls atomic.Int32
	g := &webhookGuard{lookup: func(_ context.Context, _ string) ([]net.IP, error) {
		if calls.Add(1) == 1 {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}}
	client := g.newClient(500 * time.Millisecond)

	if err := g.validateTargetURL(context.Background(), "https://rebind.example:"+u.Port()+"/"); err != nil {
		t.Fatalf("validation should pass with the public answer: %v", err)
	}
	for i := 0; i < 3; i++ {
		if resp, err := client.Get("http://rebind.example:" + u.Port() + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server on loopback received %d requests", hits.Load())
	}
}

func TestWebhookClientRefusesLoopbackAndMetadataTargets(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()

	g := &webhookGuard{lookup: func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}}
	client := g.newClient(time.Second)
	for _, target := range []string{srv.URL, "http://localhost:1/", "http://169.254.169.254/"} {
		if resp, err := client.Get(target); err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s should be refused", target)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("loopback server was reached")
	}
}

func TestNewWebhookDeliveryHTTPClientIsShared(t *testing.T) {
	if newWebhookDeliveryHTTPClient() != newWebhookDeliveryHTTPClient() {
		t.Fatal("delivery client must be reused, not rebuilt per delivery")
	}
}
