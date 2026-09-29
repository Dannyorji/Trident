package validation

import (
	"fmt"
	"strings"
)

var AllowedNetworks = map[string]bool{
	"mainnet":   true,
	"testnet":   true,
	"futurenet": true,
	"sandbox":   true,
}

// ValidateNetworkValue checks if the given network string is one of the supported values.
func ValidateNetworkValue(network string) error {
	lower := strings.ToLower(strings.TrimSpace(network))
	if !AllowedNetworks[lower] {
		return fmt.Errorf("invalid network %q: expected one of mainnet, testnet, futurenet, sandbox", network)
	}
	return nil
}
