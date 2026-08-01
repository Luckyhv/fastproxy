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
//     not the site that gave us the URL. owocdn.top serves nothing unless the
//     request claims to come from kwik.cx, a completely unrelated domain.
//
// The lookup key is the SERVER NAME the API puts in the token ("uwu", "kiwi",
// "wave"). Not the CDN hostname: hostnames rotate constantly (owocdn.top cycles
// vault-N subdomains and swaps its apex domain), while the provider does not.

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
	origin  string // Origin header (no trailing slash)
	referer string // Referer header (with trailing slash)

	// noCache adds Cache-Control/Pragma: no-cache to the UPSTREAM request. Some
	// WAF configs treat a cache-revalidating fetch as more browser-like.
	noCache bool

	// http2 forces this provider onto the HTTP/2 client. We default to HTTP/1.1
	// everywhere for throughput (see transport.go), but some Cloudflare configs
	// refuse HTTP/1.1 outright: owocdn.top answers a 403 block page to an h1
	// request and 200 to the byte-identical h2 one.
	http2 bool
}

// servers is the single source of truth, keyed by the public provider key the
// API sends. Verified live against each provider's current CDN:
//
//	uwu  → owocdn.top 403s (Cloudflare block page) for EVERY origin except
//	       kwik.cx — including its own and the site that served the link — and
//	       refuses HTTP/1.1 regardless of headers.
//	kiwi → hls.anidb.app has no referer check. Its quirk is elsewhere: it serves
//	       TS segments as .xls / application/vnd.ms-excel (see headers.go).
//	wave → echovideo.to has no referer check. Its quirk is serving the playlist
//	       as image/jpeg from an extension-less path (see m3u8.go).
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
	"uwu":  {origin: "https://kwik.cx", referer: "https://kwik.cx/", noCache: true, http2: true},
	"kiwi": {origin: "https://hls.anidb.app", referer: "https://hls.anidb.app/"},
	"wave": {origin: "https://play.echovideo.ru", referer: "https://play.echovideo.ru/"},
	"megg": {origin: "https://www.animegg.org", referer: "https://www.animegg.org/"},
}

// hostAliases maps a CDN host suffix back to its provider, for the two cases
// where a request arrives without a server name:
//
//   - tokens minted before the API sent one. These outlive a deploy: the API
//     caches proxified sources (tokens and all) in Redis for a week.
//   - an upstream redirect that lands on a provider's CDN from somewhere else.
//
// Deliberately tiny: only the CDNs we actually serve. This used to be ~30
// regexp groups copied from another proxy, covering providers this codebase has
// no scraper for — dead config that still had to be scanned and cached per
// hostname. Suffix matching on a handful of entries is both faster and honest
// about what we support.
var hostAliases = map[string]string{
	"owocdn.top":   "uwu",
	"uwucdn.top":   "uwu",
	"anidb.app":    "kiwi",
	"echovideo.to": "wave",
	"echovideo.ru": "wave",
	"echovideo.cc": "wave",
	"vidcache.net": "megg",
	"animegg.org":  "megg",
}

// lookupServer resolves the provider identity for a request. The server name the
// API sent wins; otherwise we match the CDN hostname against hostAliases,
// including subdomains ("vault-12.owocdn.top" → "owocdn.top" → uwu).
func lookupServer(host, server string) (upstreamIdentity, bool) {
	if server != "" {
		if id, ok := servers[strings.ToLower(strings.TrimSpace(server))]; ok {
			return id, true
		}
	}

	host = strings.ToLower(host)
	// Walk the label boundaries instead of scanning the whole table: "a.b.c.d"
	// checks "a.b.c.d", "b.c.d", "c.d". At most a few map lookups, no regexp, no
	// per-hostname cache to grow unbounded.
	for {
		if name, ok := hostAliases[host]; ok {
			// An alias pointing at a provider we don't define would otherwise
			// return a zero identity and blank out Origin/Referer entirely —
			// worse than falling through to the token referer.
			if id, ok := servers[name]; ok {
				return id, true
			}
			return upstreamIdentity{}, false
		}
		dot := strings.IndexByte(host, '.')
		if dot < 0 {
			return upstreamIdentity{}, false
		}
		host = host[dot+1:]
	}
}

// applyUpstreamHeaders stamps the browser baseline plus the Origin/Referer this
// upstream demands, and reports whether it must be spoken to over HTTP/2.
//
// Precedence for Origin/Referer:
//  1. the provider identity (server name, then CDN hostname)
//  2. the referer carried in the token — whatever the scraper reported
//  3. the target's own origin — the safe "same-site" shape
//
// 1 beats 2 on purpose: a provider is only listed because the scraper-supplied
// value is absent or actively wrong for it (animex reports its own domain for
// uwu links, which is exactly what gets a 403).
func applyUpstreamHeaders(req *http.Request, target *url.URL, tokenReferer, server string) (useHTTP2 bool) {
	h := req.Header
	for k, v := range defaultHeaders {
		// Keys are already canonical, so this skips Set's canonicalization — the
		// only per-request work left is the map assign. The value slices are
		// SHARED with defaultHeaders, which is safe because every writer here
		// uses Set (replaces the slice) or Add (appends to a len==cap slice, so
		// it always reallocates). Never assign into h[k][i] in place.
		h[k] = v
	}

	if id, ok := lookupServer(target.Hostname(), server); ok {
		h.Set("Origin", id.origin)
		h.Set("Referer", id.referer)
		if id.noCache {
			h.Set("Cache-Control", "no-cache")
			h.Set("Pragma", "no-cache")
		}
		return id.http2
	}

	if tokenReferer != "" {
		h.Set("Referer", tokenReferer)
		if ref, err := url.Parse(tokenReferer); err == nil && ref.Host != "" {
			h.Set("Origin", ref.Scheme+"://"+ref.Host)
		}
		return false
	}

	origin := target.Scheme + "://" + target.Host
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
	return false
}
