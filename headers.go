package main

import (
	"net/http"
	"net/url"
	"strings"
)

// allowedHosts is the Origin/Referer allow-list from ALLOWED_ORIGINS (comma
// separated). Empty = open to everyone (default). Entries are bare hosts; a
// match also covers subdomains, so "animetsu.net" allows "www.animetsu.net".
//
// NOTE: this is defense-in-depth, NOT a hard boundary. It runs on cache MISS
// only (Cloudflare HITs never reach us), and Origin/Referer can be spoofed by
// non-browser clients. For real anti-leech use signed/expiring tokens. It does
// cheaply stop casual browser embedding from other sites.
var allowedHosts = parseAllowedOrigins(getenv("ALLOWED_ORIGINS", ""))

func parseAllowedOrigins(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		if i := strings.Index(e, "://"); i >= 0 { // strip scheme
			e = e[i+3:]
		}
		if i := strings.IndexByte(e, '/'); i >= 0 { // strip path
			e = e[:i]
		}
		if i := strings.IndexByte(e, ':'); i >= 0 { // strip port
			e = e[:i]
		}
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// hostAllowed matches a host against the allow-list, including subdomains.
func hostAllowed(host string) bool {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, a := range allowedHosts {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// originAllowed decides whether a request may use the proxy. Open if no
// allow-list is configured. Otherwise the request must carry an Origin or
// Referer whose host is on the list; requests with neither are rejected (the
// common shape of a scraper / direct hotlink from a non-browser client).
func originAllowed(r *http.Request) bool {
	if len(allowedHosts) == 0 {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			return hostAllowed(u.Hostname())
		}
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host != "" {
			return hostAllowed(u.Hostname())
		}
	}
	return false
}

// withCORS wraps the handler: it stamps CORS headers on every response and
// answers preflight OPTIONS requests immediately (the browser sends these before
// a cross-origin GET with custom headers like Range). Doing it as middleware
// means every code path — stream, manifest, error — gets CORS for free.
func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Health check for load balancers — answered BEFORE the allow-list, since
		// CF's health monitor sends no Origin/Referer (which would otherwise 403).
		if r.URL.Path == "/health" {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		setCORS(w.Header())
		// Gate on the Origin/Referer allow-list (no-op when ALLOWED_ORIGINS unset).
		if !originAllowed(r) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent) // 204, no body
			return
		}
		next(w, r)
	}
}

// setCORS writes a flat, credential-less CORS policy.
//
// WHY wildcard + no credentials (this is deliberate and load-bearing):
// A media proxy never needs cookies. If we set Allow-Credentials:true we'd be
// forced to (a) echo the specific request Origin instead of "*", and (b) emit
// `Vary: Origin`. Both FRAGMENT the CDN cache: the edge would store a separate
// copy of every segment per origin, and most viewers would miss the cache. A
// flat "*" with NO Vary lets ONE cached segment be shared by every viewer — the
// difference between a 95% edge hit rate and ~0%.
func setCORS(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
	// Wildcards: valid because we send no credentials. "*" on Allow-Headers means
	// we never CORS-fail on a request header we forgot to list; "*" on
	// Expose-Headers lets the JS player read Content-Range/Length/Accept-Ranges
	// (needed for seeking + duration) without us enumerating each one.
	h.Set("Access-Control-Allow-Headers", "*")
	h.Set("Access-Control-Expose-Headers", "*")
	h.Set("Access-Control-Max-Age", "86400")
	// NOTE: intentionally no Access-Control-Allow-Credentials and no Vary.
}

// cacheableExt are the static assets safe to cache at the edge "forever". HLS
// segments and media files never change once published, so this is the bulk of
// our traffic and the bulk of our cache wins.
var cacheableExt = map[string]bool{
	"ts": true, "m4s": true, "mp4": true, "m4v": true, "mov": true, "webm": true,
	"m4a": true, "mp3": true, "aac": true, "vtt": true, "key": true,
	"xls": true,
	"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true,
}

