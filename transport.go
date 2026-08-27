package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
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
// Some CDNs (e.g. vid-cdn.xyz) sit behind Cloudflare and 403 our datacenter IP.
// Routing JUST those hosts through a clean/residential proxy bypasses the block,
// while everything else goes direct (proxies are slow + metered, so we don't
// want all traffic on them).
//
//	UPSTREAM_PROXY          = http://user:pass@host:port   (the egress proxy)
//	UPSTREAM_PROXY_DOMAINS  = vid-cdn.xyz,foo.com          (suffixes to route)
//
// If UPSTREAM_PROXY is set but DOMAINS is empty, ALL upstream traffic is proxied.
var (
	upstreamProxyURL, _  = url.Parse(getenv("UPSTREAM_PROXY", ""))
	upstreamProxyDomains = parseDomains(getenv("UPSTREAM_PROXY_DOMAINS", ""))
)

func parseDomains(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(strings.ToLower(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// proxyForRequest decides, per upstream request, whether to route via the egress
// proxy. Returning (nil, nil) means "connect directly".
func proxyForRequest(req *http.Request) (*url.URL, error) {
	if upstreamProxyURL == nil || upstreamProxyURL.Host == "" {
		return nil, nil // no egress proxy configured → always direct
	}
	if len(upstreamProxyDomains) == 0 {
		return upstreamProxyURL, nil // configured but no filter → proxy everything
	}
	host := strings.ToLower(req.URL.Hostname())
	for _, d := range upstreamProxyDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return upstreamProxyURL, nil
		}
	}
	return nil, nil
}

// ─── Upstream clients ────────────────────────────────────────────────────────
//
// We reuse a SHARED client (and therefore a shared Transport) for every upstream
// fetch. That is what lets Go pool and reuse TCP/TLS connections across
// requests — re-dialing plus a TLS handshake per segment would wreck throughput.
//
// There are exactly two, and they differ only in how they speak HTTP, so the
// shape lives here once and the flag picks the protocol.
//
// HTTP/1.1 is the default. Go's HTTP/2 client uses small flow-control windows
// (~64 KB/stream); on a high-latency VPS→CDN path that throttles large media to
// ~window/RTT (e.g. 64KB/50ms ≈ 1.3 MB/s), making mp4 fetches crawl. HTTP/1.1
// has no such app-level cap and streams at full TCP speed — and for a proxy (one
// big sequential download per request) h2's multiplexing buys nothing.
//
// h2 is the escape hatch for upstreams that refuse HTTP/1.1. owocdn.top (uwu) is
// the one we hit today: Cloudflare answers a 403 block page to an h1 request and
// 200 to the byte-identical h2 one, whatever the headers. Only providers with
// http2:true in domains.go land there. HTTP2Config lifts the receive window to
// just under the 4 MiB ceiling the stdlib allows — ~80 MB/s at the same RTT — so
// those hosts don't pay the penalty that made us disable h2 in the first place.
func newClient(http2 bool) *http.Client {
	t := &http.Transport{
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

		// Bigger socket buffers = fewer syscalls on large transfers (default 4 KB).
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,

		// Connection-pool sizing. These caps are what let one box fan out to many
		// concurrent viewers without exhausting sockets. autoTune() raises them at
		// startup on boxes with the RAM to back it — see tuneTransports.
		MaxIdleConns:        defaultMaxIdleConns,        // total idle keep-alive conns to keep around
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost, // idle conns per upstream host (default is 2!)
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// Opt-in: skip cert verification for upstreams with broken certs.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},

		// If an upstream accepts our connection but never sends response headers,
		// give up after 15s instead of hanging a goroutine forever.
		ResponseHeaderTimeout: 15 * time.Second,
	}

	if http2 {
		t.ForceAttemptHTTP2 = true
		t.HTTP2 = &http.HTTP2Config{
			MaxReceiveBufferPerStream:     4<<20 - 1,
			MaxReceiveBufferPerConnection: 4<<20 - 1,
		}
	} else {
		// A non-nil empty TLSNextProto is what actually disables h2 negotiation.
		t.ForceAttemptHTTP2 = false
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}

	return &http.Client{
		// No Client.Timeout: a hard timeout here would also kill long video
		// streams. We control lifetime with the request context + the per-phase
		// timeouts on the Transport above.
		Transport: t,

		// Do NOT auto-follow 3xx. We want to see the redirect so we can re-wrap
		// the Location through our own proxy. Returning this sentinel makes Do()
		// hand us the 3xx response as-is.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var (
	httpClient = newClient(false)
	h2Client   = newClient(true)
)

// ─── Connection-pool sizing ──────────────────────────────────────────────────
//
// The per-host cap is the one that bites. Every viewer pulls segment after
// segment from the SAME handful of CDN hostnames, so a whole box's traffic
// funnels through one entry in the pool. Once concurrent fetches to that host
// exceed the cap, Go closes the surplus connections the moment their response is
// read instead of parking them — and the next segment pays a fresh TCP handshake
// plus a full TLS handshake, which on a VPS→CDN path is one to two extra round
// trips of dead time before a single byte of video moves.
//
// The reason it can't just be enormous is RAM: a pooled connection holds the
// transport's read and write buffers (64 KiB each) plus TLS state, so we budget
// idleConnCost per parked connection and spend at most idleConnRAMShare of
// detected memory on them.
const (
	defaultMaxIdleConns        = 1000
	defaultMaxIdleConnsPerHost = 100

	idleConnCost     = 160 * 1024 // 64K read + 64K write buffer + TLS state
	idleConnRAMShare = 20         // spend at most 1/20th (5%) of RAM on parked conns
	maxIdleConnsCap  = 20000
)

// tuneTransports sizes the connection pools from detected RAM, honouring
// explicit env overrides. Called from autoTune() before the listener starts, so
// no request is in flight and mutating the live transports is safe.
func tuneTransports(mem uint64) {
	total := defaultMaxIdleConns
	if mem > 0 {
		if n := int(mem / idleConnRAMShare / idleConnCost); n > total {
			total = min(n, maxIdleConnsCap)
		}
	}
	if n := atoiDefault(getenv("MAX_IDLE_CONNS", ""), 0); n > 0 {
		total = n
	}

	// A quarter of the pool per host: enough that one busy CDN keeps its
	// connections warm, while a second and third provider still have room. This
	// raises the per-host cap even when RAM is undetected, and costs nothing —
	// MaxIdleConns is the real memory ceiling, and it hasn't moved.
	perHost := max(total/4, defaultMaxIdleConnsPerHost)
	if n := atoiDefault(getenv("MAX_IDLE_CONNS_PER_HOST", ""), 0); n > 0 {
		perHost = n
	}

	for _, c := range []*http.Client{httpClient, h2Client} {
		t := c.Transport.(*http.Transport)
		t.MaxIdleConns = total
		t.MaxIdleConnsPerHost = perHost
	}
	log.Printf("autotune: idle conns=%d (per host %d)", total, perHost)
}
