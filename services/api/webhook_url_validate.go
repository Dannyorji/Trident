package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// validateWebhookTargetURL rejects webhook target URLs that could be used
// as a server-side request forgery primitive against our own network
// (Issue #453): non-HTTPS URLs, and URLs whose host resolves to a private,
// loopback, link-local, unspecified, or cloud-metadata address. Called both
// at subscription time and again immediately before each delivery, since
// DNS can change between the two.
//
// allowInsecureWebhookTargets is the one documented exemption: the webhook
// delivery tests drive httptest servers, which are plain http:// on
// 127.0.0.1 and so are rejected by both the scheme check and the loopback
// check. Rather than weaken either rule for everyone, the test binary opts
// out explicitly. It is set only from _test.go files, so a production build
// cannot reach this path.
var allowInsecureWebhookTargets = false

func validateWebhookTargetURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid target URL: %w", err)
	}
	if allowInsecureWebhookTargets {
		return nil
	}
	if parsed.Scheme != "https" {
		return errors.New("target URL must use https")
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("target URL must have a host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("could not resolve target host: %w", err)
	}
	for _, ip := range ips {
		if isBlockedWebhookIP(ip) {
			return fmt.Errorf("target host resolves to a disallowed address: %s", ip.String())
		}
	}
	return nil
}

func isBlockedWebhookIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.Equal(net.ParseIP("169.254.169.254")) // cloud metadata endpoint
}

// newWebhookDeliveryHTTPClient does not follow redirects: a validated
// target URL could otherwise redirect to an internal address at request
// time, bypassing validateWebhookTargetURL entirely (Issue #453).
func newWebhookDeliveryHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
