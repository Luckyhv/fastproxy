package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// isPublicIP reports whether an IP is a routable public address — i.e. NOT
// loopback, private (RFC1918 / ULA), link-local (incl. 169.254.169.254 cloud
// metadata), or unspecified. Used by the SSRF guard.
func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified())
}

// dialControl runs AFTER DNS resolution, with the concrete IP we're about to
// connect to. Rejecting non-public IPs here is the real SSRF backstop: it
// catches a hostname that resolves to a private/loopback address (DNS rebinding),
// which a handler-side string check on the hostname cannot.
func dialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("blocked non-public address: %s", host)
	}
	return nil
}

// insecureTLS, when INSECURE_TLS=1, makes us skip upstream certificate
// verification. Some sketchy video CDNs serve expired/self-signed/mismatched
// certs; Go (unlike Bun's old rejectUnauthorized:false) verifies by default, so
// we'd reject them. This is opt-in because it removes a real MITM protection —
// only enable it if a source actually needs it.
var insecureTLS = getenv("INSECURE_TLS", "") == "1"

// ─── Selective egress proxy ──────────────────────────────────────────────────
// Some CDNs sit behind Cloudflare or per-IP throttles. Routing JUST those hosts
// through a proxy pool gives every upstream a separate rate-limit budget, while
// everything else stays direct (proxies are slow + metered).
//
//	UPSTREAM_PROXIES        = comma-separated proxy URLs
//	UPSTREAM_PROXY_FILE     = file with one proxy per line
//	UPSTREAM_PROXY          = legacy single proxy URL
//	UPSTREAM_PROXY_SERVERS  = provider keys to route, or "*" for every token
//	UPSTREAM_PROXY_DOMAINS  = legacy host suffix fallback for old tokens
//
// Supported proxy forms:
//   - http://user:pass@host:port
//   - host:port
//   - host:port:user:pass
var (
	upstreamProxyPool    = newProxyPool(proxyEnv()).randomStart()
	upstreamProxyServers = parseNames(getenv("UPSTREAM_PROXY_SERVERS", defaultUpstreamProxyServers))
	upstreamProxyDomains = parseNames(getenv("UPSTREAM_PROXY_DOMAINS", ""))
)

const defaultUpstreamProxyServers = "uwu,kiwi,wave"

// How long a proxy sits out after a failure. Rate limits are per-IP and
// short-lived, so a throttled proxy is worth reusing soon; a proxy we could not
// even connect through is probably dead and gets a longer rest.
var (
	proxyRatePenalty  = getenvDuration("UPSTREAM_PROXY_RATE_COOLDOWN", 60*time.Second)
	proxyErrorPenalty = getenvDuration("UPSTREAM_PROXY_ERROR_COOLDOWN", 5*time.Minute)
	// Upstream 5xx is usually the origin's fault, not the exit IP's — brief
	// enough that one flaky origin can't drain the pool.
	proxyServerPenalty = getenvDuration("UPSTREAM_PROXY_5XX_COOLDOWN", 15*time.Second)
)

type forcedProxyKey struct{}
type proxyServerKey struct{}

// proxyEntry is one exit IP plus its cooldown deadline (unix nanos, 0 =
// healthy). Held by pointer so the atomic is never copied.
type proxyEntry struct {
	url         *url.URL
	bannedUntil atomic.Int64
}

type proxyPool struct {
	entries []*proxyEntry
	// byURL maps a handed-out *url.URL back to its entry so callers can
	// penalize the exact proxy a request went through. Pointer identity is
	// safe: next() only ever returns pointers out of entries.
	byURL        map[*url.URL]*proxyEntry
	cursor       uint64
	lastDrainLog atomic.Int64
}

func proxyEnv() []string {
	values := parseProxyList(getenv("UPSTREAM_PROXIES", ""))
	if single := strings.TrimSpace(getenv("UPSTREAM_PROXY", "")); single != "" {
		values = append(values, single)
	}
	if file := strings.TrimSpace(getenv("UPSTREAM_PROXY_FILE", "")); file != "" {
		if data, err := os.ReadFile(file); err == nil {
			values = append(values, parseProxyList(string(data))...)
		}
	}
	return values
}

func parseProxyList(s string) []string {
	var out []string
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func newProxyPool(values []string) *proxyPool {
	seen := map[string]bool{}
	pool := &proxyPool{byURL: map[*url.URL]*proxyEntry{}}
	for _, raw := range values {
		normalized := normalizeProxy(raw)
		if normalized == "" || seen[normalized] {
			continue
		}
		u, err := url.Parse(normalized)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		seen[normalized] = true
		entry := &proxyEntry{url: u}
		pool.entries = append(pool.entries, entry)
		pool.byURL[u] = entry
	}
	return pool
}

// randomStart offsets the cursor so that several fastproxy instances sharing
// one proxies.txt don't march through the pool in lockstep and hammer the same
// exit IP at the same moment. Only the production pool uses it — newProxyPool
// stays deterministic so tests can assert rotation order.
func (p *proxyPool) randomStart() *proxyPool {
	if n := p.len(); n > 1 {
		p.cursor = uint64(rand.Intn(n))
	}
	return p
}

func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	parts := strings.Split(raw, ":")
	if len(parts) == 4 {
		host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
		return (&url.URL{
			Scheme: "http",
			User:   url.UserPassword(user, pass),
			Host:   net.JoinHostPort(host, port),
		}).String()
	}
	return "http://" + raw
}

