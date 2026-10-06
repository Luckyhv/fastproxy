package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPayloadRoundTripsServerName(t *testing.T) {
	url, ref, sv, ok := DecodePayload(EncodePayload("https://cdn.test/a.m3u8", "https://player.test/", "uwu"))
	if !ok {
		t.Fatal("decode failed")
	}
	if url != "https://cdn.test/a.m3u8" || ref != "https://player.test/" || sv != "uwu" {
		t.Fatalf("round trip = %q, %q, %q", url, ref, sv)
	}
}

func TestTwoFieldPayloadDecodes(t *testing.T) {
	url, ref, sv, ok := DecodePayload(EncodePayload("https://cdn.test/a.m3u8", "https://player.test/", ""))
	if !ok || url != "https://cdn.test/a.m3u8" || ref != "https://player.test/" || sv != "" {
		t.Fatalf("round trip = %q, %q, %q, ok=%v", url, ref, sv, ok)
	}
}

func TestEmptyRefererWithServerName(t *testing.T) {
	url, ref, sv, ok := DecodePayload(EncodePayload("https://cdn.test/a.m3u8", "", "wave"))
	if !ok || url != "https://cdn.test/a.m3u8" || ref != "" || sv != "wave" {
		t.Fatalf("round trip = %q, %q, %q, ok=%v", url, ref, sv, ok)
	}
}

// No expiry → same input, same token, so URL-keyed caches keep hitting.
func TestTokensWithoutExpiryAreDeterministic(t *testing.T) {
	a := EncodePayload("https://cdn.test/seg1.ts", "https://p.test/", "uwu")
	b := EncodePayload("https://cdn.test/seg1.ts", "https://p.test/", "uwu")
	if a != b {
		t.Fatalf("tokens differ: %q vs %q", a, b)
	}
}

// Flipping any single byte — URL, flag or tag — must invalidate the token.
func TestTamperedTokenIsRejected(t *testing.T) {
	raw, _ := base64.RawURLEncoding.DecodeString(EncodePayload("https://cdn.test/a.m3u8", "", "uwu"))
	for i := range raw {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x01
		if _, _, _, ok := DecodePayload(base64.RawURLEncoding.EncodeToString(bad)); ok {
			t.Fatalf("byte %d flipped and the token still verified", i)
		}
	}
}

// The old attack: recover the XOR keystream from a token with a guessable URL,
// then build a token for a URL of our choosing. The keystream is recoverable
// but the tag is not, so the forgery must fail.
func TestRecoveredKeystreamCannotForge(t *testing.T) {
	known := "https://cdn.test/a.m3u8\x00\x00uwu"
	raw, _ := base64.RawURLEncoding.DecodeString(encodePayloadAt("https://cdn.test/a.m3u8", "", "uwu", 0))
	body := raw[1 : len(raw)-tokenTagLen]
	stream := make([]byte, len(body))
	for i := range body {
		stream[i] = body[i] ^ known[i]
	}

	evil := []byte("http://169.254.169.254/latest/meta-data/")
	forged := []byte{tokenFlagPlain}
	for i := range evil {
		forged = append(forged, evil[i]^stream[i%len(stream)])
	}
	forged = append(forged, raw[len(raw)-tokenTagLen:]...) // best guess: reuse a real tag
	if _, _, _, ok := DecodePayload(base64.RawURLEncoding.EncodeToString(forged)); ok {
		t.Fatal("forged token verified")
	}
}

func TestExpiryIsEnforced(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok := encodePayloadAt("https://cdn.test/a.m3u8", "", "", uint32(now.Unix()+60))
	if _, _, _, ok := decodePayloadAt(tok, now); !ok {
		t.Fatal("unexpired token rejected")
	}
	if _, _, _, ok := decodePayloadAt(tok, now.Add(2*time.Minute)); ok {
		t.Fatal("expired token accepted")
	}
}

