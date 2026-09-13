package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
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

// Shared clients preserve TCP/TLS connections across segments. Protocol choice
// stays provider-specific; benchmark changes against the actual upstream.
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
		// concurrent viewers without exhausting sockets. autoTune sizes them at
		// startup from available RAM — see tuneTransports.
		MaxIdleConns:        defaultMaxIdleConns,        // total idle keep-alive conns to keep around
		MaxIdleConnsPerHost: defaultMaxIdleConnsPerHost, // idle conns per upstream host (default is 2!)
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// Opt-in: skip cert verification for upstreams with broken certs.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS, ClientSessionCache: tls.NewLRUClientSessionCache(256)},

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

// Budget idle HTTP buffers and TLS state across the two shared transports.
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
		// Split the RAM budget between the HTTP/1 and HTTP/2 transports.
		total = max(1, min(int(mem/idleConnRAMShare/idleConnCost/2), maxIdleConnsCap))
	}
	if n := atoiDefault(getenv("MAX_IDLE_CONNS", ""), 0); n > 0 {
		total = n
	}

	// Let a busy CDN use half of the memory-budgeted idle pool. No active
	// per-host cap by default: inFlight already bounds total work, and a lower
	// host cap would make requests queue for a connection while holding a slot.
	active := 0
	if raw := getenv("MAX_CONNS_PER_HOST", ""); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			active = n
		}
	}
	perHost := max(total/2, 1)
	if active > 0 {
		perHost = min(perHost, active)
	}
	if n := atoiDefault(getenv("MAX_IDLE_CONNS_PER_HOST", ""), 0); n > 0 {
		perHost = n
	}

	for _, c := range []*http.Client{httpClient, h2Client} {
		t := c.Transport.(*http.Transport)
		t.MaxIdleConns = total
		t.MaxIdleConnsPerHost = perHost
		t.MaxConnsPerHost = active
	}
	log.Printf("autotune: per transport idle=%d, idle/host=%d, active+idle/host=%d", total, perHost, active)
}
