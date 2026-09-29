package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// Issue #576: DefaultCacheKey joined path, network and query with an
// unescaped "|", so a "|" inside any component could imitate the delimiter
// and two genuinely different requests could produce byte-identical keys.
//
// The concrete collision under the old join, with "|" written as it arrives
// percent-encoded (%7C decodes into r.URL.Path):
//
//	path "/v1/c|mainnet", network "testnet"         -> "/v1/c|mainnet|testnet|"
//	path "/v1/c",         network "mainnet|testnet" -> "/v1/c|mainnet|testnet|"
//
// Same key, different requests: one endpoint would serve the other's cached
// response. Note the query component cannot carry a literal "|" — the key is
// built from url.Values.Encode(), which re-encodes it as %7C — so the path
// and network are the two components that can forge the framing.
//
// Length-prefixing removes the ambiguity structurally: a reader takes
// exactly the announced number of bytes, so no component's content can be
// mistaken for framing.
//
// This was latent rather than exploitable when fixed — both routes wrapped
// today validate the contract id against ^C[A-Z2-7]{55}$, which cannot
// contain "|" — but ResponseCache is a general-purpose helper, and the next
// route wrapped with a free-form path segment inherits the trap.

// requestOnNetwork builds a GET request for rawURL as though it had been
// authenticated for the given network.
func requestOnNetwork(rawURL, network string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, rawURL, nil)
	return r.WithContext(middleware.WithNetwork(r.Context(), network))
}

// TestDefaultCacheKey_EncodedPipeCannotForgeDelimiter is the acceptance
// criterion from the issue: two requests differing only by where an encoded
// "|" falls must not produce the same key.
func TestDefaultCacheKey_EncodedPipeCannotForgeDelimiter(t *testing.T) {
	// The "|" is inside the path; the network is a plain "testnet".
	forged := requestOnNetwork("/v1/c%7Cmainnet", "testnet")
	forgedKey, _ := middleware.DefaultCacheKey(forged)

	// A different request entirely: shorter path, and the "|" falls inside
	// the network component instead. Under the old bare-separator join these
	// two flattened to the identical string "/v1/c|mainnet|testnet|".
	plain := requestOnNetwork("/v1/c", "mainnet|testnet")
	plainKey, _ := middleware.DefaultCacheKey(plain)

	if forgedKey == plainKey {
		t.Fatalf("an encoded | forged the key delimiter: distinct requests both produced %q", forgedKey)
	}
}

// TestDefaultCacheKey_ComponentBoundariesAreUnambiguous asserts the general
// property rather than a single hand-picked pair: moving bytes across a
// component boundary must always change the key. Each pair below collides
// under a bare separator.
func TestDefaultCacheKey_ComponentBoundariesAreUnambiguous(t *testing.T) {
	cases := []struct {
		name       string
		urlA, netA string
		urlB, netB string
	}{
		{
			name: "pipe at the path/network boundary",
			urlA: "/v1/c%7Cmainnet", netA: "testnet",
			urlB: "/v1/c", netB: "mainnet|testnet",
		},
		{
			name: "whole network absorbed into the path",
			urlA: "/v1/spec%7Ctestnet%7C", netA: "",
			urlB: "/v1/spec", netB: "testnet||testnet",
		},
		{
			name: "empty path segment vs empty network",
			urlA: "/v1/a%7C", netA: "testnet",
			urlB: "/v1/a", netB: "|testnet",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyA, _ := middleware.DefaultCacheKey(requestOnNetwork(tc.urlA, tc.netA))
			keyB, _ := middleware.DefaultCacheKey(requestOnNetwork(tc.urlB, tc.netB))
			if keyA == keyB {
				t.Fatalf("%s: distinct requests (%q, net=%q) and (%q, net=%q) share the cache key %q",
					tc.name, tc.urlA, tc.netA, tc.urlB, tc.netB, keyA)
			}
		})
	}
}

// TestDefaultCacheKey_StillDistinguishesRealDifferences guards against a fix
// that avoids collisions by losing information: the key must still vary by
// contract, still vary by network, and still normalise query ordering.
func TestDefaultCacheKey_StillDistinguishesRealDifferences(t *testing.T) {
	reqA := httptest.NewRequest(http.MethodGet, "/v1/contracts/CABC/spec?b=2&a=1", nil)
	reqA.SetPathValue("id", "CABC")
	keyA, _ := middleware.DefaultCacheKey(reqA)

	// Same request with the query written in the other order: must normalise
	// to the same key.
	reqSame := httptest.NewRequest(http.MethodGet, "/v1/contracts/CABC/spec?a=1&b=2", nil)
	reqSame.SetPathValue("id", "CABC")
	keySame, _ := middleware.DefaultCacheKey(reqSame)
	if keyA != keySame {
		t.Errorf("query order changed the key: %q vs %q", keyA, keySame)
	}

	// Different contract on the same registered route: must not share an entry.
	reqOther := httptest.NewRequest(http.MethodGet, "/v1/contracts/COTHER/spec?a=1&b=2", nil)
	reqOther.SetPathValue("id", "COTHER")
	keyOther, _ := middleware.DefaultCacheKey(reqOther)
	if keyOther == keyA {
		t.Error("different contract ids produced the same cache key")
	}

	// Same path and query on a different network: must not share an entry.
	keyTestnet, _ := middleware.DefaultCacheKey(requestOnNetwork("/v1/contracts/CABC/spec?a=1", "testnet"))
	keyMainnet, _ := middleware.DefaultCacheKey(requestOnNetwork("/v1/contracts/CABC/spec?a=1", "mainnet"))
	if keyTestnet == keyMainnet {
		t.Error("different networks produced the same cache key")
	}
}
