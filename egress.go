package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Upstream egress proxies ─────────────────────────────────────────────────
//
// A proxy costs roughly 3x the direct per-segment latency (measured Aug 2026), so
// it is only worth paying where a host actually throttles us:
//
//   - UPSTREAM_PROXY_SERVERS / UPSTREAM_PROXY_DOMAINS always use the pool
//     (UPSTREAM_PROXY_SERVERS=* proxies everything).
//   - Every other host goes DIRECT first. A 429 on a direct fetch replays that
//     request through the pool and keeps the host there for hotHostTTL, so
//     unthrottled hosts keep direct speed and throttled ones recover by themselves.
//     With no filters at all, this is how every host is handled.
//
// Exits are STICKY per viewer+host. Go's transport pools idle connections per
// proxy URL, so a new exit per request pays a fresh CONNECT + TLS handshake on
// every segment. Pinning per viewer keeps one playback session on one warm tunnel
// (and one IP, for IP-bound signed URLs) while different viewers still spread
// across the pool. Pinning per provider would instead funnel every viewer of that
// provider through a single exit — the exact aggregation the pool exists to undo.

const (
	hotHostTTL    = 10 * time.Minute // how long a host that 429'd direct stays on the pool
	proxyCooldown = 30 * time.Second // how long a failing exit is skipped
)

type egressConfig struct {
	proxies          []*url.URL
	servers, domains []string
	down             []atomic.Int64 // per exit: unix nanos until which it is skipped
	hot              sync.Map       // host → unix nanos until which it is proxied
}

type egressKey struct{}

var egress, egressErr = loadEgress()

func newEgress(proxies []*url.URL, servers, domains []string) *egressConfig {
	return &egressConfig{proxies: proxies, servers: servers, domains: domains, down: make([]atomic.Int64, len(proxies))}
}

func parseDomains(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(strings.ToLower(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func loadEgress() (*egressConfig, error) {
	servers, domains := parseDomains(getenv("UPSTREAM_PROXY_SERVERS", "")), parseDomains(getenv("UPSTREAM_PROXY_DOMAINS", ""))
	entries := strings.Split(getenv("UPSTREAM_PROXIES", ""), ",")
	entries = append(entries, getenv("UPSTREAM_PROXY", ""))
	if file := getenv("UPSTREAM_PROXY_FILE", ""); file != "" {
		f, err := os.Open(file)
		if err != nil {
			return newEgress(nil, nil, nil), fmt.Errorf("cannot open UPSTREAM_PROXY_FILE")
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			entries = append(entries, scanner.Text())
		}
		if scanner.Err() != nil {
			return newEgress(nil, nil, nil), fmt.Errorf("cannot read UPSTREAM_PROXY_FILE")
		}
	}
	var proxies []*url.URL
	seen := make(map[string]bool)
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		// Never include raw configuration in errors: it can contain credentials.
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return newEgress(nil, nil, nil), fmt.Errorf("invalid upstream proxy URL (use scheme://user:pass@host:port)")
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return newEgress(nil, nil, nil), fmt.Errorf("invalid upstream proxy port")
			}
		}
		if !seen[u.String()] {
			proxies = append(proxies, u)
			seen[u.String()] = true
		}
	}
	if len(proxies) == 0 && (len(servers) > 0 || len(domains) > 0) {
		return newEgress(nil, nil, nil), fmt.Errorf("proxy filters configured without any upstream proxies")
	}
	return newEgress(proxies, servers, domains), nil
}

