package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// Child URLs emitted by a rewritten manifest must carry the SAME provider key,
// or segments get a different Origin/Referer than the manifest did and 403.
func TestManifestChildrenInheritServer(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-ENDLIST\nseg1.ts\n"
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": {"application/vnd.apple.mpegurl"}},
		Body:       io.NopCloser(strings.NewReader(playlist)),
	}
	base, _ := url.Parse("https://vault-9.owocdn.top/hls/master.m3u8")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://proxy.test/m3u8/token", nil)

	serveManifest(rec, req, resp, base, "https://animex.example/", "uwu")

	var child string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if i := strings.LastIndexByte(line, '/'); strings.HasPrefix(line, "http://proxy.test/") {
			child = line[i+1:]
		}
	}
	if child == "" {
		t.Fatalf("no rewritten child URL in output:\n%s", rec.Body.String())
	}
	gotURL, gotRef, gotServer, ok := DecodePayload(child)
	if !ok {
		t.Fatal("child token did not decode")
	}
	if gotServer != "uwu" {
		t.Errorf("server = %q, want %q", gotServer, "uwu")
	}
	if gotRef != "https://animex.example/" {
		t.Errorf("referer = %q", gotRef)
	}
	if gotURL != "https://vault-9.owocdn.top/hls/seg1.ts" {
		t.Errorf("url = %q", gotURL)
	}
}

// The segment request built from that child token must come out as the uwu
// identity on the h2 path — the whole point of the port.
func TestSegmentRequestGetsUwuIdentity(t *testing.T) {
	target, _ := url.Parse("https://vault-9.owocdn.top/hls/seg1.ts")
	req, _ := http.NewRequest("GET", target.String(), nil)
	req.Header.Set("Range", "bytes=0-1023")

	useHTTP2 := applyUpstreamHeaders(req, target, "https://animex.example/", "uwu")

	if !useHTTP2 {
		t.Error("uwu must route over HTTP/2")
	}
	if got := req.Header.Get("Referer"); got != "https://kwik.cx/" {
		t.Errorf("Referer = %q, want https://kwik.cx/", got)
	}
	if got := req.Header.Get("Origin"); got != "https://kwik.cx" {
		t.Errorf("Origin = %q, want https://kwik.cx", got)
	}
	if got := req.Header.Get("Range"); got != "bytes=0-1023" {
		t.Errorf("Range clobbered by the header baseline: %q", got)
	}
	if !strings.Contains(req.Header.Get("User-Agent"), "Chrome/126") {
		t.Errorf("browser baseline missing: %q", req.Header.Get("User-Agent"))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManifestResponses(t *testing.T) {
	old := httpClient
	defer func() { httpClient = old }()
	for _, tc := range []struct {
		name, method string
		status       int
		headers      http.Header
		body         string
		want         int
	}{
		{"head", "HEAD", 200, http.Header{"Content-Type": {"application/vnd.apple.mpegurl"}}, "#EXTM3U\nseg.ts\n", 200},
		{"not modified", "GET", 304, http.Header{"Etag": {"abc"}}, "", 304},
		{"forbidden", "GET", 403, http.Header{"Retry-After": {"10"}}, "denied", 403},
		{"challenge", "GET", 200, http.Header{"Cf-Mitigated": {"challenge"}}, "<html>challenge</html>", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: tc.headers, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}
			req := httptest.NewRequest(tc.method, "http://proxy.test/stream/"+EncodePayload("https://cdn.example/index.m3u8", "", ""), nil)
			w := httptest.NewRecorder()
			handleProxy(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d", w.Code)
			}
			if (tc.method == "HEAD" || tc.status == 304) && w.Body.Len() != 0 {
				t.Fatal("unexpected body")
			}
			if tc.status == 304 && w.Header().Get("Cache-Control") != "" {
				t.Fatal("revalidation must preserve cached freshness")
			}
			if tc.status == 304 && w.Header().Get("Etag") != "abc" {
				t.Fatal("lost validator")
			}
			if tc.status == 403 && w.Header().Get("Retry-After") != "10" {
				t.Fatal("lost Retry-After")
			}
			if tc.want >= 400 && !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("cacheable error")
			}
		})
	}
}

func TestOversizedManifestRejected(t *testing.T) {
	body := "#EXTM3U\n" + strings.Repeat("x", maxManifestSize)
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	base, _ := url.Parse("https://cdn.test/index.m3u8")
	w := httptest.NewRecorder()
	serveManifest(w, httptest.NewRequest("GET", "http://proxy.test/", nil), resp, base, "", "")
	if w.Code != 502 {
		t.Fatalf("oversized playlist returned %d", w.Code)
	}
}

func TestSmallMemoryPoolBudget(t *testing.T) {
	t.Setenv("MAX_IDLE_CONNS", "")
	t.Setenv("MAX_IDLE_CONNS_PER_HOST", "")
	defer tuneTransports(0)
	const mem = 256 << 20
	tuneTransports(mem)
	a := httpClient.Transport.(*http.Transport)
	b := h2Client.Transport.(*http.Transport)
	if (a.MaxIdleConns+b.MaxIdleConns)*idleConnCost > mem/idleConnRAMShare {
		t.Fatal("combined idle pools exceed RAM budget")
	}
	if a.MaxIdleConnsPerHost > a.MaxIdleConns {
		t.Fatal("host cap exceeds pool")
	}
}

type failedBody struct{ sent bool }

func (b *failedBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial segment"), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (*failedBody) Close() error { return nil }

// Test over real HTTP: the viewer must see an incomplete response rather than
// a successful EOF for a truncated body without a Content-Length.
func TestBrokenSegmentAbortsResponse(t *testing.T) {
	old := httpClient
	defer func() { httpClient = old }()
	var fetched atomic.Int32
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		fetched.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp2t"}}, Body: &failedBody{}, Request: r}, nil
	})}
	proxy := httptest.NewServer(http.HandlerFunc(handleProxy))
	defer proxy.Close()
	resp, err := proxy.Client().Get(proxy.URL + "/stream/" + EncodePayload("https://cdn.test/seg.ts", "", ""))
	if err == nil {
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("truncated segment reported as successful")
	}
	if fetched.Load() != 1 {
		t.Fatal("must not replay partially delivered segment")
	}
}

func TestUpstreamErrorsPassThroughOnce(t *testing.T) {
	withEgress(t, newEgress(nil, nil, nil))
	for _, status := range []int{403, 429, 502, 503, 504} {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"10"}}, Body: io.NopCloser(strings.NewReader("upstream error")), Request: r}, nil
		})}
		req, _ := http.NewRequest("GET", "https://cdn.test/segment.ts", nil)
		resp, err := fetchUpstream(client, req, "", "")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if calls != 1 || resp.StatusCode != status || resp.Header.Get("Retry-After") != "10" {
			t.Fatalf("status %d was retried or changed", status)
		}
	}
}
