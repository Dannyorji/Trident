package main

import (
	"net"
	"testing"
)

func TestIsBlockedWebhookIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"private 10/8", "10.0.0.5", true},
		{"private 172.16/12", "172.16.0.5", true},
		{"private 192.168/16", "192.168.1.5", true},
		{"link-local", "169.254.1.1", true},
		{"cloud metadata", "169.254.169.254", true},
		{"unspecified", "0.0.0.0", true},
		{"public v4", "93.184.216.34", false},
		{"public v6", "2606:4700:4700::1111", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("failed to parse test IP %q", tc.ip)
			}
			got := isBlockedWebhookIP(ip)
			if got != tc.blocked {
				t.Errorf("isBlockedWebhookIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

func TestValidateWebhookTargetURL_RejectsNonHTTPS(t *testing.T) {
	err := validateWebhookTargetURL("http://example.com/hook")
	if err == nil {
		t.Fatal("expected error for non-https URL, got nil")
	}
}

func TestValidateWebhookTargetURL_RejectsInvalidURL(t *testing.T) {
	err := validateWebhookTargetURL("not a url")
	if err == nil {
		t.Fatal("expected error for malformed URL, got nil")
	}
}
