package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func headersFor(t *testing.T, rawURL, tokenReferer, server string) (http.Header, bool) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	h2 := applyUpstreamHeaders(req, u, tokenReferer, server)
	return req.Header, h2
}

// uwu: owocdn.top answers 403 (Cloudflare) for every origin except kwik.cx, and
// refuses HTTP/1.1 on top of that. Both have to come from one lookup.
func TestUwuGetsKwikOriginAndHTTP2(t *testing.T) {
	h, useHTTP2 := headersFor(t, "https://vault-12.owocdn.top/stream/12/13/abc/uwu.m3u8", "", "uwu")
	if got := h.Get("Referer"); got != "https://kwik.cx/" {
		t.Fatalf("Referer = %q, want https://kwik.cx/", got)
	}
	if got := h.Get("Origin"); got != "https://kwik.cx" {
		t.Fatalf("Origin = %q, want https://kwik.cx", got)
	}
	if !useHTTP2 {
		t.Fatal("uwu must be routed over HTTP/2")
	}
	if h.Get("Cache-Control") != "no-cache" || h.Get("Pragma") != "no-cache" {
		t.Fatal("uwu should send the no-cache pair upstream")
	}
}

// The provider table must beat a scraper-supplied referer, not lose to it:
// animex reports its own domain for uwu links, and that value is what 403s.
func TestServerBeatsTokenReferer(t *testing.T) {
	h, _ := headersFor(t, "https://vault-12.owocdn.top/x/uwu.m3u8", "https://animex.one/", "uwu")
	if got := h.Get("Referer"); got != "https://kwik.cx/" {
		t.Fatalf("Referer = %q, want the provider table to win", got)
	}
}

// The whole point of keying on the server name: it works for a CDN hostname the
// code has never seen, which is what happens when owocdn rotates domains.
func TestServerNameWorksForUnknownHost(t *testing.T) {
	h, useHTTP2 := headersFor(t, "https://vault-99.brand-new-cdn.example/x.m3u8", "https://animex.one/", "uwu")
	if got := h.Get("Origin"); got != "https://kwik.cx" {
		t.Fatalf("Origin = %q, want the uwu entry to win", got)
	}
	if !useHTTP2 {
		t.Fatal("http2 flag must follow the server name, not the hostname")
	}
}

func TestServerNameIsCaseAndSpaceInsensitive(t *testing.T) {
	h, _ := headersFor(t, "https://cdn.example.test/a.m3u8", "", "  UWU ")
	if got := h.Get("Origin"); got != "https://kwik.cx" {
		t.Fatalf("Origin = %q", got)
	}
}

func TestKiwiAndWaveIdentities(t *testing.T) {
	kiwi, kiwiH2 := headersFor(t, "https://hls.anidb.app/stream/abc/master.m3u8", "", "kiwi")
	if got := kiwi.Get("Referer"); got != "https://hls.anidb.app/" {
		t.Fatalf("kiwi Referer = %q", got)
	}
	if kiwiH2 {
		t.Fatal("kiwi should stay on HTTP/1.1 — h2 exists only for hosts that refuse h1")
	}
	wave, waveH2 := headersFor(t, "https://ru-cdn1.echovideo.to/cdn/abc?t.m3u8", "", "wave")
	if got := wave.Get("Referer"); got != "https://play.echovideo.ru/" {
		t.Fatalf("wave Referer = %q", got)
	}
	if waveH2 {
		t.Fatal("wave should stay on HTTP/1.1")
	}
}

// The server name is the ONLY key. A request without one falls straight through
// to the token referer — no hostname guessing, even for a CDN we know by sight.
func TestNoServerNameFallsThroughToTokenReferer(t *testing.T) {
	h, useHTTP2 := headersFor(t, "https://vault-12.owocdn.top/x/uwu.m3u8", "https://animex.example/", "")
	if got := h.Get("Referer"); got != "https://animex.example/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
	if got := h.Get("Origin"); got != "https://animex.example" {
		t.Fatalf("Origin = %q, want it derived from the token referer", got)
	}
	if useHTTP2 {
		t.Fatal("HTTP/2 is a per-provider opt-in; without a server name we stay on h1")
	}
}

// A name we don't have an entry for behaves the same way — it must not blank out
// the identity, because no referer at all is itself a 403 on most CDNs.
func TestUnknownServerNameKeepsTokenReferer(t *testing.T) {
	h, _ := headersFor(t, "https://cdn.example/x.m3u8", "https://player.example/", "not-a-real-server")
	if got := h.Get("Referer"); got != "https://player.example/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
}

