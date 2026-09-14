package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// isM3U8Ref replaces a url.Parse + isM3U8URL round trip in the rewrite loop, so
// it has to answer identically for every shape a playlist can hand us.
func TestIsM3U8RefMatchesIsM3U8URL(t *testing.T) {
	refs := []string{
		"https://ru-cdn1.echovideo.to/cdn/092e3d?t.m3u8",
		"https://hls.anidb.app/stream/abc/master.m3u8",
		"https://vault-12.owocdn.top/stream/12/13/x/uwu.m3u8",
		"https://hls.anidb.app/stream/abc/file-1-f1-v1.xls",
		"https://cdn.example.test/segment0.ts",
		"https://cdn.example.test/MASTER.M3U8",
		"https://cdn.example.test/x.m3u",
		"seg1.ts",
		"index.m3u8",
		"index.m3u8#frag",
		"/a/b/index.m3u8?token=abc",
		"/a/b/opaque?x=1&t.m3u8",
		"https://cdn.example.test/a?t.m3u8#frag",
		"",
	}
	for _, ref := range refs {
		u, err := url.Parse(ref)
		if err != nil {
			t.Fatalf("parse %q: %v", ref, err)
		}
		if got, want := isM3U8Ref(ref), isM3U8URL(u); got != want {
			t.Fatalf("isM3U8Ref(%q) = %v, isM3U8URL = %v", ref, got, want)
		}
	}
}

func TestHasSuffixFold(t *testing.T) {
	cases := []struct {
		s, suffix string
		want      bool
	}{
		{"a/b/x.M3U8", ".m3u8", true},
		{"a/b/x.m3u8", ".M3U8", true},
		{"x.m3u", ".m3u8", false},
		{".m3u8", ".m3u8", true},
		{"m3u8", ".m3u8", false},
		{"", ".m3u8", false},
		{"cdn.anidb.app", ".anidb.app", true},
		{"anidb.app", ".anidb.app", false},
	}
	for _, c := range cases {
		if got := hasSuffixFold(c.s, c.suffix); got != c.want {
			t.Fatalf("hasSuffixFold(%q, %q) = %v, want %v", c.s, c.suffix, got, c.want)
		}
	}
}

// The token path skips url.Query() entirely, so make sure the raw-URL path is
// still detected exactly when it should be.
func TestRawURLQuery(t *testing.T) {
	cases := map[string]bool{
		"/stream/sometoken":                      false,
		"/stream/sometoken?":                     false,
		"/stream/sometoken?foo=bar":              false,
		"/stream?url=":                           false,
		"/stream?url=https%3A%2F%2Fa.test%2Fx":   true,
		"/stream?ref=x&url=https%3A%2F%2Fa.test": true,
	}
	for target, want := range cases {
		r := httptest.NewRequest("GET", target, nil)
		if got := rawURLQuery(r) != nil; got != want {
			t.Fatalf("rawURLQuery(%q) present = %v, want %v", target, got, want)
		}
	}
}

// serveManifest hoists the proxy base out of the per-line closure; the hoisted
// form must still produce exactly what proxyURLFor does.
func TestProxyURLForMatchesHoistedForm(t *testing.T) {
	r := httptest.NewRequest("GET", "http://proxy.test/m3u8/tok", nil)
	for _, uri := range []string{
		"https://cdn.test/seg1.ts",
		"https://cdn.test/variant.m3u8",
		"https://cdn.test/opaque?t.m3u8",
	} {
		want := proxyURLFor(r, uri, "https://ref.test/", "wave")
		got := proxyBaseURL(r) + proxyRoute(uri) + EncodePayload(uri, "https://ref.test/", "wave")
		if got != want {
			t.Fatalf("hoisted form %q != proxyURLFor %q", got, want)
		}
	}
}

