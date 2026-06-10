package main

import (
	"crypto/tls"
	"fmt"
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

		// Bigger socket buffers = fewer syscalls on large transfers (default 4 KB).
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,

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
