// Package cursor provides opaque pagination cursor encoding and decoding.
// Cursors are base64url-encoded JSON payloads that wrap a raw pagingToken,
// hiding internal pagination details from API consumers.
package cursor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// payload is the internal JSON structure embedded in every opaque cursor.
type payload struct {
	V int    `json:"v"`
	T string `json:"t"`
}

// Encode takes a raw pagingToken string and returns an opaque, URL-safe
// base64-encoded cursor string that can be passed back to API consumers.
func Encode(pagingToken string) string {
	p := payload{V: 1, T: pagingToken}
	data, err := json.Marshal(p)
	if err != nil {
		// json.Marshal of a static struct with string fields cannot fail.
		panic(fmt.Sprintf("cursor: marshal failed: %v", err))
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(data)
}

// Decode takes an opaque cursor string produced by Encode and returns the
// underlying pagingToken. It returns an error if the cursor is malformed,
// cannot be base64-decoded, or carries an unrecognised version number.
func Decode(opaque string) (string, error) {
	if opaque == "" {
		return "", errors.New("cursor: empty cursor string")
	}
	if len(opaque) > 256 {
		return "", errors.New("cursor: payload too large")
	}

	data, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(opaque)
	if err != nil {
		return "", fmt.Errorf("cursor: base64 decode: %w", err)
	}

	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		return "", fmt.Errorf("cursor: json unmarshal: %w", err)
	}

	if p.V != 1 {
		return "", fmt.Errorf("cursor: unsupported version %d", p.V)
	}

	return p.T, nil
}

// EncodeKeyset opaquely encodes a (timestamp, id) compound sort key (issue
// #220), for keyset-paginating a listing ordered by a column that is not
// itself unique (e.g. created_at, where two rows can share a timestamp) —
// id is the tiebreaker that makes the pair a genuine total order, matching
// the WHERE (created_at, id) < ($1, $2) row-value comparison the query
// issues. Built on Encode, so it carries the same opacity/version guarantees.
func EncodeKeyset(t time.Time, id string) string {
	return Encode(t.UTC().Format(time.RFC3339Nano) + "," + id)
}

// DecodeKeyset is the inverse of EncodeKeyset.
func DecodeKeyset(opaque string) (t time.Time, id string, err error) {
	raw, err := Decode(opaque)
	if err != nil {
		return time.Time{}, "", err
	}
	// LastIndex rather than a fixed split: defensive against an id format
	// that might one day itself contain a comma, even though neither UUIDs
	// nor any id this is used with today do.
	idx := strings.LastIndex(raw, ",")
	if idx < 0 {
		return time.Time{}, "", errors.New("cursor: malformed keyset payload")
	}
	t, err = time.Parse(time.RFC3339Nano, raw[:idx])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor: keyset timestamp: %w", err)
	}
	return t, raw[idx+1:], nil
}
