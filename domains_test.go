package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func headersFor(t *testing.T, rawURL, tokenReferer, server string) http.Header {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	applyUpstreamHeaders(req, u, tokenReferer, server)
	return req.Header
}

// The provider table must beat a scraper-supplied referer, not lose to it:
// animex reports its own domain, and that value is what 403s.
func TestServerBeatsTokenReferer(t *testing.T) {
	h := headersFor(t, "https://ru-cdn1.echovideo.to/x.m3u8", "https://animex.one/", "wave")
	if got := h.Get("Referer"); got != "https://play.echovideo.ru/" {
		t.Fatalf("Referer = %q, want the provider table to win", got)
	}
}

// The whole point of keying on the server name: it works for a CDN hostname the
// code has never seen, which is what happens when a provider rotates domains.
func TestServerNameWorksForUnknownHost(t *testing.T) {
	h := headersFor(t, "https://vault-99.brand-new-cdn.example/x.m3u8", "https://animex.one/", "wave")
	if got := h.Get("Origin"); got != "https://play.echovideo.ru" {
		t.Fatalf("Origin = %q, want the wave entry to win", got)
	}
}

func TestServerNameIsCaseAndSpaceInsensitive(t *testing.T) {
	h := headersFor(t, "https://cdn.example.test/a.m3u8", "", "  WAVE ")
	if got := h.Get("Origin"); got != "https://play.echovideo.ru" {
		t.Fatalf("Origin = %q", got)
	}
}

// uwu is retired (its CDN no longer resolves): the name is now just unknown and
// must fall through to the token referer like any other.
func TestRetiredUwuFallsThrough(t *testing.T) {
	h := headersFor(t, "https://cdn.example.test/a.m3u8", "https://player.example/", "uwu")
	if got := h.Get("Referer"); got != "https://player.example/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
}

func TestKiwiAndWaveIdentities(t *testing.T) {
	// animex's kiwi subtitles and yuki dub masters sit behind megaplay's gate:
	// measured, only a megaplay.buzz Referer gets past it.
	for _, server := range []string{"kiwi", "yuki"} {
		h := headersFor(t, "https://megap.shiora.top/abc/def/master.m3u8", "", server)
		if got := h.Get("Referer"); got != "https://megaplay.buzz/" {
			t.Fatalf("%s Referer = %q, want megaplay.buzz", server, got)
		}
		if got := h.Get("Origin"); got != "" {
			t.Fatalf("%s sent Origin %q; aniwatchtv 403s any Origin", server, got)
		}
	}
	wave := headersFor(t, "https://ru-cdn1.echovideo.to/cdn/abc?t.m3u8", "", "wave")
	if got := wave.Get("Referer"); got != "https://play.echovideo.ru/" {
		t.Fatalf("wave Referer = %q", got)
	}
}

// The server name is the ONLY key. A request without one falls straight through
// to the token referer — no hostname guessing, even for a CDN we know by sight.
func TestNoServerNameFallsThroughToTokenReferer(t *testing.T) {
	h := headersFor(t, "https://vault-12.owocdn.top/x/uwu.m3u8", "https://animex.example/", "")
	if got := h.Get("Referer"); got != "https://animex.example/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
	if got := h.Get("Origin"); got != "https://animex.example" {
		t.Fatalf("Origin = %q, want it derived from the token referer", got)
	}
}

// A name we don't have an entry for behaves the same way — it must not blank out
// the identity, because no referer at all is itself a 403 on most CDNs.
func TestUnknownServerNameKeepsTokenReferer(t *testing.T) {
	h := headersFor(t, "https://cdn.example/x.m3u8", "https://player.example/", "not-a-real-server")
	if got := h.Get("Referer"); got != "https://player.example/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
}

// With no server name and no token referer, the target's own origin is the last
// resort — the safe "same-site" shape.
func TestNoServerAndNoRefererUsesTargetOrigin(t *testing.T) {
	h := headersFor(t, "https://cdn.example/x.m3u8", "", "")
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
		h := headersFor(t, tc.url, tc.tokenReferer, "koto")
		if got := h.Get("Referer"); got != tc.tokenReferer {
			t.Fatalf("%s -> Referer %q, want the token referer %q", tc.url, got, tc.tokenReferer)
		}
		wantOrigin := strings.TrimSuffix(tc.tokenReferer, "/")
		if got := h.Get("Origin"); got != wantOrigin {
			t.Fatalf("%s -> Origin %q, want %q", tc.url, got, wantOrigin)
		}
	}
}