// forced reports whether this request must always use the pool. With no
// filters nothing is forced: every host goes direct and only a 429 moves it.
func (c *egressConfig) forced(host, server string) bool {
	for _, s := range c.servers {
		if s == "*" || s == server {
			return true
		}
	}
	for _, d := range c.domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func (c *egressConfig) isHot(host string) bool {
	v, ok := c.hot.Load(host)
	if !ok {
		return false
	}
	if time.Now().UnixNano() < v.(int64) {
		return true
	}
	c.hot.CompareAndDelete(host, v)
	return false
}

// markHot returns true when the host was not already on the pool (log once).
func (c *egressConfig) markHot(host string) bool {
	now := time.Now().UnixNano()
	prev, loaded := c.hot.Swap(host, now+int64(hotHostTTL))
	return !loaded || prev.(int64) <= now
}

// pick returns the index of the exit for key. attempt 0 is the sticky choice;
// attempt n walks to the nth healthy exit after it. Exits cooling down are
// skipped, so a dead proxy's viewers move to a neighbour and come back later.
func (c *egressConfig) pick(key string, attempt int) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	n := len(c.proxies)
	start := int(h.Sum64() % uint64(n))
	now := time.Now().UnixNano()
	skip := attempt
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if c.down[idx].Load() > now {
			continue
		}
		if skip == 0 {
			return idx
		}
		skip--
	}
	return (start + attempt) % n // every exit is cooling down: try them anyway
}

// markDown returns true when the exit was healthy until now (log once).
func (c *egressConfig) markDown(idx int) bool {
	now := time.Now().UnixNano()
	return c.down[idx].Swap(now+int64(proxyCooldown)) <= now
}

// proxyForRequest is the Transport's Proxy hook; fetchUpstream already chose.
func proxyForRequest(req *http.Request) (*url.URL, error) {
	p, _ := req.Context().Value(egressKey{}).(*url.URL)
	return p, nil
}

// clientIP identifies the viewer for sticky egress. Behind Cloudflare → Caddy
// the socket peer is always Caddy, so the forwarded headers are the real signal;
// without them every viewer would share one exit. A spoofed header only picks a
// different exit — it bypasses nothing.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// fetchUpstream runs one upstream request with the egress policy above. Only
// bodyless requests (GET/HEAD) are replayed; nothing has reached the viewer yet
// at this point, so a replay can never duplicate bytes.
func fetchUpstream(client *http.Client, req *http.Request, server, viewer string) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	// chromeClient cannot tunnel (see chrome.go), so it never enters the pool —
	// handing it an exit would only be ignored and misreport the request.
	if len(egress.proxies) == 0 || client == chromeClient {
		resp, err := client.Do(req)
		recordUpstream(host, resp, err, false)
		return resp, err
	}
	replayable := req.Body == nil || req.Body == http.NoBody
	server = strings.ToLower(strings.TrimSpace(server))

	if !egress.forced(host, server) && !egress.isHot(host) {
		resp, err := client.Do(req)
		recordUpstream(host, resp, err, false)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || !replayable {
			return resp, err
		}
		resp.Body.Close()
		if egress.markHot(host) {
			log.Printf("egress: %s answered 429 direct; proxying it for %s", host, hotHostTTL)
		}
	}

	attempts := 1
	if replayable && len(egress.proxies) > 1 {
		attempts = 2
	}
	key := viewer + "|" + host
	var resp *http.Response
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		idx := egress.pick(key, attempt)
		exit := egress.proxies[idx]
		resp, err = client.Do(req.WithContext(context.WithValue(req.Context(), egressKey{}, exit)))
		recordUpstream(host, resp, err, true)
		if req.Context().Err() != nil {
			return resp, err // the viewer left; not the exit's fault
		}
		switch {
		case err != nil:
			if egress.markDown(idx) {
				log.Printf("egress: proxy %s failed for %s (%v); skipping it for %s", exit.Host, host, redactURLError(err), proxyCooldown)
			}
		case resp.StatusCode == http.StatusProxyAuthRequired:
			if egress.markDown(idx) {
				log.Printf("egress: proxy %s rejected credentials (407); skipping it for %s", exit.Host, proxyCooldown)
			}
		case resp.StatusCode == http.StatusTooManyRequests:
			// This exit is throttled by this host; the next one may not be.
		default:
			return resp, nil
		}
		if attempt+1 < attempts && resp != nil {
			resp.Body.Close()
		}
	}
	return resp, err
}

// redactURLError drops the *url.Error wrapper, whose message carries the full
// target URL — signed query tokens included.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
