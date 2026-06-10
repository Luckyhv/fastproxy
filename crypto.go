package main

import "encoding/base64"

// secretKey is the shared secret used to obfuscate target URLs in the path.
// IMPORTANT: XOR is *obfuscation*, not encryption — it stops the URL from being
// readable/guessable in the path and ties payloads to your key, but it is not a
// security boundary. We keep this scheme only because your frontend + Bun proxy
// already generate payloads this exact way, so they stay compatible.
var secretKey = getenv("SECRET_KEY", "aproxy2026")

// xorWithSecret flips each byte against the repeating key, in place. XOR is its
// own inverse, so the SAME function both encodes and decodes.
func xorWithSecret(data []byte, secret string) {
	if len(secret) == 0 {
		return
	}
	for i := range data {
		data[i] ^= secret[i%len(secret)]
	}
}

// EncodePayload turns (targetURL, referer) into a path-safe token.
// Layout before XOR:  <targetURL bytes> 0x00 <referer bytes>
// The 0x00 is a separator so Decode knows where the URL ends and referer begins.
func EncodePayload(targetURL, referer string) string {
	payload := make([]byte, len(targetURL)+1+len(referer))
	copy(payload, targetURL)                  // [0 .. len(url))      = url
	payload[len(targetURL)] = 0x00            // [len(url)]           = separator
	copy(payload[len(targetURL)+1:], referer) // [len(url)+1 ..]   = referer
	xorWithSecret(payload, secretKey)
	// RawURLEncoding = base64url WITHOUT '=' padding — safe inside a URL path and
	// byte-for-byte compatible with JS Buffer.toString("base64url").
	return base64.RawURLEncoding.EncodeToString(payload)
}

// DecodePayload reverses EncodePayload. ok=false means the token was malformed.
func DecodePayload(token string) (targetURL, referer string, ok bool) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", false
	}
	xorWithSecret(data, secretKey) // same call undoes the XOR

	// Find the 0x00 separator.
	sep := -1
	for i, b := range data {
		if b == 0x00 {
			sep = i
			break
		}
	}
	if sep == -1 {
		// No separator → whole thing is the URL, no referer.
		return string(data), "", true
	}
	return string(data[:sep]), string(data[sep+1:]), true
}
