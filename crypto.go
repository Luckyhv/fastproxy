package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"strconv"
	"sync"
	"time"
)

// Token format — MUST stay byte-identical to anikage-api src/utils/mediaProxy.ts.
//
//	base64url( flag | [expiry] | XOR(payload) | tag )
//
//	flag     1 byte: 0x01 = no expiry, 0x02 = 4-byte big-endian unix expiry follows
//	payload  <targetURL> 0x00 <referer> [0x00 <server>]
//	tag      first 16 bytes of HMAC-SHA256(macKey, everything before the tag)
//
// The XOR only hides the URL from casual reading; the HMAC tag is what makes a
// token unforgeable. The two keys are DERIVED from SECRET_KEY one-way, so the
// XOR keystream — which anyone can recover from a token whose URL they can
// guess — reveals nothing about the signing key or SECRET_KEY itself.
//
// Without expiry a token is deterministic (same URL → same token), so every
// cache keyed on the URL behaves exactly as before. With TOKEN_TTL_SEC set the
// expiry is rounded up to a 10-minute bucket, so tokens stay identical within a
// bucket instead of changing every second.

const (
	tokenFlagPlain   = 0x01
	tokenFlagExpires = 0x02
	tokenTagLen      = 16
	expiryBucketSec  = 600
)

// secretKey has NO default: a box started without it would otherwise sign with
// a key that is sitting in git. main() refuses to start when it is unset.
var secretKey = getenv("SECRET_KEY", "")

// tokenTTL is how long tokens fastproxy mints (playlist children) stay valid.
// 0 = never expire (default). Expired tokens are rejected whatever this is set
// to, so the API can turn expiry on independently.
var tokenTTL = time.Duration(atoiDefault(getenv("TOKEN_TTL_SEC", "0"), 0)) * time.Second

var xorKey, macKey = deriveTokenKeys(secretKey)

func deriveTokenKeys(secret string) (xor, mac []byte) {
	derive := func(label string) []byte {
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(label))
		return h.Sum(nil)
	}
	return derive("anikage-proxy/xor"), derive("anikage-proxy/mac")
}

func xorWith(data, key []byte) {
	for i := range data {
		data[i] ^= key[i%len(key)]
	}
}

// Each borrower owns its signer until the tag is copied out. Reset preserves
// the keyed HMAC state, avoiding fresh hash allocations for every segment.
type tokenMAC struct {
	hash hash.Hash
	sum  [sha256.Size]byte
}

func newTokenMACPool(key []byte) *sync.Pool {
	return &sync.Pool{New: func() any {
		return &tokenMAC{hash: hmac.New(sha256.New, key)}
	}}
}

var tokenMACPool = newTokenMACPool(macKey)

func tokenTag(signed []byte) [tokenTagLen]byte {
	m := tokenMACPool.Get().(*tokenMAC)
	m.hash.Reset()
	m.hash.Write(signed)
	digest := m.hash.Sum(m.sum[:0])
	var tag [tokenTagLen]byte
	copy(tag[:], digest)
	tokenMACPool.Put(m)
	return tag
}

// tokenExpiry is the expiry to stamp on a new token, or 0 for none.
func tokenExpiry(now time.Time) uint32 {
	if tokenTTL <= 0 {
		return 0
	}
	at := now.Add(tokenTTL).Unix()
	at = (at + expiryBucketSec - 1) / expiryBucketSec * expiryBucketSec
	return uint32(at)
}

// EncodePayload turns (targetURL, referer, server) into a signed, path-safe token.
func EncodePayload(targetURL, referer, server string) string {
	if tokenTTL <= 0 {
		return encodePayloadAt(targetURL, referer, server, 0)
	}
	return encodePayloadAt(targetURL, referer, server, tokenExpiry(time.Now()))
}

func encodePayloadAt(targetURL, referer, server string, expiry uint32) string {
	head := 1
	if expiry != 0 {
		head = 5
	}
	n := len(targetURL) + 1 + len(referer)
	if server != "" {
		n += 1 + len(server)
	}
	buf := make([]byte, head, head+n+tokenTagLen)
	if expiry != 0 {
		buf[0] = tokenFlagExpires
		binary.BigEndian.PutUint32(buf[1:5], expiry)
	} else {
		buf[0] = tokenFlagPlain
	}
	buf = append(buf, targetURL...)
	buf = append(buf, 0x00)
	buf = append(buf, referer...)
	if server != "" {
		buf = append(buf, 0x00)
		buf = append(buf, server...)
	}
	xorWith(buf[head:], xorKey)
	tag := tokenTag(buf)
	buf = append(buf, tag[:]...)
	// RawURLEncoding = base64url WITHOUT '=' padding — safe inside a URL path and
	// byte-for-byte compatible with JS Buffer.toString("base64url").
	return base64.RawURLEncoding.EncodeToString(buf)
}

// DecodePayload verifies and reverses EncodePayload. ok=false means the token
// is malformed, forged, or expired.
func DecodePayload(token string) (targetURL, referer, server string, ok bool) {
	return decodePayloadAt(token, time.Now())
}

func decodePayloadAt(token string, now time.Time) (targetURL, referer, server string, ok bool) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) < 1+tokenTagLen {
		return "", "", "", false
	}
	signed, tag := data[:len(data)-tokenTagLen], data[len(data)-tokenTagLen:]
	want := tokenTag(signed)
	if !hmac.Equal(tag, want[:]) {
		return "", "", "", false
	}

	var body []byte
	switch signed[0] {
	case tokenFlagPlain:
		body = signed[1:]
	case tokenFlagExpires:
		if len(signed) < 5 {
			return "", "", "", false
		}
		if int64(binary.BigEndian.Uint32(signed[1:5])) < now.Unix() {
			return "", "", "", false
		}
		body = signed[5:]
	default:
		return "", "", "", false
	}
	xorWith(body, xorKey) // in place: `data` is ours and not reused

	sep := indexZero(body, 0)
	if sep == -1 {
		return string(body), "", "", true
	}
	targetURL = string(body[:sep])
	rest := body[sep+1:]
	if sep2 := indexZero(rest, 0); sep2 != -1 {
		return targetURL, string(rest[:sep2]), string(rest[sep2+1:]), true
	}
	return targetURL, string(rest), "", true
}

func indexZero(data []byte, from int) int {
	for i := from; i < len(data); i++ {
		if data[i] == 0x00 {
			return i
		}
	}
	return -1
}

// tokenConfigError is non-empty when the signing config can't be trusted.
func tokenConfigError() string {
	if len(secretKey) < 16 {
		return "SECRET_KEY must be set to at least 16 characters (generate one with: openssl rand -base64 48)"
	}
	if tokenTTL < 0 {
		return "TOKEN_TTL_SEC must be >= 0, got " + strconv.Itoa(int(tokenTTL/time.Second))
	}
	return ""
}
