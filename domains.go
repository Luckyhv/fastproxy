package main

import (
	"net/http"
	"net/url"
	"strings"
)

// ─── Upstream request identity ───────────────────────────────────────────────
//
// Video CDNs decide whether to serve bytes almost entirely from request headers.
// Two things matter, and we were getting both wrong:
//
//  1. The request has to LOOK like a browser. A bare "Mozilla/5.0" with no
//     Accept / Accept-Language / Sec-Fetch-* is a fingerprint no real Chrome
//     produces, and WAFs score it accordingly.
//
//  2. The Referer/Origin has to be the host's OWN player page — not our proxy,
//     not the site that gave us the URL. megaplay's CDNs serve nothing unless
//     the request claims to come from megaplay.buzz, an unrelated domain.
//
// The lookup key is the SERVER NAME the API puts in the token ("kiwi", "wave",
// …). Not the CDN hostname: hostnames rotate constantly while the provider
// does not.

// defaultHeaders is the browser-shaped baseline every upstream request starts
// from, pre-canonicalized so stamping it is a plain map copy with no per-request
// textproto canonicalization. Accept-Encoding is deliberately absent — the
// caller forces `identity` so gzip never wraps video bytes.
var defaultHeaders = http.Header{
	"User-Agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"},
	"Accept":             {"*/*"},
	"Accept-Language":    {"en-US,en;q=0.9"},
	"Sec-Ch-Ua":          {`"Chromium";v="126", "Not)A;Brand";v="24", "Google Chrome";v="126"`},
	"Sec-Ch-Ua-Mobile":   {"?0"},
	"Sec-Ch-Ua-Platform": {`"Windows"`},
	"Sec-Fetch-Dest":     {"empty"},
	"Sec-Fetch-Mode":     {"cors"},
	"Sec-Fetch-Site":     {"cross-site"},
}

// upstreamIdentity is everything a given provider's CDN demands of us.
type upstreamIdentity struct {
	origin  string // Origin header (no trailing slash); empty = send none
	referer string // Referer header (with trailing slash)

	// secFetchSite overrides the baseline's "cross-site". settlar's media host
	// answers `cross-site-hotlink-denied` to anything but the "same-site" a
	// browser sends when embed.settlar.io asks media.settlar.io.
	secFetchSite string

	// chromeTLS routes the provider through chromeClient: its hosts fingerprint
	// the TLS handshake and refuse Go's. Implies HTTP/2 and direct egress.
	chromeTLS bool

	// sessionCookie names the cookie that carries a per-session value from the
	// token (see splitServer). Empty = the provider needs none. Only the VALUE
	// travels in the token; the name is fixed here, so a token can never make
	// us send an arbitrary header.
	sessionCookie string
}

// splitServer separates the token's server field into the provider name and an
// optional per-session value: "kite~<value>". Everything that keys on the
// provider (identity, egress rules, the nero mint) uses the name alone; the
// whole field is what gets re-signed into child URLs, so a playlist's segments
// keep the session they were minted for.
func splitServer(server string) (name, session string) {
	if i := strings.IndexByte(server, '~'); i >= 0 {
		return server[:i], server[i+1:]
	}
	return server, ""
}

