package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
)

// isPublicHost is the cheap, no-DNS SSRF check on the target hostname: reject
// "localhost" and literal private/loopback IPs outright for a clean 403. Real
// hostnames pass here and are validated against their RESOLVED ip by the
// transport's dialControl (which also catches DNS rebinding).
func isPublicHost(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPublicIP(ip)
	}
	return true // hostname — dialControl validates the resolved IP
}

// bufPool hands out 1 MB scratch buffers and takes them back when a stream ends,
// so we don't allocate (and then garbage-collect) a fresh buffer for every
// request. Buffer size is a knob: smaller = less RAM per concurrent stream (more
// viewers per box), larger = fewer read/write syscalls (more throughput per big
// file). 1 MB favors throughput; autoTune()'s perStreamBudget is sized to match.
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1024*1024) // 1 MB
		return &b
	},
}

// headersToForward are the upstream response headers we pass through to the
// client. We allow-list rather than copy everything, so hop-by-hop and
// origin-leaking headers never reach the viewer.
//
// These MUST stay in Go's canonical form (http.CanonicalHeaderKey: first letter
// and every letter after a hyphen capitalized, the rest lowered — so "Etag", NOT
// "ETag"). The copy loop indexes both header maps directly to skip textproto
// canonicalization on every request, and a non-canonical key here would simply
// never match. TestHeadersToForwardAreCanonical guards this.
var headersToForward = []string{
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"Last-Modified",
	"Etag",
}

// allowRawURL enables plain-URL mode (/stream?url=...). On by default so any
// HLS stream can be proxied without pre-encoding a token; set ALLOW_RAW_URL=0
// to require tokens (i.e. knowledge of SECRET_KEY) instead.
var allowRawURL = getenv("ALLOW_RAW_URL", "1") != "0"

// readerCloser lets us swap a response body's Reader (e.g. after sniffing a few
// bytes) while still closing the ORIGINAL body to release the connection.
type readerCloser struct {
	io.Reader
	io.Closer
}

// writerOnly hides every method of a ResponseWriter except Write.
//
// This is load-bearing, not cosmetic. io.CopyBuffer only uses the buffer you
// hand it when NEITHER side offers a shortcut: if the destination implements
// io.ReaderFrom it calls dst.ReadFrom(src) and DROPS the buffer on the floor.
// net/http's *response does implement ReaderFrom, so a bare
// io.CopyBuffer(w, body, oneMegBuffer) silently ignored our 1 MB buffer and ran
// the transfer through net/http's internal 32 KB one — and for a non-chunked
// response it went further, to net.TCPConn.ReadFrom, which (with an HTTP body
// as src, so neither splice nor sendfile can apply) falls back to io.Copy and
// allocates yet another 32 KB buffer per stream.
//
// That ReaderFrom shortcut exists to reach sendfile/splice, and neither can
// ever fire here: our source is an HTTP response body, not a file or a raw TCP
// socket. So hiding it costs nothing and buys 32x larger reads and writes:
// +31% on a 4 MB segment and +52% on a 32 MB file over loopback, where the
// network is free — see BenchmarkStreamReaderFrom vs BenchmarkStreamPooledBuf.
// On a real path the win shows up as CPU per byte, i.e. streams per box.
type writerOnly struct{ io.Writer }

// knownRoutes are the path prefixes the player will hit. The encrypted token is
// the segment right after the prefix. We keep a small set so a bare /<token>
// also works.
var knownRoutes = map[string]bool{"stream": true, "m3u8": true, "hls": true}

// extractPayload pulls the encrypted token out of the request path. It handles
//
//	/stream/<token>          -> <token>
//	/stream/<token>/seg.jpg  -> <token>   (suffixes added in the m3u8 part)
//	/<token>                 -> <token>
func extractPayload(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.SplitN(path, "/", 2)
	if knownRoutes[parts[0]] {
		if len(parts) < 2 {
			return ""
		}
		// Drop any trailing /suffix the player appended.
		return strings.SplitN(parts[1], "/", 2)[0]
	}
	return strings.SplitN(parts[0], "/", 2)[0]
}

// maxManifestSize caps how much of a "playlist" we'll buffer. A real .m3u8 is a
// few KB; this guard stops a malicious/mislabeled upstream from making us read a
// multi-GB body into memory (which would defeat the whole backpressure design).
const maxManifestSize = 10 << 20 // 10 MiB

// serveManifest reads the (small) playlist fully, rewrites its child URLs back
// through us, and writes the result. Unlike the streaming path this DOES buffer
// the whole body — but only up to maxManifestSize, and only for manifests.
func proxyBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf == "http" || xf == "https" {
		scheme = xf
	}
	return scheme + "://" + r.Host
}

