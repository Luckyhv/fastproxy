package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
