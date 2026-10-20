package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
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
//   - It cannot use net/http's Proxy hook, which tunnels and then runs
//     crypto/tls on top — replacing the very handshake this client controls. So
//     the egress pool gets its own clients (chromeClientVia): CONNECT through the
//     exit by hand, then the uTLS handshake inside the tunnel.
var chromeClient = newChromeClient(nil)

// Oct 2026: settlar's Cloudflare firewall started answering "Access denied" to
// every fastproxy box IP (direct), while pool exits pass. fetchUpstream replays a
// Chrome-TLS 403 through the pool; one client per exit keeps h2 pooling per exit.
var chromeExits sync.Map // *url.URL -> *http.Client

func chromeClientVia(exit *url.URL) *http.Client {
	if c, ok := chromeExits.Load(exit); ok {
		return c.(*http.Client)
	}
	c, _ := chromeExits.LoadOrStore(exit, newChromeClient(exit))
	return c.(*http.Client)
}

func newChromeClient(exit *url.URL) *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialChromeTLS(ctx, network, addr, exit)
			},
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
}

func dialChromeTLS(ctx context.Context, network, addr string, exit *url.URL) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var raw net.Conn
	if exit == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: dialControl}
		raw, err = dialer.DialContext(ctx, network, addr)
	} else {
		raw, err = dialTunnel(ctx, exit, addr)
	}
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

// dialTunnel opens a raw TCP tunnel to addr through exit: SOCKS5, or HTTP
// CONNECT. Like the pool path in net/http, the exit resolves addr itself.
// shortcut: an https:// exit is spoken to in plain TCP; add a TLS hop if one is ever configured.
func dialTunnel(ctx context.Context, exit *url.URL, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if strings.HasPrefix(exit.Scheme, "socks5") {
		d, err := proxy.FromURL(exit, dialer)
		if err != nil {
			return nil, err
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, "tcp", addr)
		}
		return d.Dial("tcp", addr)
	}
	raw, err := dialer.DialContext(ctx, "tcp", exit.Host)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		raw.SetDeadline(deadline)
	} else {
		raw.SetDeadline(time.Now().Add(10 * time.Second))
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if u := exit.User; u != nil {
		pass, _ := u.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pass)))
	}
	if err := req.Write(raw); err != nil {
		raw.Close()
		return nil, err
	}
	// The tunnel's first bytes are the server's TLS, which only arrives after our
	// ClientHello, so this reader never buffers past the CONNECT response.
	resp, err := http.ReadResponse(bufio.NewReader(raw), req)
	if err != nil {
		raw.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw.Close()
		return nil, fmt.Errorf("proxy CONNECT: %s", resp.Status)
	}
	raw.SetDeadline(time.Time{})
	return raw, nil
}