// validSession keeps the session value to cookie-safe characters.
func validSession(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// servers is the single source of truth, keyed by the public provider key the
// API sends. Verified live against each provider's current CDN:
//
//	kiwi → animex stopped offering kiwi itself; the API still tags animex
//	       SUBTITLES "kiwi", and they sit on megaplay-family CDNs (rotating
//	       *.top hosts) that 403 anything but a megaplay.buzz Referer —
//	       including the old hls.anidb.app identity, which no current host
//	       accepts (measured Sept 23 2026).
//	yuki → animex's dub sub-server: megap.* masters behind the same gate.
//	       Both send the Referer WITHOUT an Origin: every megaplay host takes
//	       that, and hls2.aniwatchtv.uk (also in kiwi's subtitle set) 403s any
//	       request carrying an Origin, whatever its value.
//	wave → echovideo.to has no referer check. Its quirk is serving the playlist
//	       as image/jpeg from an extension-less path (see m3u8.go).
//	pam  → *.settlar.io, ani.pm's player. Cloudflare refuses any non-Chrome TLS
//	       handshake (chrome.go); media.settlar.io additionally wants the
//	       embed's own Origin and a same-site fetch. Its tokens are not IP-bound,
//	       so the API minting the session and us redeeming it is fine.
//	pamapi → ani.pm's own JSON API, same TLS gate. The API cannot pass it from
//	       Bun, so it asks through us (see anikage-api's anipm scraper).
//	kite → anv.to serves playlists, segments and subtitles itself, from
//	       /stream/<token>. Each one 403s without the `shiro_watch` cookie of
//	       the session that asked for the episode, so the API puts that
//	       session's value after the name ("kite~<value>"). Not IP-bound
//	       (minted on v4, fetched on v6, Oct 2026). Segments are MPEG-TS behind
//	       a 70-byte PNG header, served as image/png; hls.js finds the sync
//	       byte and plays them.
//
// A provider belongs here ONLY when one fixed identity is right for every source
// it returns. koto is the counter-example and is deliberately absent: it fans out
// across unrelated CDNs (megap.mikora.top, vidtub.shiora.site, …) that each
// demand the referer of the player page that served them (megaplay.buzz,
// vidtube.site). The scraper already reports that per source, so pinning one
// "koto" identity here replaced a correct referer with one every koto CDN 403s.
// When the right value varies per source, let the token referer win.
//
// Add a provider here and it is wired end to end; there is nothing else to edit.
var servers = map[string]upstreamIdentity{
	"kiwi":   {referer: "https://megaplay.buzz/"},
	"yuki":   {referer: "https://megaplay.buzz/"},
	"wave":   {origin: "https://play.echovideo.ru", referer: "https://play.echovideo.ru/"},
	"megg":   {origin: "https://www.animegg.org", referer: "https://www.animegg.org/"},
	"pam":    {origin: "https://embed.settlar.io", referer: "https://embed.settlar.io/", secFetchSite: "same-site", chromeTLS: true},
	"pamapi": {origin: "https://ani.pm", referer: "https://ani.pm/", secFetchSite: "same-origin", chromeTLS: true},
	"kite":   {origin: "https://anv.to", referer: "https://anv.to/", secFetchSite: "same-origin", sessionCookie: "shiro_watch"},
}

// lookupServer resolves the provider identity from the server name the API put
// in the token. That name is the only key: the API attaches it to every source
// it proxifies, so there is nothing to guess from the hostname — and guessing
// was never reliable anyway, since a CDN's hosts rotate (owocdn.top cycles
// vault-N subdomains and swaps its apex domain) while the provider name doesn't.
//
// An unknown or absent name is not an error: the caller falls back to the token
// referer, which is the correct identity for every provider not listed above.
func lookupServer(server string) (upstreamIdentity, bool) {
	server, _ = splitServer(server)
	if server == "" {
		return upstreamIdentity{}, false
	}
	id, ok := servers[strings.ToLower(strings.TrimSpace(server))]
	return id, ok
}

// chromeServer reports whether this provider must go through chromeClient.
func chromeServer(server string) bool {
	id, ok := lookupServer(server)
	return ok && id.chromeTLS
}

// applyUpstreamHeaders stamps the browser baseline plus the Origin/Referer this
// upstream demands.
//
// Precedence for Origin/Referer:
//  1. the provider identity the server name selects
//  2. the referer carried in the token — whatever the scraper reported
//  3. the target's own origin — the safe "same-site" shape
//
// 1 beats 2 on purpose: a provider is only listed because the scraper-supplied
// value is absent or actively wrong for it (animex reports its own domain,
// which is exactly what gets a 403).
func applyUpstreamHeaders(req *http.Request, target *url.URL, tokenReferer, server string) {
	h := req.Header
	for k, v := range defaultHeaders {
		// Keys are already canonical, so this skips Set's canonicalization — the
		// only per-request work left is the map assign. The value slices are
		// SHARED with defaultHeaders, which is safe because every writer here
		// uses Set (replaces the slice) or Add (appends to a len==cap slice, so
		// it always reallocates). Never assign into h[k][i] in place.
		h[k] = v
	}

	if id, ok := lookupServer(server); ok {
		if id.origin != "" {
			h.Set("Origin", id.origin)
		}
		h.Set("Referer", id.referer)
		if id.secFetchSite != "" {
			h.Set("Sec-Fetch-Site", id.secFetchSite)
		}
		if id.sessionCookie != "" {
			if _, session := splitServer(server); validSession(session) {
				h.Set("Cookie", id.sessionCookie+"="+session)
			}
		}
		return
	}

	if tokenReferer != "" {
		h.Set("Referer", tokenReferer)
		if ref, err := url.Parse(tokenReferer); err == nil && ref.Host != "" {
			h.Set("Origin", ref.Scheme+"://"+ref.Host)
		}
		return
	}

	origin := target.Scheme + "://" + target.Host
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
}
