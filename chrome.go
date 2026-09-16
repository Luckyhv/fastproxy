package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// chromeClient speaks to upstreams that fingerprint the TLS handshake itself.
//
// settlar.io (and ani.pm in front of it) sit behind Cloudflare bot management
// that 403s any ClientHello which is not a real browser's: Go's crypto/tls, Bun
// and plain curl are all refused no matter what headers they send, while the
// byte-identical request from a Chrome-shaped handshake is served. Headers are
// not the lever here — the JA3/JA4 fingerprint is — so this client does the
// handshake with uTLS's Chrome ClientHello.
//
// Two consequences of that handshake, both deliberate:
//
//   - It always speaks HTTP/2. Chrome's hello offers h2 first, so a Cloudflare
//     origin always picks it; net/http's Transport cannot run HTTP/2 over a
//     non-crypto/tls conn, so the x/net/http2 transport does it directly. A host
//     that would answer HTTP/1.1 to a Chrome hello cannot use this client.
//   - It is DIRECT only. The egress pool works by having net/http tunnel through
//     the proxy and then run crypto/tls on top, which would replace the very
//     handshake this client exists to control. fetchUpstream skips the pool for
//     it; see chromeServer.
var chromeClient = &http.Client{
	Transport: &http2.Transport{
		DialTLSContext:             dialChromeTLS,
		ReadIdleTimeout:            30 * time.Second, // ping a quiet conn before reuse
		PingTimeout:                10 * time.Second,
		MaxReadFrameSize:           1 << 20,
		StrictMaxConcurrentStreams: false,
	},
	// Same redirect contract as the other clients: the handler follows 3xx
	// itself so every hop re-passes the SSRF guard.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func dialChromeTLS(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: dialControl}
	raw, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn := utls.UClient(raw, &utls.Config{ServerName: host, InsecureSkipVerify: insecureTLS}, utls.HelloChrome_Auto)
	hsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hsCtx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}