func isCacheableAsset(path string) bool {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return false
	}
	return cacheableExt[strings.ToLower(path[i+1:])]
}

// isCacheableContentType is the fallback when the URL has no usable extension
// (common — our proxy paths are /stream/<token>, and many providers use
// extension-less segment URLs). A 200 of this content-type is static media and
// safe to cache hard. Manifests are handled separately, so they never reach here.
func isCacheableContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "font/") ||
		strings.Contains(ct, "mp2t") || // .ts segments
		strings.Contains(ct, "octet-stream") ||
		strings.Contains(ct, "vtt") // subtitles
}

// mpegTSContentType returns "video/mp2t" when the sniffed opening bytes are an
// MPEG-TS packet but the upstream declared something that clearly isn't video.
// 0x47 is the TS sync byte, at the head of every 188-byte packet. Returns "" to
// mean "leave the declared type alone" — we only correct a type we know is
// wrong, never one that's merely unfamiliar.
func mpegTSContentType(sniffed []byte, declared string) string {
	if len(sniffed) == 0 || sniffed[0] != 0x47 {
		return ""
	}
	d := strings.ToLower(declared)
	if strings.HasPrefix(d, "video/") || strings.HasPrefix(d, "audio/") {
		return "" // already a media type; trust it
	}
	return "video/mp2t"
}

func responseContentType(host, path string, h http.Header) string {
	ct := h.Get("Content-Type")
	lowerHost := strings.ToLower(host)
	lowerPath := strings.ToLower(path)
	if strings.HasSuffix(lowerPath, ".xls") &&
		(lowerHost == "hls.anidb.app" || strings.HasSuffix(lowerHost, ".anidb.app")) {
		return "video/mp2t"
	}
	return ct
}

// Cache policies. We set THREE headers so each layer obeys independently:
//
//	Cache-Control            -> the browser
//	CDN-Cache-Control        -> standards-compliant CDNs (Fastly, etc.)
//	Cloudflare-CDN-Cache-Control -> Cloudflare specifically
const (
	immutableForever = "public, max-age=31536000, s-maxage=31536000, immutable"
	manifestPolicy   = "public, max-age=300, s-maxage=14400" // browser 5m, edge 4h
	livePolicy       = "public, max-age=1, s-maxage=2"       // sliding live playlists
	noStore          = "no-store"
)

// setCC writes the same cache policy to all three header families.
func setCC(h http.Header, policy string) {
	h.Set("Cache-Control", policy)
	h.Set("CDN-Cache-Control", policy)
	h.Set("Cloudflare-CDN-Cache-Control", policy)
}

// setAssetCache picks a policy for a streamed (non-manifest) response.
//
// The critical rule: only cache a FULL 200 response. A 206 is a byte-range
// slice — caching it as the object corrupts seeking. The CDN serving a stored
// 206 to a later, different range request returns the wrong bytes, and a player
// that gets a truncated file (missing moov atom) shows zero duration and no
// Content-Range. That's the mp4 bug. The CDN still caches mp4 fine: when it wants
// to populate cache it fetches the FULL file (no Range) and gets the cacheable
// 200 below, then serves ranges out of that full object itself.
func setAssetCache(h http.Header, path string, status int, contentType string) {
	switch {
	case status == http.StatusPartialContent: // 206
		// Never cache a partial. Works for the client, just not stored at the edge.
		setCC(h, noStore)
	case status == http.StatusNotModified: // 304
		// A revalidation hit. Setting no-store here would tell the browser to drop
		// the copy it just validated — leave cache headers alone.
	case status != http.StatusOK: // 403/404/5xx
		// Never let a CDN pin an error — a transient upstream blip would otherwise
		// break that asset at the edge for a year.
		setCC(h, noStore)
	case isCacheableAsset(path) || isCacheableContentType(contentType):
		// full 200 of a media asset (by extension OR content-type)
		setCC(h, immutableForever)
	}
}