func (p *proxyPool) len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// healthy reports how many proxies are not currently cooling down. Diagnostics
// only — nothing routes on it.
func (p *proxyPool) healthy() int {
	now := time.Now().UnixNano()
	n := 0
	for _, e := range p.entries {
		if e.bannedUntil.Load() <= now {
			n++
		}
	}
	return n
}

// next hands out the next proxy in round-robin order, skipping any that are
// still cooling down after a recent failure. Round-robin (rather than a random
// pick) keeps the spread exactly even across the pool and guarantees that the
// retry attempts for one request walk distinct proxies.
func (p *proxyPool) next() *url.URL {
	n := p.len()
	if n == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	start := atomic.AddUint64(&p.cursor, 1) - 1

	var soonest *proxyEntry
	var soonestUntil int64
	for off := 0; off < n; off++ {
		e := p.entries[int((start+uint64(off))%uint64(n))]
		until := e.bannedUntil.Load()
		if until <= now {
			// Advance past the slots we skipped so the next caller resumes
			// AFTER the proxy we just used. Without this, the healthy entry
			// sitting behind a cooling one gets picked twice per lap.
			if off > 0 {
				atomic.AddUint64(&p.cursor, uint64(off))
			}
			return e.url
		}
		if soonest == nil || until < soonestUntil {
			soonest, soonestUntil = e, until
		}
	}
	// Every proxy is cooling down — the whole pool got throttled, or a dead
	// link 403'd its way through it. Failing the fetch outright would be worse
	// than one more try, so use whichever is closest to recovering.
	p.warnDrained(n)
	return soonest.url
}

// warnDrained logs an exhausted pool at most once every 30s. Without the
// throttle a drained pool would log on every single segment fetch.
func (p *proxyPool) warnDrained(size int) {
	now := time.Now().UnixNano()
	prev := p.lastDrainLog.Load()
	if now-prev < int64(30*time.Second) || !p.lastDrainLog.CompareAndSwap(prev, now) {
		return
	}
	log.Printf("upstream proxy pool drained: %d/%d healthy — serving from a cooling proxy",
		p.healthy(), size)
}

// penalize benches the proxy a failed request went through. Deadlines only ever
// move later, so a concurrent shorter penalty can't cut a longer one short.
func (p *proxyPool) penalize(u *url.URL, d time.Duration) {
	if p == nil || u == nil || d <= 0 {
		return
	}
	e := p.byURL[u]
	if e == nil {
		return
	}
	until := time.Now().Add(d).UnixNano()
	for {
		prev := e.bannedUntil.Load()
		if prev >= until || e.bannedUntil.CompareAndSwap(prev, until) {
			return
		}
	}
}

// proxyPenaltyFor maps an upstream status onto how long its exit IP sits out.
func proxyPenaltyFor(status int) time.Duration {
	switch status {
	case http.StatusTooManyRequests, http.StatusForbidden:
		return proxyRatePenalty
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return proxyServerPenalty
	}
	return 0
}

