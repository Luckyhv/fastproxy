package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeProxy(t *testing.T) {
	tests := map[string]string{
		"1.2.3.4:8080":           "http://1.2.3.4:8080",
		"1.2.3.4:8080:user:pass": "http://user:pass@1.2.3.4:8080",
		"http://u:p@host:9000":   "http://u:p@host:9000",
	}

	for input, want := range tests {
		if got := normalizeProxy(input); got != want {
			t.Fatalf("normalizeProxy(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProxyPoolRotates(t *testing.T) {
	pool := newProxyPool([]string{
		"http://a.example:1",
		"http://b.example:2",
		"http://a.example:1",
	})

	if pool.len() != 2 {
		t.Fatalf("pool length = %d, want 2", pool.len())
	}
	got := []*url.URL{pool.next(), pool.next(), pool.next()}
	if got[0].Host != "a.example:1" || got[1].Host != "b.example:2" || got[2].Host != "a.example:1" {
		t.Fatalf("pool did not rotate as expected: %#v", got)
	}
}

func TestProxyPoolSkipsCoolingProxies(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1", "http://b:2", "http://c:3"})

	// Bench b; rotation should step straight over it.
	pool.penalize(pool.entries[1].url, time.Minute)
	if got := pool.healthy(); got != 2 {
		t.Fatalf("healthy = %d, want 2", got)
	}

	var hosts []string
	for i := 0; i < 4; i++ {
		hosts = append(hosts, pool.next().Host)
	}
	want := []string{"a:1", "c:3", "a:1", "c:3"}
	for i := range want {
		if hosts[i] != want[i] {
			t.Fatalf("rotation = %v, want %v", hosts, want)
		}
	}
}

func TestProxyPoolCooldownExpires(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1", "http://b:2"})
	pool.penalize(pool.entries[1].url, 20*time.Millisecond)

	if got := pool.next().Host; got != "a:1" {
		t.Fatalf("next = %q, want a:1", got)
	}
	if got := pool.next().Host; got != "a:1" {
		t.Fatalf("next while b is cooling = %q, want a:1", got)
	}

	time.Sleep(30 * time.Millisecond)
	if got := pool.healthy(); got != 2 {
		t.Fatalf("healthy after cooldown = %d, want 2", got)
	}
}

// A longer penalty must not be shortened by a later, smaller one.
func TestProxyPenaltyOnlyExtends(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1"})
	entry := pool.entries[0]

	pool.penalize(entry.url, time.Hour)
	long := entry.bannedUntil.Load()
	pool.penalize(entry.url, time.Second)

	if entry.bannedUntil.Load() != long {
		t.Fatalf("short penalty shortened the cooldown")
	}
}

// With every proxy cooling down we must still hand one back — the one closest
// to recovering — rather than failing the fetch outright.
func TestProxyPoolFullyDrainedFallsBack(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1", "http://b:2", "http://c:3"})
	pool.penalize(pool.entries[0].url, time.Hour)
	pool.penalize(pool.entries[1].url, 5*time.Minute) // soonest to recover
	pool.penalize(pool.entries[2].url, time.Hour)

	if got := pool.healthy(); got != 0 {
		t.Fatalf("healthy = %d, want 0", got)
	}
	if got := pool.next(); got == nil || got.Host != "b:2" {
		t.Fatalf("drained fallback = %v, want b:2", got)
	}
}

func TestProxyPenaltyForStatus(t *testing.T) {
	cases := map[int]time.Duration{
		http.StatusTooManyRequests:     proxyRatePenalty,
		http.StatusForbidden:           proxyRatePenalty,
		http.StatusBadGateway:          proxyServerPenalty,
		http.StatusGatewayTimeout:      proxyServerPenalty,
		http.StatusNotFound:            0,
		http.StatusOK:                  0,
		http.StatusInternalServerError: 0,
	}
	for status, want := range cases {
		if got := proxyPenaltyFor(status); got != want {
			t.Fatalf("proxyPenaltyFor(%d) = %s, want %s", status, got, want)
		}
	}
}

// penalize is a no-op for a nil pool/URL and for a URL we never handed out.
func TestProxyPenalizeIgnoresUnknown(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1"})
	other, _ := url.Parse("http://elsewhere:9")

	pool.penalize(nil, time.Minute)
	pool.penalize(other, time.Minute)
	(*proxyPool)(nil).penalize(other, time.Minute)

	if got := pool.healthy(); got != 1 {
		t.Fatalf("healthy = %d, want 1", got)
	}
}

func TestParseProxyList(t *testing.T) {
	got := parseProxyList("http://a:1, http://b:2\n#comment\nhttp://c:3")
	if len(got) != 3 {
		t.Fatalf("proxy count = %d, want 3: %#v", len(got), got)
	}
}

func TestProxyRoutingUsesServerName(t *testing.T) {
	oldPool, oldServers, oldDomains := upstreamProxyPool, upstreamProxyServers, upstreamProxyDomains
	defer func() {
		upstreamProxyPool = oldPool
		upstreamProxyServers = oldServers
		upstreamProxyDomains = oldDomains
	}()

	upstreamProxyPool = newProxyPool([]string{"http://proxy.example:8080"})
	upstreamProxyServers = []string{"uwu", "kiwi", "wave"}
	upstreamProxyDomains = nil

	req, err := http.NewRequestWithContext(
		context.WithValue(context.Background(), proxyServerKey{}, "uwu"),
		http.MethodGet,
		"https://brand-new-cdn.example/video.m3u8",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !shouldProxyRequest(req) {
		t.Fatal("uwu should route through proxy by server key even on unknown CDN host")
	}
}

func TestProxyRoutingIgnoresHostWhenOnlyServersConfigured(t *testing.T) {
	oldPool, oldServers, oldDomains := upstreamProxyPool, upstreamProxyServers, upstreamProxyDomains
	defer func() {
		upstreamProxyPool = oldPool
		upstreamProxyServers = oldServers
		upstreamProxyDomains = oldDomains
	}()

	upstreamProxyPool = newProxyPool([]string{"http://proxy.example:8080"})
	upstreamProxyServers = []string{"uwu"}
	upstreamProxyDomains = nil

	req, err := http.NewRequest(http.MethodGet, "https://hls.anidb.app/video.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if shouldProxyRequest(req) {
		t.Fatal("host should not trigger proxy routing when only server routing is configured")
	}
}

// ─── Egress proxy connection reuse ───────────────────────────────────────────

// countingProxy is a minimal forward proxy that counts how many TCP connections
// were opened to it. Every proxy URL in the test points at this ONE listener, so
// a rise in dials cannot be blamed on a different network path — it can only
// come from the client refusing to reuse an idle connection.
type countingProxy struct {
	srv   *http.Server
	ln    net.Listener
	dials atomic.Int64
}

type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

func newCountingProxy(t *testing.T) *countingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{ln: ln}
	// Plain (absolute-URI) forwarding — enough to exercise Transport pooling.
	p.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, err := http.NewRequest(r.Method, r.URL.String(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultClient.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})}
	go p.srv.Serve(countingListener{ln, &p.dials})
	t.Cleanup(func() { _ = p.srv.Close() })
	return p
}

// urls builds n DISTINCT proxy URL strings that all resolve to the same
// listener, mirroring production where 1000 entries share one credential.
func (p *countingProxy) urls(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("http://user%d:pass@%s", i, p.ln.Addr().String())
	}
	return out
}

// runProxiedFetches drives n sequential upstream fetches for a proxied server
// name through a transport wired exactly like production's, and reports how many
// TCP connections the transport opened.
func runProxiedFetches(t *testing.T, proxies []string, n int) int64 {
	return runFetches(t, proxies, n, nil)
}

// runFetches drives n sequential upstream fetches through a transport wired like
// production's. decorate optionally adds the sticky key handleProxy attaches.
func runFetches(t *testing.T, proxies []string, n int, decorate func(context.Context) context.Context) int64 {
	t.Helper()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	t.Cleanup(origin.Close)

	oldPool, oldServers := upstreamProxyPool, upstreamProxyServers
	t.Cleanup(func() { upstreamProxyPool, upstreamProxyServers = oldPool, oldServers })
	upstreamProxyPool = newProxyPool(proxies)
	upstreamProxyServers = []string{"uwu"}

	var dials atomic.Int64
	client := &http.Client{Transport: &http.Transport{
		Proxy: proxyForRequest, // the real production hook
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
	}}

	for i := 0; i < n; i++ {
		ctx := context.WithValue(context.Background(), proxyServerKey{}, "uwu")
		if decorate != nil {
			ctx = decorate(ctx)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/seg.ts", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !shouldProxyRequest(req) {
			t.Fatal("request should be routed through the pool")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return dials.Load()
}

// TestStickyProxyReusesOneConnection is the fix, measured on the same harness:
// a viewer's segments must all ride ONE tunnel even against a 1000-entry pool.
func TestStickyProxyReusesOneConnection(t *testing.T) {
	const requests = 20
	proxy := newCountingProxy(t)

	viewer := func(ip string) func(context.Context) context.Context {
		return func(ctx context.Context) context.Context {
			return context.WithValue(ctx, stickyKeyKey{}, stickyKey(ip, "cdn.example"))
		}
	}

	got := runFetches(t, proxy.urls(1000), requests, viewer("203.0.113.7"))
	if got != 1 {
		t.Fatalf("sticky viewer opened %d connections over %d requests, want 1", got, requests)
	}
}

// Stickiness must not collapse every viewer onto one exit — that would hand the
// CDN a single IP to throttle, which is the whole reason the pool exists.
func TestStickyProxySpreadsViewersAcrossPool(t *testing.T) {
	pool := newProxyPool([]string{
		"http://a:1", "http://b:2", "http://c:3", "http://d:4",
		"http://e:5", "http://f:6", "http://g:7", "http://h:8",
	})

	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i%256)
		seen[pool.sticky(stickyKey(ip, "cdn.example"), 0).Host]++
	}
	if len(seen) < 5 {
		t.Fatalf("200 viewers landed on only %d of 8 exits: %v", len(seen), seen)
	}

	// The same viewer must be stable across calls, or reuse never happens.
	key := stickyKey("198.51.100.42", "cdn.example")
	first := pool.sticky(key, 0).Host
	for i := 0; i < 50; i++ {
		if got := pool.sticky(key, 0).Host; got != first {
			t.Fatalf("sticky pick drifted from %s to %s", first, got)
		}
	}
}

// A retry must step OFF the failed proxy, and a benched proxy must never be the
// sticky pick — otherwise one dead exit pins a viewer to a broken stream.
func TestStickyProxyRetriesAndSkipsBenched(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1", "http://b:2", "http://c:3"})
	key := stickyKey("203.0.113.9", "cdn.example")

	pinned := pool.sticky(key, 0)
	if next := pool.sticky(key, 1); next.Host == pinned.Host {
		t.Fatalf("retry reused the failed proxy %s", pinned.Host)
	}

	pool.penalize(pinned, proxyErrorPenalty)
	if got := pool.sticky(key, 0); got.Host == pinned.Host {
		t.Fatalf("benched proxy %s was still handed out", pinned.Host)
	}
}

// TestRotatingProxyColdConnectionsScaleWithPool pins down WHY the production
// pool is the worst case: the number of cold connections is min(requests, pool
// size). With 1000 entries and a few hundred segments per episode, a lap never
// completes, so no segment ever reuses a tunnel.
func TestRotatingProxyColdConnectionsScaleWithPool(t *testing.T) {
	const requests = 12
	proxy := newCountingProxy(t)

	for _, size := range []int{1, 2, 12} {
		got := runProxiedFetches(t, proxy.urls(size), requests)
		want := int64(size)
		if got != want {
			t.Fatalf("pool of %d over %d requests opened %d connections, want %d",
				size, requests, got, want)
		}
	}
}
