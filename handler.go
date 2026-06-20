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
	"sync/atomic"
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
var headersToForward = []string{
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"Last-Modified",
	"ETag",
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
func serveManifest(w http.ResponseWriter, resp *http.Response, base *url.URL, referer string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
	if err != nil {
		http.Error(w, "manifest read failed", http.StatusBadGateway)
		return
	}

	// Guard: if it doesn't actually start with #EXTM3U, it was mislabeled — pass
	// it through untouched rather than corrupting it.
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U")) {
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		nw, _ := w.Write(body)
		atomic.AddInt64(&mBytesDown, int64(nw))
		return
	}

	// encode closes over `referer` so every child URL we emit carries the same
	// referer — segments need to forge it just like the manifest did.
	encode := func(u *url.URL) string {
		return "/stream/" + EncodePayload(u.String(), referer)
	}
	rewritten := rewritePlaylist(body, base, encode)

	h := w.Header()
	h.Set("Content-Type", "application/vnd.apple.mpegurl")
	// Manifests get a SHORT edge cache: long enough that a burst of viewers
	// starting the same episode share one fetch, short enough that live/sliding
	// playlists refresh. Only cache successful manifests.
	if resp.StatusCode == http.StatusOK {
		setCC(h, manifestPolicy)
	} else {
		setCC(h, noStore)
	}
	w.WriteHeader(resp.StatusCode)
	nw, _ := w.Write(rewritten)
	atomic.AddInt64(&mBytesDown, int64(nw))
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
			atomic.AddInt64(&mBusy, 1)
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
	}

	// Count this as an accepted request and track it as in-flight for the whole
	// proxied lifetime (released when the handler returns).
	atomic.AddInt64(&mTotalReq, 1)
	atomic.AddInt64(&mActive, 1)
	defer atomic.AddInt64(&mActive, -1)

	// 1. Decode the encrypted target from the path, e.g. /stream/<payload>.
	token := extractPayload(r.URL.Path)
	if token == "" {
		atomic.AddInt64(&mRejected, 1)
		http.Error(w, "missing payload", http.StatusBadRequest)
		return
	}
	rawURL, referer, ok := DecodePayload(token)
	if !ok {
		atomic.AddInt64(&mRejected, 1)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(rawURL)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		atomic.AddInt64(&mRejected, 1)
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	// SSRF guard: refuse targets that resolve to loopback/private/link-local
	// space, so a forged token can't make us fetch internal services or cloud
	// metadata (169.254.169.254). External video CDNs are always public IPs.
	if !isPublicHost(target.Hostname()) {
		atomic.AddInt64(&mRejected, 1)
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
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")

	// Many video hosts only serve bytes when the request looks like it came from
	// their own player page. The referer baked into the payload is how we forge
	// that origin. We derive Origin from it too (scheme://host, no path).
	if referer != "" {
		req.Header.Set("Referer", referer)
		if ref, err := url.Parse(referer); err == nil {
			req.Header.Set("Origin", ref.Scheme+"://"+ref.Host)
		}
	}

	// 4. Fire the upstream fetch. resp.Body is a STREAM, not the full payload —
	//    nothing has been downloaded into memory yet at this line.
	resp, err := httpClient.Do(req)
	if err != nil {
		atomic.AddInt64(&mUpstreamErr, 1)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	// Count every byte we pull from the origin (ingress). Wrapping here covers
	// both the streaming copy and the manifest read below.
	resp.Body = &countingReadCloser{rc: resp.Body, n: &mBytesUp}
	defer resp.Body.Close() // always release the upstream connection

	// 4a. Redirect? Upstream often 302s a segment to a signed CDN URL. We don't
	//     want the BROWSER to follow it (it'd hit the origin directly, leaking it
	//     and losing referer-forging). Instead we resolve the Location, re-wrap it
	//     through us (referer preserved), and redirect the player to OUR path.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			atomic.AddInt64(&mRedirects, 1)
			abs := resolve(loc, target)
			http.Redirect(w, r, "/stream/"+EncodePayload(abs.String(), referer), resp.StatusCode)
			return
		}
	}

	// 4b. Is this an HLS manifest? If so, we must read it, rewrite every child
	//     URL to point back through us, and serve the modified text — NOT stream
	//     it raw. Detect by the target's extension or the response content-type.
	if isM3U8Path(target.Path) || isM3U8ContentType(resp.Header.Get("Content-Type")) {
		atomic.AddInt64(&mManifests, 1)
		serveManifest(w, resp, target, referer)
		return
	}

	// 5. Pass through the safe subset of headers, then layer our cache policy on
	//    top (long-immutable for media assets, no-store for error statuses).
	out := w.Header()
	for _, k := range headersToForward {
		if v := resp.Header.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	setAssetCache(out, target.Path, resp.StatusCode, resp.Header.Get("Content-Type"))

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
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	n, err := io.CopyBuffer(w, resp.Body, *buf)
	atomic.AddInt64(&mBytesDown, n) // bytes delivered to client (egress)
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