func parseNames(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(strings.ToLower(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func nameListed(value string, names []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, name := range names {
		if name == "*" || name == value {
			return true
		}
	}
	return false
}

func hostListed(host string, suffixes []string) bool {
	host = strings.ToLower(host)
	for _, suffix := range suffixes {
		if suffix == "*" || host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func shouldProxyRequest(req *http.Request) bool {
	if upstreamProxyPool.len() == 0 {
		return false
	}
	if server, ok := req.Context().Value(proxyServerKey{}).(string); ok &&
		nameListed(server, upstreamProxyServers) {
		return true
	}
	return len(upstreamProxyDomains) > 0 &&
		hostListed(req.URL.Hostname(), upstreamProxyDomains)
}

// proxyForRequest decides, per upstream request, whether to route via the egress
// proxy. Returning (nil, nil) means "connect directly".
func proxyForRequest(req *http.Request) (*url.URL, error) {
	if forced, ok := req.Context().Value(forcedProxyKey{}).(*url.URL); ok {
		return forced, nil
	}
	if !shouldProxyRequest(req) {
		return nil, nil
	}
	return upstreamProxyPool.next(), nil
}

func retryableUpstreamStatus(status int) bool {
	return status == http.StatusForbidden ||
		status == http.StatusTooManyRequests ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

func upstreamAttempts(req *http.Request) int {
	if req.Method == http.MethodPost || !shouldProxyRequest(req) {
		return 1
	}
	n := upstreamProxyPool.len()
	if n < 2 {
		return 1
	}
	if n > 4 {
		return 4
	}
	return n
}

func doUpstream(req *http.Request, useHTTP2 bool) (*http.Response, error) {
	client := httpClient
	if useHTTP2 {
		client = h2Client
	}
	attempts := upstreamAttempts(req)
	// Pick the exit IP here rather than letting the Transport call next() for
	// us: we need to know which proxy served the request so a failure can be
	// charged to it. Requests that shouldn't be proxied leave proxy nil and
	// fall through to proxyForRequest's direct path.
	proxied := shouldProxyRequest(req)
	var lastErr error
	for i := 0; i < attempts; i++ {
		nextReq := req
		var proxy *url.URL
		if proxied {
			proxy = upstreamProxyPool.next()
			nextReq = req.Clone(context.WithValue(req.Context(), forcedProxyKey{}, proxy))
		}
		resp, err := client.Do(nextReq)
		if err != nil {
			// Couldn't even complete the request through this proxy — most
			// likely a dead exit. Bench it for longer than a rate limit.
			upstreamProxyPool.penalize(proxy, proxyErrorPenalty)
			lastErr = err
			continue
		}
		if retryableUpstreamStatus(resp.StatusCode) {
			upstreamProxyPool.penalize(proxy, proxyPenaltyFor(resp.StatusCode))
			if i+1 < attempts {
				resp.Body.Close()
				continue
			}
		}
		return resp, nil
	}
	return nil, lastErr
}

// httpClient is the ONE client we reuse for every upstream fetch. Sharing a
// single client (and therefore a single Transport) is what lets Go pool and
// reuse TCP/TLS connections to upstreams across requests — re-dialing + a TLS
// handshake per segment would wreck throughput.
var httpClient = &http.Client{
	// No Client.Timeout: a hard timeout here would also kill long video streams.
	// We control lifetime with the request context + the per-phase timeouts on
	// the Transport below.
	Transport: &http.Transport{
		// Per-request egress decision: route blocked hosts through the clean
		// proxy, everything else direct.
		Proxy: proxyForRequest,

		// How we open the raw TCP connection. 10s to connect, then keep the
		// socket warm with TCP keepalives for reuse.
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   dialControl, // SSRF backstop on the resolved IP
		}).DialContext,

		// Force HTTP/1.1 to upstream. Go's HTTP/2 client uses small flow-control
		// windows (~64 KB/stream); on a high-latency VPS→CDN path that throttles
		// large media to ~window/RTT (e.g. 64KB/50ms ≈ 1.3 MB/s), making mp4
		// fetches crawl. HTTP/1.1 has no such app-level cap and streams at full TCP
		// speed — and for a proxy (one big sequential download per request) h2's
		// multiplexing buys nothing. A non-nil empty TLSNextProto disables h2.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},

		// 16x Go's 4 KB default: fewest syscalls per connection on large
		// transfers. These are charged per upstream connection, idle ones
		// included, so they trade memory for headroom on the fastest paths —
		// BenchmarkTransportBufferSweep measured 64 KiB and 32 KiB as equal
		// throughput under CDN-like load, with 64 KiB costing ~28% more
		// per-viewer heap. Sized for speed by choice; see perStreamBudget.
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 8 * 1024,

		// Connection-pool sizing. These caps are what let one box fan out to many
		// concurrent viewers without exhausting sockets.
		MaxIdleConns:        1000, // total idle keep-alive conns to keep around
		MaxIdleConnsPerHost: 100,  // idle conns per upstream host (default is 2!)
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// Opt-in: skip cert verification for upstreams with broken certs.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},

		// If an upstream accepts our connection but never sends response headers,
		// give up after 15s instead of hanging a goroutine forever.
		ResponseHeaderTimeout: 15 * time.Second,
	},

	// Do NOT auto-follow 3xx. We want to see the redirect so we can re-wrap the
	// Location through our own proxy (handled in a later part). Returning this
	// sentinel makes Do() hand us the 3xx response as-is.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// h2Client is the HTTP/2 escape hatch for upstreams that refuse HTTP/1.1.
// owocdn.top (uwu) is the one we hit today: Cloudflare answers a 403 block page
// to an h1 request and 200 to the byte-identical h2 one, whatever the headers.
//
// The reason h2 isn't the default is throughput — Go's h2 client defaults to a
// ~64 KB per-stream flow-control window, which on a high-latency VPS→CDN path
// caps a single big download at window/RTT (64KB/50ms ≈ 1.3 MB/s). HTTP2Config
// lifts that window to just under the 4 MiB ceiling the stdlib allows, which at
// the same RTT is ~80 MB/s — well past what any segment needs, so hosts routed
// here don't pay the penalty that made us disable h2 in the first place.
var h2Client = &http.Client{
	Transport: &http.Transport{
		Proxy: proxyForRequest,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   dialControl,
		}).DialContext,

		ForceAttemptHTTP2: true,
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream:     4<<20 - 1,
			MaxReceiveBufferPerConnection: 4<<20 - 1,
		},

		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 8 * 1024,

		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecureTLS},
		ResponseHeaderTimeout: 15 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