// proxyRoute picks the path prefix a rewritten child URL gets. Playlists go to
// /m3u8/ so the next hop knows to rewrite again; everything else streams.
func proxyRoute(targetURL string) string {
	if isM3U8Ref(targetURL) {
		return "/m3u8/"
	}
	return "/stream/"
}

func proxyURLFor(r *http.Request, targetURL, referer, server string) string {
	return proxyBaseURL(r) + proxyRoute(targetURL) + EncodePayload(targetURL, referer, server)
}

func serveManifest(w http.ResponseWriter, r *http.Request, resp *http.Response, base *url.URL, referer, server string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
	if err != nil {
		http.Error(w, "manifest read failed", http.StatusBadGateway)
		return
	}

	// Guard: if it doesn't actually start with #EXTM3U, it was mislabeled — pass
	// it through untouched rather than corrupting it. We only buffered the first
	// maxManifestSize bytes, so stream whatever is left instead of silently
	// serving a truncated file (a big asset behind a .m3u8 URL would otherwise
	// arrive cut off at exactly 10 MiB, with a 200).
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U")) {
		// A media body behind a manifest-looking URL still gets the masquerade,
		// so this path can't become a hole that advertises video/*.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			if masked := maskedContentType(ct, resp.StatusCode); masked != "" {
				ct = masked
			}
			w.Header().Set("Content-Type", ct)
		}
		setCC(w.Header(), noStore) // not a playlist and not a known asset — don't pin it
		w.WriteHeader(resp.StatusCode)
		if _, err := w.Write(body); err == nil && len(body) == maxManifestSize {
			buf := bufPool.Get().(*[]byte)
			defer bufPool.Put(buf)
			_, _ = io.CopyBuffer(writerOnly{w}, resp.Body, *buf)
		}
		return
	}

	// encode closes over `referer` and `server` so every child URL we emit carries
	// the same identity — segments and keys have to forge the origin exactly like
	// the manifest did, and they inherit it from here. The proxy base is hoisted
	// out: it is the same for every line, and rebuilding it per segment cost an
	// allocation on each of a 1200-line playlist's rows.
	self := proxyBaseURL(r)
	encode := func(uri string) string {
		return self + proxyRoute(uri) + EncodePayload(uri, referer, server)
	}
	rewritten := rewritePlaylist(body, base, encode)

	h := w.Header()
	h.Set("Content-Type", "application/vnd.apple.mpegurl")
	// Cache policy depends on the playlist KIND. Master playlists and finished
	// (VOD) media playlists never change → short-browser/long-edge cache so a
	// burst of viewers shares one fetch. A live/event playlist (media playlist
	// with no #EXT-X-ENDLIST) slides every few seconds — caching it at the edge
	// for hours would freeze the stream, so it gets (near) no cache.
	switch {
	case resp.StatusCode != http.StatusOK:
		setCC(h, noStore)
	case bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) || // master
		bytes.Contains(body, []byte("#EXT-X-ENDLIST")): // finished VOD
		setCC(h, manifestPolicy)
	default: // live / sliding-window media playlist
		setCC(h, livePolicy)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(rewritten)
}

// rawURLQuery returns the parsed query when this is a raw-URL request
// (/stream?url=...), and nil when it is not — so the token path never pays for
// parsing a query string it doesn't have.
func rawURLQuery(r *http.Request) url.Values {
	if !allowRawURL || r.URL.RawQuery == "" {
		return nil
	}
	q := r.URL.Query()
	if q.Get("url") == "" {
		return nil
	}
	return q
}

