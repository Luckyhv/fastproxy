package main

import "encoding/base64"

// secretKey is the shared secret used to obfuscate target URLs in the path.
// IMPORTANT: XOR is *obfuscation*, not encryption — it stops the URL from being
// readable/guessable in the path and ties payloads to your key, but it is not a
// security boundary. We keep this scheme only because your frontend + Bun proxy
// already generate payloads this exact way, so they stay compatible.
var secretKey = getenv("SECRET_KEY", "dj5D455Lzl2LKJXEtFwb5gy2oGFSYPnBKp7PTgFPm6Gn2MGb")

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

// EncodePayload turns (targetURL, referer, server) into a path-safe token.
// Layout before XOR:  <targetURL> 0x00 <referer> 0x00 <server>
//
// The third field is the PROVIDER KEY the API resolved this URL from ("uwu",
// "kiwi", "wave", …). It is what picks the upstream Origin/Referer, because the
// provider is stable while its CDN hostnames rotate constantly. It is optional:
// a token from an older API build has only two fields and still decodes, we just
// fall back to matching on the CDN hostname (see domains.go).
func EncodePayload(targetURL, referer, server string) string {
	payload := make([]byte, 0, len(targetURL)+len(referer)+len(server)+2)
	payload = append(payload, targetURL...)
	payload = append(payload, 0x00)
	payload = append(payload, referer...)
	if server != "" {
		payload = append(payload, 0x00)
		payload = append(payload, server...)
	}
	xorWithSecret(payload, secretKey)
	// RawURLEncoding = base64url WITHOUT '=' padding — safe inside a URL path and
	// byte-for-byte compatible with JS Buffer.toString("base64url").
	return base64.RawURLEncoding.EncodeToString(payload)
}

// DecodePayload reverses EncodePayload. ok=false means the token was malformed.
func DecodePayload(token string) (targetURL, referer, server string, ok bool) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", "", false
	}
	xorWithSecret(data, secretKey) // same call undoes the XOR

	sep := indexZero(data, 0)
	if sep == -1 {
		// No separator → whole thing is the URL, no referer, no server.
		return string(data), "", "", true
	}
	targetURL = string(data[:sep])

	rest := data[sep+1:]
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
