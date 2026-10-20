package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// validateWebhookTargetURL rejects webhook target URLs that could be used
// as a server-side request forgery primitive against our own network
// (Issue #453): non-HTTPS URLs, and URLs whose host resolves to a private,
// loopback, link-local, unspecified, or cloud-metadata address. Called both
// at subscription time and again immediately before each delivery. This is a
// fast, friendly rejection only: the policy is enforced at dial time by
// webhookGuard, so the connected address is the checked address even if DNS
// changes between the check and the connection (DNS rebinding).
//
// allowInsecureWebhookTargets is the one documented exemption: the webhook
// delivery tests drive httptest servers, which are plain http:// on
// 127.0.0.1 and so are rejected by both the scheme check and the loopback
// check. Rather than weaken either rule for everyone, the test binary opts
// out explicitly. It is set only from _test.go files, so a production build
// cannot reach this path.
var allowInsecureWebhookTargets = false

var errWebhookTargetBlocked = errors.New("webhook target address is not allowed")

// blockedWebhookNets lists the IANA special-purpose ranges a webhook may never
// reach: loopback, private, link-local (cloud metadata), CGNAT, benchmarking,
// documentation, multicast, reserved, and the IPv6 equivalents.
var blockedWebhookNets = mustParseCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
	"2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}

func isBlockedWebhookIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedWebhookNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// webhookGuard enforces the target blocklist at dial time. DNS is resolved once
// per dial, every answer is checked, the connection is made to that pinned IP,
// and net.Dialer.Control checks the socket address once more.
type webhookGuard struct {
	lookup func(ctx context.Context, host string) ([]net.IP, error)
	// bypass, when it returns true, disables the blocklist (tests only).
	bypass func() bool
}

func (g *webhookGuard) bypassed() bool { return g.bypass != nil && g.bypass() }

func (g *webhookGuard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if g.lookup != nil {
		return g.lookup(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func (g *webhookGuard) checkIPs(ips []net.IP) error {
	if len(ips) == 0 {
		return errors.New("webhook host did not resolve")
	}
	if g.bypassed() {
		return nil
	}
	for _, ip := range ips {
		if isBlockedWebhookIP(ip) {
			return fmt.Errorf("%w: %s", errWebhookTargetBlocked, ip)
		}
	}
	return nil
}

func (g *webhookGuard) control(_, address string, _ syscall.RawConn) error {
	if g.bypassed() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if isBlockedWebhookIP(net.ParseIP(host)) {
		return fmt.Errorf("%w: %s", errWebhookTargetBlocked, host)
	}
	return nil
}

func (g *webhookGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if err := g.checkIPs(ips); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// newClient returns a client that dials through the guard, does not follow
// redirects (a validated URL could otherwise redirect to an internal address,
// Issue #453), and uses a bounded transport so connections are reused.
func (g *webhookGuard) newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           g.dialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   4,
			MaxConnsPerHost:       32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (g *webhookGuard) validateTargetURL(ctx context.Context, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid target URL: %w", err)
	}
	if g.bypassed() {
		return nil
	}
	if parsed.Scheme != "https" {
		return errors.New("target URL must use https")
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("target URL must have a host")
	}
	if parsed.User != nil {
		return errors.New("target URL must not include credentials")
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("could not resolve target host: %w", err)
	}
	return g.checkIPs(ips)
}

var defaultWebhookGuard = &webhookGuard{bypass: func() bool { return allowInsecureWebhookTargets }}

func validateWebhookTargetURL(rawURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return defaultWebhookGuard.validateTargetURL(ctx, rawURL)
}

var webhookDeliveryClient = defaultWebhookGuard.newClient(5 * time.Second)

// newWebhookDeliveryHTTPClient returns the shared delivery client rather than
// building a new one per delivery, so connections are reused.
func newWebhookDeliveryHTTPClient() *http.Client { return webhookDeliveryClient }
