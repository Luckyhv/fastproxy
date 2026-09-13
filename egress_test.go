package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func withEgress(t *testing.T, c *egressConfig) {
	t.Helper()
	old, oldErr := egress, egressErr
	egress, egressErr = c, nil
	t.Cleanup(func() { egress, egressErr = old, oldErr })
}

func testExits(n int) []*url.URL {
	out := make([]*url.URL, n)
	for i := range out {
		out[i], _ = url.Parse(fmt.Sprintf("http://exit%d.test:8080", i))
	}
	return out
}

func okResponse(r *http.Request, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("segment")), Request: r}
}

// One viewer stays on one exit; different viewers spread across the pool.
func TestExitIsStickyPerViewerAndSpreads(t *testing.T) {
	c := newEgress(testExits(8), []string{"uwu"}, nil)
	first := c.pick("1.1.1.1|cdn.test", 0)
	for i := 0; i < 10; i++ {
		if c.pick("1.1.1.1|cdn.test", 0) != first {
			t.Fatal("viewer changed exits between segments")
		}
	}
	used := map[int]bool{}
	for i := 0; i < 64; i++ {
		used[c.pick(fmt.Sprintf("10.0.0.%d|cdn.test", i), 0)] = true
	}
	if len(used) < 4 {
		t.Fatalf("64 viewers used only %d of 8 exits", len(used))
	}
	if c.pick("1.1.1.1|cdn.test", 1) == first {
		t.Fatal("retry must use a different exit")
	}
	c.markDown(first)
	if c.pick("1.1.1.1|cdn.test", 0) == first {
		t.Fatal("cooling-down exit still selected")
	}
}

// A dead exit costs one failed attempt, then the request succeeds on the next.
func TestFailedExitRetriesOnNext(t *testing.T) {
	withEgress(t, newEgress(testExits(3), []string{"uwu"}, nil))
	var bad *url.URL
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		p, _ := proxyForRequest(r)
		if p == nil {
			t.Fatal("forced server went direct")
		}
		if bad == nil {
			bad = p
		}
		if p == bad {
			return nil, errors.New("proxyconnect tcp: connection refused")
		}
		return okResponse(r, 200), nil
	})}
	req, _ := http.NewRequest("GET", "https://cdn.test/seg.ts", nil)
	resp, err := fetchUpstream(client, req, "uwu", "1.1.1.1")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
	if egress.proxies[egress.pick("1.1.1.1|cdn.test", 0)] == bad {
		t.Fatal("failed exit not skipped afterwards")
	}
}

// Unlisted hosts go direct; a 429 moves the host onto the pool.
func TestDirectFirstFallsBackOn429(t *testing.T) {
	withEgress(t, newEgress(testExits(2), []string{"uwu"}, nil))
	var direct, proxied atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if p, _ := proxyForRequest(r); p != nil {
			proxied.Add(1)
			return okResponse(r, 200), nil
		}
		direct.Add(1)
		return okResponse(r, 429), nil
	})}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("GET", "https://kiwi-cdn.test/seg.ts", nil)
		resp, err := fetchUpstream(client, req, "kiwi", "1.1.1.1")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("resp=%v err=%v", resp, err)
		}
		resp.Body.Close()
	}
	if direct.Load() != 1 || proxied.Load() != 3 {
		t.Fatalf("direct=%d proxied=%d, want 1 and 3", direct.Load(), proxied.Load())
	}

	// A non-429 failure is a header/protocol problem a proxy can't fix.
	direct.Store(0)
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if p, _ := proxyForRequest(r); p != nil {
			t.Fatal("403 must not trigger proxying")
		}
		direct.Add(1)
		return okResponse(r, 403), nil
	})
	req, _ := http.NewRequest("GET", "https://other.test/seg.ts", nil)
	resp, _ := fetchUpstream(client, req, "wave", "1.1.1.1")
	resp.Body.Close()
	if resp.StatusCode != 403 || direct.Load() != 1 {
		t.Fatal("403 was retried")
	}
}

// End to end through a real HTTP proxy, credentials included.
func TestConfiguredProxyUsed(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Host != "public.example" {
			t.Errorf("target = %s", r.URL.Host)
		}
		if r.Header.Get("Proxy-Authorization") == "" {
			t.Error("missing proxy auth")
		}
		w.Write([]byte("segment"))
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("user", "pass")
	withEgress(t, newEgress([]*url.URL{u}, []string{"uwu"}, nil))
	tr := &http.Transport{Proxy: proxyForRequest}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "http://public.example/seg.ts", nil)
	resp, err := fetchUpstream(&http.Client{Transport: tr}, req, "uwu", "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "segment" || hits.Load() != 1 {
		t.Fatal("proxy response not delivered")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	if clientIP(r) != "127.0.0.1" {
		t.Fatal("remote addr")
	}
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 127.0.0.1")
	if clientIP(r) != "9.9.9.9" {
		t.Fatal("forwarded-for")
	}
	r.Header.Set("CF-Connecting-IP", "8.8.8.8")
	if clientIP(r) != "8.8.8.8" {
		t.Fatal("cloudflare header must win")
	}
}