// No table entry: fall back to the token referer, then the target's own origin.
func TestUnknownHostFallbacks(t *testing.T) {
	withToken := headersFor(t, "https://cdn.example.test/a.m3u8", "https://player.example.test/", "")
	if got := withToken.Get("Referer"); got != "https://player.example.test/" {
		t.Fatalf("Referer = %q, want the token referer", got)
	}
	if got := withToken.Get("Origin"); got != "https://player.example.test" {
		t.Fatalf("Origin = %q", got)
	}

	bare := headersFor(t, "https://cdn.example.test/a.m3u8", "", "")
	if got := bare.Get("Referer"); got != "https://cdn.example.test/" {
		t.Fatalf("Referer = %q, want the target's own origin", got)
	}
}

// A bare "Mozilla/5.0" with no Accept/Sec-Fetch is a fingerprint no browser
// emits; every request must carry the full baseline.
func TestBrowserBaselineIsStamped(t *testing.T) {
	h := headersFor(t, "https://cdn.example.test/a.m3u8", "", "")
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
	first := headersFor(t, "https://a.example.test/x.m3u8", "", "uwu")
	first.Set("User-Agent", "tampered")
	first.Add("Accept", "text/html")

	second := headersFor(t, "https://b.example.test/x.m3u8", "", "")
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

func TestEgressConfig(t *testing.T) {
	for _, k := range []string{"UPSTREAM_PROXY", "UPSTREAM_PROXIES", "UPSTREAM_PROXY_FILE", "UPSTREAM_PROXY_SERVERS", "UPSTREAM_PROXY_DOMAINS"} {
		t.Setenv(k, "")
	}
	t.Setenv("UPSTREAM_PROXIES", "http://user:secret@proxy.test:8080,http://user:secret@proxy.test:8080,socks5://other.test:1080")
	c, err := loadEgress()
	if err != nil || len(c.proxies) != 2 {
		t.Fatalf("load: %d %v", len(c.proxies), err)
	}
	if c.forced("a.test", "uwu") {
		t.Fatal("no filters must go direct first")
	}
	c.servers = []string{"*"}
	if !c.forced("a.test", "") {
		t.Fatal("* must proxy everything")
	}
	c.servers = []string{"uwu"}
	if !c.forced("a.test", "uwu") || c.forced("a.test", "wave") {
		t.Fatal("server filter")
	}
	c.domains = []string{"cdn.test"}
	if !c.forced("a.cdn.test", "") || c.forced("evilcdn.test", "") {
		t.Fatal("suffix boundary")
	}
	file := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(file, []byte("# pool\nhttps://file.test:8443\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UPSTREAM_PROXY_FILE", file)
	if loaded, err := loadEgress(); err != nil || len(loaded.proxies) != 3 {
		t.Fatalf("file load: %d %v", len(loaded.proxies), err)
	}
	t.Setenv("UPSTREAM_PROXY_FILE", "")

	t.Setenv("UPSTREAM_PROXIES", "ftp://user:secret@proxy.test")
	if _, err := loadEgress(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid URL must fail without credentials")
	}
	t.Setenv("UPSTREAM_PROXIES", "")
	t.Setenv("UPSTREAM_PROXY_FILE", "/missing-fastproxy-test-file")
	if _, err := loadEgress(); err == nil {
		t.Fatal("missing proxy file silently accepted")
	}
}

// pam: media.settlar.io answers `cross-site-hotlink-denied` unless the request
// looks like the embed asking its own sibling host, and every *.settlar.io host
// refuses a non-Chrome TLS handshake.
func TestPamGetsEmbedOriginSameSiteAndChromeTLS(t *testing.T) {
	h := headersFor(t, "https://media.settlar.io/v1/object/v2.abc", "https://ani.pm/", "pam")
	if got := h.Get("Origin"); got != "https://embed.settlar.io" {
		t.Fatalf("Origin = %q, want the embed origin", got)
	}
	if got := h.Get("Sec-Fetch-Site"); got != "same-site" {
		t.Fatalf("Sec-Fetch-Site = %q, want same-site", got)
	}
	if !chromeServer("pam") || !chromeServer("pamapi") {
		t.Fatal("settlar and ani.pm must go through the Chrome TLS client")
	}
}

// The override is per provider: everyone else keeps the browser baseline, and
// nobody else is moved onto the Chrome client by accident.
func TestSecFetchSiteAndChromeTLSStayScoped(t *testing.T) {
	for _, server := range []string{"uwu", "wave", "koto", ""} {
		h := headersFor(t, "https://cdn.example.test/a.m3u8", "", server)
		if got := h.Get("Sec-Fetch-Site"); got != "cross-site" {
			t.Fatalf("%q: Sec-Fetch-Site = %q, want the cross-site baseline", server, got)
		}
		if chromeServer(server) {
			t.Fatalf("%q must not use the Chrome TLS client", server)
		}
	}
}

// A 403 sample must say who refused and what we sent, and never leak the
// signed query string.
func TestForbiddenSampleRedactsQuery(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://megap.example.top/v1/0123456789abcdef0123456789abcdef/seg-1.ts?token=SECRET", nil)
	req.Header.Set("Referer", "https://megaplay.buzz/")
	resp := &http.Response{StatusCode: 403, Request: req, Header: http.Header{
		"Server": {"cloudflare"}, "Cf-Mitigated": {"challenge"}, "Content-Type": {"text/html"}}}
	site := httptest.NewRequest("GET", "http://proxy.test/x", nil)
	site.Header.Set("Origin", "https://gg.akage.lol")
	resp.Request = req.WithContext(context.WithValue(req.Context(), viewerSiteKey{}, viewerSite(site)))
	got := describeForbidden(resp, true)
	for _, want := range []string{"site=gg.akage.lol", `by="cloudflare cf-mitigated=challenge"`, "query=present", "referer=https://megaplay.buzz/", "proxied=true", "/v1/01234567…/seg-1.ts"} {
		if !strings.Contains(got, want) {
			t.Errorf("sample %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "SECRET") {
		t.Fatalf("sample leaked the query: %q", got)
	}
}

func TestSessionCookieTravelsWithTheServerName(t *testing.T) {
	target, _ := url.Parse("https://anv.to/stream/abc/index.m3u8?k=1")
	headers := func(server string) http.Header {
		req, _ := http.NewRequest(http.MethodGet, target.String(), nil)
		applyUpstreamHeaders(req, target, "https://ignored.example/", server)
		return req.Header
	}

	h := headers("kite~Ok39ck5h.1791919801.C-Qrsz_x")
	if got := h.Get("Cookie"); got != "shiro_watch=Ok39ck5h.1791919801.C-Qrsz_x" {
		t.Fatalf("Cookie = %q", got)
	}
	if h.Get("Origin") != "https://anv.to" || h.Get("Referer") != "https://anv.to/" || h.Get("Sec-Fetch-Site") != "same-origin" {
		t.Fatalf("identity not applied: %v", h)
	}

	// No session, or one with characters a cookie value cannot hold: the
	// identity still applies, no Cookie is sent.
	for _, server := range []string{"kite", "kite~", "kite~a b", "kite~x; other=1", "kite~x\r\nHost: evil"} {
		if got := headers(server).Get("Cookie"); got != "" {
			t.Fatalf("server %q sent Cookie %q", server, got)
		}
	}
	// A provider with no session cookie configured never sends one.
	if got := headers("megg~abc").Get("Cookie"); got != "" {
		t.Fatalf("megg sent Cookie %q", got)
	}

	// Child URLs are re-signed with the whole field, so segments keep the session.
	token := EncodePayload(target.String(), "", "kite~abc.123")
	_, _, server, ok := DecodePayload(token)
	if !ok || server != "kite~abc.123" {
		t.Fatalf("round trip = %q, %v", server, ok)
	}
	if name, session := splitServer(server); name != "kite" || session != "abc.123" {
		t.Fatalf("split = %q, %q", name, session)
	}
}
