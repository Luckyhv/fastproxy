package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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

// TestRotatingProxyDefeatsConnectionReuse is the reproduction for the slowdown.
//
// Go keys its idle-connection pool on the PROXY URL as well as the target, so a
// pool that hands out a different proxy per request never gets a keep-alive hit:
// every HLS segment pays a fresh TCP connect (plus, in production, a CONNECT
// tunnel and a full TLS handshake to the CDN) instead of reusing a warm socket.
//
// Both halves talk to the same listener, so the only variable is rotation.
func TestRotatingProxyDefeatsConnectionReuse(t *testing.T) {
	const requests = 20

	proxy := newCountingProxy(t)

	rotating := runProxiedFetches(t, proxy.urls(8), requests)
	pinned := runProxiedFetches(t, proxy.urls(1), requests)

	t.Logf("dials over %d requests: rotating pool = %d, pinned proxy = %d", requests, rotating, pinned)

	if pinned != 1 {
		t.Fatalf("pinned proxy opened %d connections, want 1 (keep-alive reuse)", pinned)
	}
	if rotating <= pinned {
		t.Fatalf("rotating pool opened %d connections, expected far more than pinned %d", rotating, pinned)
	}
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

// Sticky selection must steer viewers onto the fast half of the pool. A 60-exit
// sample of the real proxies.txt spanned 0.37s-0.99s TTFB, so which exit a
// viewer is assigned to is worth roughly 2x.
func TestStickyPrefersFastExits(t *testing.T) {
	var urls []string
	for i := 0; i < 20; i++ {
		urls = append(urls, fmt.Sprintf("http://p%d:1", i))
	}
	pool := newProxyPool(urls)

	// Spread latencies 100ms…2000ms so the median cleanly splits the pool.
	for i, e := range pool.entries {
		pool.observe(e.url, time.Duration(i+1)*100*time.Millisecond)
	}
	pool.lastCutoffCalc.Store(0)
	pool.refreshCutoff()

	cutoff := pool.fastCutoff.Load()
	if cutoff == 0 {
		t.Fatal("cutoff never computed")
	}

	seen := map[string]bool{}
	steered, explorers := 0, 0
	for i := 0; i < 500; i++ {
		key := stickyKey(fmt.Sprintf("198.51.100.%d", i%256), "cdn.example")
		u := pool.sticky(key, 0)
		if key%explorerShare == 0 {
			explorers++
			continue
		}
		steered++
		if got := pool.byURL[u].latency.Load(); got > cutoff {
			t.Fatalf("steered viewer picked slow exit %s (%v > cutoff %v)", u.Host,
				time.Duration(got), time.Duration(cutoff))
		}
		seen[u.Host] = true
	}
	if steered == 0 {
		t.Fatal("no steered viewers to assert on")
	}
	// Explorers must stay a small minority, or steering does nothing.
	if explorers > steered/4 {
		t.Fatalf("explorers %d vs steered %d — exploration is too aggressive", explorers, steered)
	}
	// Steering must not collapse everyone onto the single fastest exit — that
	// would rebuild the per-IP throttling the pool exists to spread.
	if len(seen) < 5 {
		t.Fatalf("viewers concentrated on %d exits: %v", len(seen), seen)
	}
}

// Unmeasured exits must still get sampled, or the ranked set could never grow
// past whatever happened to be measured first. Shaped like production: a large
// pool where only a handful of exits have been measured so far.
func TestStickyExplorersSampleUnmeasuredExits(t *testing.T) {
	var urls []string
	for i := 0; i < 200; i++ {
		urls = append(urls, fmt.Sprintf("http://p%d:1", i))
	}
	pool := newProxyPool(urls)
	for i := 0; i < 10; i++ {
		pool.observe(pool.entries[i].url, time.Duration(i+1)*100*time.Millisecond)
	}
	pool.lastCutoffCalc.Store(0)
	pool.refreshCutoff()
	if pool.fastCutoff.Load() == 0 {
		t.Fatal("cutoff never computed")
	}

	unmeasured := 0
	for i := 0; i < 4000; i++ {
		u := pool.sticky(stickyKey(fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256), "cdn"), 0)
		if pool.byURL[u].latency.Load() == 0 {
			unmeasured++
		}
	}
	if unmeasured == 0 {
		t.Fatal("unmeasured exits were never selected, so they can never earn a sample")
	}
}

// A pool with too few samples must stay unsteered rather than ranking on noise —
// and must not be locked out of recomputing for 30s by that early bail.
func TestCutoffWaitsForEnoughSamples(t *testing.T) {
	pool := newProxyPool([]string{"http://a:1", "http://b:2", "http://c:3"})
	pool.observe(pool.entries[0].url, 50*time.Millisecond)
	if got := pool.fastCutoff.Load(); got != 0 {
		t.Fatalf("cutoff computed from 1 sample: %v", time.Duration(got))
	}
	if got := pool.lastCutoffCalc.Load(); got != 0 {
		t.Fatal("early bail consumed the recompute slot, delaying steering by 30s")
	}
}
