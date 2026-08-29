package main

import (
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
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

// tuneTransports must never shrink the pool below the shipped defaults, and must
// honour the memory ceiling.
func TestTuneTransports(t *testing.T) {
	defer tuneTransports(0) // restore defaults for other tests

	// Undetected RAM keeps the shipped total. The per-host share still rises to a
	// quarter of it, which costs no extra memory (the total is the RAM ceiling)
	// and is the cap that actually throttles a single busy CDN.
	tuneTransports(0)
	tr := httpClient.Transport.(*http.Transport)
	if tr.MaxIdleConns != defaultMaxIdleConns {
		t.Fatalf("undetected RAM should keep the default total, got %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != defaultMaxIdleConns/4 {
		t.Fatalf("per-host share = %d, want %d", tr.MaxIdleConnsPerHost, defaultMaxIdleConns/4)
	}

	tuneTransports(64 << 30) // 64 GiB
	if tr.MaxIdleConns <= defaultMaxIdleConns {
		t.Fatalf("64 GiB should raise the pool, got %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConns > maxIdleConnsCap {
		t.Fatalf("pool exceeded the cap: %d", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost < defaultMaxIdleConnsPerHost {
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