// inFlight optionally caps how many streams we serve at once. Backpressure
// already bounds RAM per stream (~1 MB buffer), but a hard ceiling protects
// against running out of file descriptors / sockets under a stampede. It is
// sized by autoTune() at startup (from MAX_CONCURRENT, or derived from detected
// RAM). nil = unlimited. When full we shed load with 503 rather than queueing.
var inFlight chan struct{}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	if inFlight != nil {
		select {
		case inFlight <- struct{}{}: // got a slot
			defer func() { <-inFlight }()
		default:
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
	}

	// 1. Resolve the target. Two ways in:
	//    a) plain-URL mode: /stream?url=<absolute-url>&ref=<referer> — the easy
	//       way to proxy any HLS stream. Disable with ALLOW_RAW_URL=0 if you only
	//       want tokenized (key-tied) access.
	//    b) token mode: /stream/<encrypted-payload> (what the frontend generates).
	//    Token links carry no query string at all, which is the overwhelming
	//    majority of our traffic (one request per segment). Gating on RawQuery
	//    keeps url.Query()'s parse + map allocation off that path entirely.
	var rawURL, referer, server string
	if q := rawURLQuery(r); q != nil {
		rawURL = q.Get("url")
		referer = q.Get("ref")
		if referer == "" {
			referer = q.Get("referer")
		}
		// Raw-URL mode gets the same provider steering as a token: ?server=uwu.
		server = q.Get("server")
	} else {
		token := extractPayload(r.URL.Path)
		if token == "" {
			http.Error(w, "missing payload", http.StatusBadRequest)
			return
		}
		var ok bool
		rawURL, referer, server, ok = DecodePayload(token)
		if !ok {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
	}
	target, err := url.Parse(rawURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	// SSRF guard: refuse targets that resolve to loopback/private/link-local
	// space, so a forged token can't make us fetch internal services or cloud
	// metadata (169.254.169.254). External video CDNs are always public IPs.
	if !isPublicHost(target.Hostname()) {
		http.Error(w, "forbidden target", http.StatusForbidden)
		return
	}

	// 2. Build the upstream request bound to THIS client's context.
	//    r.Context() is cancelled the instant the viewer disconnects (closed tab,
	//    seek, quality switch). That cancellation propagates into the fetch and
	//    aborts it — freeing the upstream socket and its buffers immediately.
	//    This is exactly the disconnect-cleanup the Bun version leaked on.
	//    Forward the client's method + body. POST is rare for video but some
	//    sources need it; HEAD we send upstream as GET (many hosts don't support
	//    HEAD) and simply drop the body on the way back.
	upstreamMethod := r.Method
	var reqBody io.Reader
	if r.Method == http.MethodPost {
		reqBody = r.Body
	} else if r.Method == http.MethodHead {
		upstreamMethod = http.MethodGet
	}
	req, err := http.NewRequestWithContext(r.Context(), upstreamMethod, target.String(), reqBody)
	if err != nil {
		http.Error(w, "bad request", http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
	}

	// 3. Forward Range so the player can seek / load partial byte ranges, and ask
	//    upstream for identity encoding (we never want gzip wrapping video bytes).
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	// Conditional headers let the browser/edge revalidate instead of re-download:
	// upstream answers 304 (no body) and we pass it straight through.
	for _, k := range []string{"If-None-Match", "If-Modified-Since", "If-Range"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	// Many video hosts only serve bytes when the request both LOOKS like a real
	// browser and claims to come from that host's own player page. Both of those
	// live in domains.go, keyed by the provider name the token carries — the CDN
	// hostnames rotate, the provider doesn't. Falls back to the token referer,
	// then to the target's own origin.
	useHTTP2 := applyUpstreamHeaders(req, target, referer, server)
	req.Header.Set("Accept-Encoding", "identity")

	// A couple of providers sit behind a Cloudflare config that 403s HTTP/1.1
	// outright, so they get the h2 client; everyone else keeps h1 for throughput.
	client := httpClient
	if useHTTP2 {
		client = h2Client
	}

	// 4. Fire the upstream fetch. resp.Body is a STREAM, not the full payload —
	//    nothing has been downloaded into memory yet at this line.
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	// 4a. Redirect? Upstream often 302s a segment to a signed CDN URL. Follow it
	//     SERVER-SIDE (up to 5 hops): each browser-side bounce would cost a full
	//     extra client round-trip per segment. Every hop re-passes the SSRF guard
	//     (plus dialControl on the resolved IP). Only safe for GET/HEAD — a POST
	//     body was already consumed, so those fall back to a client redirect.
	for hops := 0; hops < 5 && resp.StatusCode >= 300 && resp.StatusCode < 400 && r.Method != http.MethodPost; hops++ {
		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}
		abs := resolve(loc, target)
		resp.Body.Close() // release the redirect response's connection
		if (abs.Scheme != "http" && abs.Scheme != "https") || !isPublicHost(abs.Hostname()) {
			http.Error(w, "forbidden redirect target", http.StatusForbidden)
			return
		}
		target = abs
		nreq, nerr := http.NewRequestWithContext(r.Context(), upstreamMethod, target.String(), nil)
		if nerr != nil {
			http.Error(w, "bad redirect", http.StatusBadGateway)
			return
		}
		nreq.Header = req.Header // same forged headers on every hop
		req = nreq
		resp, err = client.Do(req)
		if err != nil {
			http.Error(w, "upstream fetch failed", http.StatusBadGateway)
			return
		}
	}

	defer resp.Body.Close() // always release the upstream connection

	// Still 3xx (POST, redirect loop, or missing Location)? Re-wrap the Location
	// through us and let the client bounce — never expose the origin URL.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			abs := resolve(loc, target)
			http.Redirect(w, r, proxyURLFor(r, abs.String(), referer, server), resp.StatusCode)
			return
		}
	}

	// 4b. Is this an HLS manifest? If so, we must read it, rewrite every child
	//     URL to point back through us, and serve the modified text — NOT stream
	//     it raw. Detect by the target's extension or the response content-type;
	//     when both are ambiguous (extension-less URL + text/octet-stream
	//     content-type, common on shady hosts) sniff the first bytes for #EXTM3U.
	ct := resp.Header.Get("Content-Type")
	isManifest := isM3U8URL(target) || isM3U8ContentType(ct)
	// Sniff whenever the URL gives us no usable extension. We deliberately do NOT
	// gate on content-type here: the wave CDN serves its master playlist as
	// `image/jpeg` from an extension-less path, so trusting either signal alone
	// streams the playlist through raw — child URLs never rewritten, and the
	// player gets an "image" it can't parse. The peek is 7 bytes and is stitched
	// straight back onto the stream, so it costs nothing when we're wrong.
	var sniffed []byte
	if !isManifest && resp.StatusCode == http.StatusOK && r.Method != http.MethodHead && !isCacheableAsset(target.Path) {
		peek := make([]byte, len("#EXTM3U"))
		n, _ := io.ReadFull(resp.Body, peek)
		sniffed = peek[:n]
		isManifest = string(sniffed) == "#EXTM3U"
		if isManifest {
			// Only the manifest path needs the peeked bytes stitched back onto
			// the reader, because serveManifest reads the body as one unit. The
			// streaming path deliberately does NOT get a MultiReader: that
			// wrapper would sit in front of every 1 MB read for the whole
			// transfer to replay 7 bytes it already handed back. It writes the
			// prefix once itself instead (step 7 below).
			resp.Body = readerCloser{io.MultiReader(bytes.NewReader(sniffed), resp.Body), resp.Body}
		}
	}
	if isManifest {
		serveManifest(w, r, resp, target, referer, server)
		return
	}

	// 5. Pass through the safe subset of headers, then layer our cache policy on
	//    top (long-immutable for media assets, no-store for error statuses).
	out := w.Header()
	for _, k := range headersToForward {
		// Direct map access on both sides: the keys are already canonical (see
		// headersToForward), so this skips the textproto canonicalization that
		// Get/Set would redo for every header on every request. The value slice
		// is shared with resp.Header, which is safe because every later writer
		// here uses Set — that replaces the slice rather than mutating it.
		if v := resp.Header[k]; len(v) > 0 && v[0] != "" {
			out[k] = v
		}
	}
	ct = responseContentType(target.Hostname(), target.Path, resp.Header)
	// We already have this segment's first bytes from the manifest sniff above, so
	// correcting a mislabeled type is free: wave serves MPEG-TS segments as
	// `image/jpeg`. hls.js goes by the manifest and doesn't care, but Safari's
	// native player does, and an "image" content-type also confuses caches.
	if tsCT := mpegTSContentType(sniffed, ct); tsCT != "" {
		ct = tsCT
	}
	// Cache policy is decided from the TRUE content-type, BEFORE any masquerade —
	// masking must never be able to change what the edge stores or for how long.
	setAssetCache(out, target.Path, resp.StatusCode, ct)
	// Last step: optionally advertise media as image/jpeg (MASK_SEGMENT_TYPE=1).
	if masked := maskedContentType(ct, resp.StatusCode); masked != "" {
		out.Set("Content-Type", masked)
	}

	// 6. Send the upstream status line (200 full body, 206 for a Range).
	w.WriteHeader(resp.StatusCode)

	// A HEAD request wants headers only — never a body.
	if r.Method == http.MethodHead {
		return
	}

	// 7. THE BACKPRESSURED COPY — the heart of the whole thing.
	//    io.CopyBuffer does: read up to 1 MB from upstream -> write it to the
	//    client -> repeat. The write BLOCKS until the client's TCP send buffer
	//    has room. A slow viewer therefore slows our reads from upstream, so at
	//    most ~one buffer is in flight per connection — RAM stays bounded (flat)
	//    no matter how slow the client, instead of buffering the whole file.
	// Replay the bytes the manifest sniff consumed. This body was NOT wrapped in
	// a MultiReader (see step 4b), so the prefix is written once, here, and the
	// copy below then runs against the bare upstream body.
	if len(sniffed) > 0 {
		if _, err := w.Write(sniffed); err != nil {
			return
		}
	}

	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	_, err = io.CopyBuffer(writerOnly{w}, resp.Body, *buf)
	if err != nil && !isClientGone(err) {
		// Only log genuine failures — client disconnects (seek/close) are normal
		// for video and would otherwise spam the log on every interaction.
		log.Printf("stream error: %v", err)
	}
}

// isClientGone reports whether a copy error is just the viewer disconnecting
// (closed tab, seek, quality switch) rather than a real problem. These dominate
// a video proxy's "errors" and are not worth logging.
func isClientGone(err error) bool {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "context canceled")
}
