package handlers

import (
	"encoding/base64"
	"fmt"
	"math/big"

	"github.com/stellar/go/strkey"
	"github.com/stellar/go/xdr"
)

// decodeScValJSON renders an xdr.ScVal as a best-effort generic JSON value.
//
// Scope (issue #264): the codebase has no contract-spec fetch/interpret
// system (no #260/#261 SCSpec decoder exists anywhere in this repo), so this
// decodes by the ScVal's own shape rather than by named spec fields. It
// covers the common cases a read-only balance/total_supply-style call
// returns: bool, integers up to 128-bit, bytes, string/symbol, address,
// vec, and map. Anything else (U256/I256, timepoint/duration, contract
// instance, etc.) falls back to a {"type": "..."} marker so the caller can
// still fall back to the raw XDR field in the response.
func decodeScValJSON(v xdr.ScVal) any {
	switch v.Type {
	case xdr.ScValTypeScvVoid:
		return nil
	case xdr.ScValTypeScvBool:
		if v.B != nil {
			return *v.B
		}
		return nil
	case xdr.ScValTypeScvU32:
		if v.U32 != nil {
			return uint32(*v.U32)
		}
	case xdr.ScValTypeScvI32:
		if v.I32 != nil {
			return int32(*v.I32)
		}
	case xdr.ScValTypeScvU64:
		if v.U64 != nil {
			return uint64(*v.U64)
		}
	case xdr.ScValTypeScvI64:
		if v.I64 != nil {
			return int64(*v.I64)
		}
	case xdr.ScValTypeScvU128:
		if v.U128 != nil {
			return u128ToString(*v.U128)
		}
	case xdr.ScValTypeScvI128:
		if v.I128 != nil {
			return i128ToString(*v.I128)
		}
	case xdr.ScValTypeScvU256:
		if v.U256 != nil {
			return u256ToString(*v.U256)
		}
	case xdr.ScValTypeScvI256:
		if v.I256 != nil {
			return i256ToString(*v.I256)
		}
	case xdr.ScValTypeScvTimepoint:
		if v.Timepoint != nil {
			return fmt.Sprintf("%d", *v.Timepoint)
		}
	case xdr.ScValTypeScvDuration:
		if v.Duration != nil {
			return fmt.Sprintf("%d", *v.Duration)
		}
	case xdr.ScValTypeScvError:
		// xdr.ScError is a union with no String method of its own. Render the
		// arm the discriminant selects: contract errors carry a numeric code,
		// every other type carries an ScErrorCode.
		if v.Error != nil {
			if v.Error.Type == xdr.ScErrorTypeSceContract && v.Error.ContractCode != nil {
				return fmt.Sprintf("%s(%d)", v.Error.Type.String(), *v.Error.ContractCode)
			}
			if v.Error.Code != nil {
				return fmt.Sprintf("%s(%s)", v.Error.Type.String(), v.Error.Code.String())
			}
			return v.Error.Type.String()
		}
	case xdr.ScValTypeScvBytes:
		if v.Bytes != nil {
			return base64.StdEncoding.EncodeToString(*v.Bytes)
		}
	case xdr.ScValTypeScvString:
		if v.Str != nil {
			return string(*v.Str)
		}
	case xdr.ScValTypeScvSymbol:
		if v.Sym != nil {
			return string(*v.Sym)
		}
	case xdr.ScValTypeScvAddress:
		if v.Address != nil {
			if addr, err := scAddressToString(*v.Address); err == nil {
				return addr
			}
		}
	case xdr.ScValTypeScvVec:
		if v.Vec != nil && *v.Vec != nil {
			items := make([]any, 0, len(**v.Vec))
			for _, elem := range **v.Vec {
				items = append(items, decodeScValJSON(elem))
			}
			return items
		}
		return []any{}
	case xdr.ScValTypeScvMap:
		if v.Map != nil && *v.Map != nil {
			entries := make([]map[string]any, 0, len(**v.Map))
			for _, entry := range **v.Map {
				entries = append(entries, map[string]any{
					"key":   decodeScValJSON(entry.Key),
					"value": decodeScValJSON(entry.Val),
				})
			}
			return entries
		}
		return []map[string]any{}
	}

	// Unsupported/unrecognized shape: report the discriminant so the client
	// knows to fall back to decoding the raw XDR itself.
	return map[string]string{"type": v.Type.String()}
}

// u128ToString renders an unsigned 128-bit ScVal as a base-10 string (too
// large for JSON number precision to round-trip safely).
func u128ToString(v xdr.UInt128Parts) string {
	hi := new(big.Int).SetUint64(uint64(v.Hi))
	hi.Lsh(hi, 64)
	lo := new(big.Int).SetUint64(uint64(v.Lo))
	return hi.Add(hi, lo).String()
}

// i128ToString renders a signed 128-bit ScVal as a base-10 string. The value
// is (Hi << 64) + Lo, where Hi carries the sign and Lo is always the
// unsigned low-order magnitude (two's complement composition).
func i128ToString(v xdr.Int128Parts) string {
	full := new(big.Int).Lsh(big.NewInt(int64(v.Hi)), 64)
	full.Add(full, new(big.Int).SetUint64(uint64(v.Lo)))
	return full.String()
}

// u256ToString renders an unsigned 256-bit ScVal as a base-10 string.
func u256ToString(v xdr.UInt256Parts) string {
	res := new(big.Int).SetUint64(uint64(v.HiHi))
	res.Lsh(res, 64)
	res.Add(res, new(big.Int).SetUint64(uint64(v.HiLo)))
	res.Lsh(res, 64)
	res.Add(res, new(big.Int).SetUint64(uint64(v.LoHi)))
	res.Lsh(res, 64)
	res.Add(res, new(big.Int).SetUint64(uint64(v.LoLo)))
	return res.String()
}

// i256ToString renders a signed 256-bit ScVal as a base-10 string.
func i256ToString(v xdr.Int256Parts) string {
	res := big.NewInt(int64(v.HiHi))
	res.Lsh(res, 64)
	res.Add(res, new(big.Int).SetUint64(uint64(v.HiLo)))
	res.Lsh(res, 64)
	res.Add(res, new(big.Int).SetUint64(uint64(v.LoHi)))
	res.Lsh(res, 64)
	res.Add(res, new(big.Int).SetUint64(uint64(v.LoLo)))
	return res.String()
}

// scAddressToString renders an ScAddress (account or contract) as its
// strkey form.
func scAddressToString(addr xdr.ScAddress) (string, error) {
	switch addr.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		if addr.AccountId == nil {
			return "", fmt.Errorf("nil account id")
		}
		return addr.AccountId.Address(), nil
	case xdr.ScAddressTypeScAddressTypeContract:
		if addr.ContractId == nil {
			return "", fmt.Errorf("nil contract id")
		}
		return strkey.Encode(strkey.VersionByteContract, addr.ContractId[:])
	default:
		return "", fmt.Errorf("unsupported address type %v", addr.Type)
	}
}