// tuneTransports keeps defaults when RAM is unknown and grows for larger hosts.
func TestTuneTransports(t *testing.T) {
	defer tuneTransports(0) // restore defaults for other tests

	// Unknown RAM keeps the default total pool; one busy CDN may park half of it.
	tuneTransports(0)
	tr := httpClient.Transport.(*http.Transport)
	if tr.MaxIdleConns != defaultMaxIdleConns {
		t.Fatalf("undetected RAM should keep the default total, got %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != defaultMaxIdleConns/2 || tr.MaxConnsPerHost != 0 {
		t.Fatalf("per-host idle=%d active=%d, want %d and unlimited", tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost, defaultMaxIdleConns/2)
	}

	tuneTransports(64 << 30) // 64 GiB
	if tr.MaxIdleConns <= defaultMaxIdleConns {
		t.Fatalf("64 GiB should raise the pool, got %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConns > maxIdleConnsCap {
		t.Fatalf("pool exceeded the cap: %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost < 64 {
		t.Fatalf("per-host cap regressed below the default: %d", tr.MaxIdleConnsPerHost)
	}
	if h2 := h2Client.Transport.(*http.Transport); h2.MaxIdleConns != tr.MaxIdleConns {
		t.Fatalf("h2 client not tuned: %d", h2.MaxIdleConns)
	}
}

// The pooled-buffer copy is the path every byte of video takes, so prove it
// delivers the body intact across many buffer boundaries — a 5 MB body is five
// full 1 MB reads plus a short tail.
func TestWriterOnlyCopyIsByteExact(t *testing.T) {
	body := make([]byte, 5<<20)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer origin.Close()

	// Plain client: the production transport's SSRF guard blocks loopback.
	up := &http.Client{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := up.Get(origin.URL)
		if err != nil {
			http.Error(w, "upstream", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
		w.WriteHeader(200)
		buf := bufPool.Get().(*[]byte)
		defer bufPool.Put(buf)
		if _, err := io.CopyBuffer(writerOnly{w}, resp.Body, *buf); err != nil {
			t.Errorf("copy: %v", err)
		}
	}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	if sha256.Sum256(got) != want {
		t.Fatal("body corrupted in transit")
	}
}

func isolateCapacity(t *testing.T) {
	t.Helper()
	slots := inFlight
	t.Cleanup(func() { inFlight = slots; tuneTransports(0) })
	for _, k := range []string{"MAX_CONCURRENT", "MAX_CONNS_PER_HOST", "MAX_IDLE_CONNS", "MAX_IDLE_CONNS_PER_HOST"} {
		t.Setenv(k, "")
	}
}
func TestCapacityMovesWithServer(t *testing.T) {
	isolateCapacity(t)
	for _, tc := range []struct {
		ram  uint64
		want int
	}{{4 << 30, 1365}, {8 << 30, 2730}, {512 << 20, 170}, {0, 0}, {64 << 20, 64}} {
		tuneConcurrency(tc.ram)
		tuneTransports(tc.ram)
		if cap(inFlight) != tc.want {
			t.Fatalf("ram=%d capacity=%d want=%d", tc.ram, cap(inFlight), tc.want)
		}
		for _, c := range []*http.Client{httpClient, h2Client} {
			tr := c.Transport.(*http.Transport)
			if tr.MaxConnsPerHost != 0 {
				t.Fatal("host cap must default to unlimited")
			}
			if tr.MaxIdleConnsPerHost > tr.MaxIdleConns {
				t.Fatal("idle host exceeds total")
			}
		}
	}
}
func TestCapacityOverridesAndReset(t *testing.T) {
	isolateCapacity(t)
	t.Setenv("MAX_CONCURRENT", "100")
	t.Setenv("MAX_CONNS_PER_HOST", "25")
	tuneConcurrency(4 << 30)
	tuneTransports(4 << 30)
	if cap(inFlight) != 100 || httpClient.Transport.(*http.Transport).MaxConnsPerHost != 25 {
		t.Fatal("operator overrides ignored")
	}
	t.Setenv("MAX_CONCURRENT", "0")
	t.Setenv("MAX_CONNS_PER_HOST", "0")
	tuneConcurrency(4 << 30)
	tuneTransports(4 << 30)
	if inFlight != nil || httpClient.Transport.(*http.Transport).MaxConnsPerHost != 0 {
		t.Fatal("explicit unlimited ignored")
	}
	t.Setenv("MAX_CONCURRENT", "invalid")
	t.Setenv("MAX_CONNS_PER_HOST", "invalid")
	tuneConcurrency(4 << 30)
	tuneTransports(4 << 30)
	if cap(inFlight) != 1365 || httpClient.Transport.(*http.Transport).MaxConnsPerHost != 0 {
		t.Fatal("invalid settings should retain safe auto defaults")
	}
}

// More than 64 simultaneous reads must reach one upstream host before it sends
// any response. This would deadlock behind the former fixed 64-connection cap.
func TestMinimumVPSConcurrentBurst(t *testing.T) {
	isolateCapacity(t)
	old := httpClient
	httpClient = newClient(false)
	defer func() { httpClient.Transport.(*http.Transport).CloseIdleConnections(); httpClient = old }()
	tuneConcurrency(4 << 30)
	tuneTransports(4 << 30)
	const count = 96
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	var arrived atomic.Int32
	ready := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == count {
			close(ready)
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = io.WriteString(w, "segment")
	}))
	defer origin.Close()
	tr := httpClient.Transport.(*http.Transport)
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	proxy := httptest.NewServer(http.HandlerFunc(handleProxy))
	defer proxy.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	target := proxy.URL + "/stream/" + EncodePayload("http://cdn.test/seg.ts", "", "")
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			resp, err := client.Get(target)
			if err != nil {
				results <- err
				return
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil && (resp.StatusCode != 200 || string(b) != "segment") {
				err = fmt.Errorf("status=%d body=%q", resp.StatusCode, b)
			}
			results <- err
		}()
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Errorf("only %d of %d requests reached upstream concurrently", arrived.Load(), count)
	}
	release()
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if len(inFlight) != 0 {
		t.Fatal("request capacity leaked")
	}
}