func TestExpiryIsBucketedSoTokensStayStable(t *testing.T) {
	prev := tokenTTL
	tokenTTL = time.Hour
	defer func() { tokenTTL = prev }()

	base := time.Unix(1_800_000_000/expiryBucketSec*expiryBucketSec, 0) // bucket start
	if tokenExpiry(base.Add(time.Second)) != tokenExpiry(base.Add(9*time.Minute)) {
		t.Fatal("expiry changed inside one bucket")
	}
	if got := int64(tokenExpiry(base)); got < base.Add(time.Hour).Unix() {
		t.Fatalf("expiry %d earlier than TTL", got)
	}
}

// Shared vector with anikage-api src/utils/mediaProxy.test.ts. If this changes,
// the two sides no longer understand each other's tokens.
func TestCrossLanguageVector(t *testing.T) {
	prevX, prevM, prevPool := xorKey, macKey, tokenMACPool
	xorKey, macKey = deriveTokenKeys("test-secret-key-0123456789")
	tokenMACPool = newTokenMACPool(macKey)
	defer func() { xorKey, macKey, tokenMACPool = prevX, prevM, prevPool }()

	if got := encodePayloadAt("https://cdn.test/a.m3u8", "https://p.test/", "uwu", 0); got != crossLanguagePlain {
		t.Fatalf("plain vector = %q", got)
	}
	if got := encodePayloadAt("https://cdn.test/a.m3u8", "https://p.test/", "uwu", 1_900_000_000); got != crossLanguageExpiring {
		t.Fatalf("expiring vector = %q", got)
	}
}

const (
	crossLanguagePlain    = "AbGyPtsorRuK5L5KYBv7wLjj6TrY_UG3KmaheEqwQ8lFqeg-zijjG6XyrVE5J_Q46VweytOStOZ_RzED"
	crossLanguageExpiring = "AnE_swCxsj7bKK0biuS-SmAb-8C44-k62P1BtypmoXhKsEPJRanoPs4o4xul8q1R5MFR2-eUrj3f-mWv3QSjsA"
)

func TestTokenMACPoolIsSafeForConcurrentRequests(t *testing.T) {
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := fmt.Sprintf("https://cdn.test/segment-%d.ts", i)
			for j := 0; j < 100; j++ {
				token := encodePayloadAt(target, "https://player.test/", "wave", 0)
				got, ref, server, ok := DecodePayload(token)
				if !ok || got != target || ref != "https://player.test/" || server != "wave" {
					t.Errorf("worker %d: invalid round trip", i)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// Compare pooled signing to a fresh standard-library HMAC for varied lengths.
func TestTokenTagMatchesStandardHMAC(t *testing.T) {
	for _, size := range []int{0, 1, 32, 64, 65, 1024} {
		signed := bytes.Repeat([]byte{0x5a}, size)
		h := hmac.New(sha256.New, macKey)
		h.Write(signed)
		want := h.Sum(nil)[:tokenTagLen]
		got := tokenTag(signed)
		if !bytes.Equal(got[:], want) {
			t.Fatalf("incorrect HMAC for %d bytes", size)
		}
	}
}

func FuzzPayloadRoundTrip(f *testing.F) {
	f.Add("https://cdn.test/seg.ts", "https://player.test/", "wave", uint32(0))
	f.Add("", "", "", uint32(0))
	f.Add("https://cdn.test/index.m3u8?q=a%2Fb", "", "pam", uint32(1900000000))
	now := time.Unix(1800000000, 0)
	f.Fuzz(func(t *testing.T, target, referer, server string, expiry uint32) {
		if len(target)+len(referer)+len(server) > 8192 ||
			strings.ContainsRune(target+referer+server, 0) {
			t.Skip() // NUL is the token format's field delimiter.
		}
		token := encodePayloadAt(target, referer, server, expiry)
		got, ref, provider, ok := decodePayloadAt(token, now)
		if expiry != 0 && int64(expiry) < now.Unix() {
			if ok {
				t.Fatal("accepted an expired token")
			}
			return
		}
		if !ok || got != target || ref != referer || provider != server {
			t.Fatal("round trip changed token fields")
		}
	})
}
