package turnstate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"time"
)

const (
	fernetDecodedLength = 217
	fernetEncodedLength = 292
	minFernetTimestamp  = int64(946684800)  // 2000-01-01T00:00:00Z
	maxFernetTimestamp  = int64(4102444800) // 2100-01-01T00:00:00Z
)

// ParseFernet validates the complete envelope and returns its issuance time
// and a short stable fingerprint. Turn-state values are Fernet tokens with a
// fixed envelope shape; accepting a merely decodable value would allow a
// malformed response header to become injectable state.
func ParseFernet(value string) (issuedAt time.Time, fingerprint string, ok bool) {
	if len(value) != fernetEncodedLength || strings.IndexByte(value, '=') != len(value)-2 || strings.ContainsAny(value, "+/") {
		return time.Time{}, "", false
	}
	raw, err := base64.URLEncoding.Strict().DecodeString(value)
	if err != nil || len(raw) != fernetDecodedLength || raw[0] != 0x80 {
		return time.Time{}, "", false
	}
	secs := binary.BigEndian.Uint64(raw[1:9])
	if secs > uint64(maxFernetTimestamp) || secs < uint64(minFernetTimestamp) {
		return time.Time{}, "", false
	}
	issuedAt = time.Unix(int64(secs), 0).UTC()
	digest := sha256.Sum256([]byte(value))
	return issuedAt, hex.EncodeToString(digest[:8]), true
}

// FernetIssuedAt extracts the issuance time from a valid turn-state token.
func FernetIssuedAt(value string) (time.Time, bool) {
	issuedAt, _, ok := ParseFernet(value)
	return issuedAt, ok
}