// With no server name and no token referer, the target's own origin is the last
// resort — the safe "same-site" shape.
func TestNoServerAndNoRefererUsesTargetOrigin(t *testing.T) {
	h, _ := headersFor(t, "https://cdn.example/x.m3u8", "", "")
	if got := h.Get("Origin"); got != "https://cdn.example" {
		t.Fatalf("Origin = %q, want the target's own origin", got)
	}
	if got := h.Get("Referer"); got != "https://cdn.example/" {
		t.Fatalf("Referer = %q, want the target's own origin", got)
	}
}

// koto is deliberately NOT in the servers table. It fans out across several
// unrelated CDNs (megap.mikora.top, vidtub.shiora.site …) and each one demands
// the referer of the player page that served it — megaplay.buzz, vidtube.site —
// which the scraper already reports per source. A single "koto" identity
// (anikoto.to) overrides that correct value with one every CDN 403s, so the
// token referer has to win here.
func TestKotoKeepsItsPerSourceTokenReferer(t *testing.T) {
	for _, tc := range []struct{ url, tokenReferer string }{
		{"https://megap.mikora.top/abc/def/master.m3u8", "https://megaplay.buzz/"},
		{"https://vidtub.shiora.site/abc/master.m3u8", "https://vidtube.site/"},
	} {
		h, useHTTP2 := headersFor(t, tc.url, tc.tokenReferer, "koto")
		if got := h.Get("Referer"); got != tc.tokenReferer {
			t.Fatalf("%s -> Referer %q, want the token referer %q", tc.url, got, tc.tokenReferer)
		}
		wantOrigin := strings.TrimSuffix(tc.tokenReferer, "/")
		if got := h.Get("Origin"); got != wantOrigin {
			t.Fatalf("%s -> Origin %q, want %q", tc.url, got, wantOrigin)
		}
		if useHTTP2 {
			t.Fatalf("%s should stay on HTTP/1.1", tc.url)
		}
	}
}

// No table entry: fall back to the token referer, then the target's own origin.
func TestUnknownHostFallbacks(t *testing.T) {
	withToken, _ := headersFor(t, "https://cdn.example.test/a.m3u8", "https://player.example.test/", "")
	if got := withToken.Get("Referer"); got != "https://player.example.test/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
	if got := withToken.Get("Origin"); got != "https://player.example.test" {
		t.Fatalf("Origin = %q", got)
	}

	bare, _ := headersFor(t, "https://cdn.example.test/a.m3u8", "", "")
	if got := bare.Get("Referer"); got != "https://cdn.example.test/" {
		t.Fatalf("Referer = %q, want the target's own origin", got)
	}
}

// A bare "Mozilla/5.0" with no Accept/Sec-Fetch is a fingerprint no browser
// emits; every request must carry the full baseline.
func TestBrowserBaselineIsStamped(t *testing.T) {
	h, _ := headersFor(t, "https://cdn.example.test/a.m3u8", "", "")
	for _, k := range []string{"Accept", "Accept-Language", "Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Ch-Ua"} {
		if h.Get(k) == "" {
			t.Fatalf("missing %s", k)
		}
	}
	if ua := h.Get("User-Agent"); ua == "Mozilla/5.0" || ua == "" {
		t.Fatalf("User-Agent = %q, want a full browser UA", ua)
	}
}

// The baseline is stamped by sharing defaultHeaders' value slices. That is only
// safe if no request can mutate them, so prove one request can't leak into the
// next.
func TestBaselineIsNotMutatedAcrossRequests(t *testing.T) {
	first, _ := headersFor(t, "https://a.example.test/x.m3u8", "", "uwu")
	first.Set("User-Agent", "tampered")
	first.Add("Accept", "text/html")

	second, _ := headersFor(t, "https://b.example.test/x.m3u8", "", "")
	if got := second.Get("User-Agent"); got == "tampered" {
		t.Fatal("User-Agent leaked between requests")
	}
	if got := second["Accept"]; len(got) != 1 {
		t.Fatalf("Accept leaked between requests: %v", got)
	}
	if defaultHeaders.Get("Origin") != "" {
		t.Fatal("defaultHeaders picked up a per-request Origin")
	}
}
