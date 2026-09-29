package validation

import (
	"testing"
)

func TestValidateNetworkValue(t *testing.T) {
	tests := []struct {
		name    string
		net     string
		wantErr bool
	}{
		{name: "valid mainnet", net: "mainnet", wantErr: false},
		{name: "valid testnet", net: "testnet", wantErr: false},
		{name: "valid futurenet", net: "futurenet", wantErr: false},
		{name: "valid sandbox", net: "sandbox", wantErr: false},
		{name: "typo tesnet", net: "tesnet", wantErr: true},
		{name: "empty network", net: "", wantErr: true},
		{name: "unknown string", net: "foo", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNetworkValue(tt.net)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateNetworkValue(%q) error = %v, wantErr %v", tt.net, err, tt.wantErr)
			}
		})
	}
}
