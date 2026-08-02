package main

import (
	"context"
	"net/http"
	"net/url"
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
