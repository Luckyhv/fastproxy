package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// applyUpstreamHeaders runs on every single segment request, so it sits directly
// on the hot path.
func BenchmarkApplyUpstreamHeaders(b *testing.B) {
	u, _ := url.Parse("https://vault-12.owocdn.top/stream/12/13/abc/seg-1.jpg")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		// Build the request once: we're measuring the header stamping, not
		// net/http's request construction.
		req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
		for pb.Next() {
			applyUpstreamHeaders(req, u, "", "uwu")
		}
	})
}

// The hostname-alias path is what legacy/cached tokens take.
func BenchmarkLookupServerByHost(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lookupServer("vault-12.owocdn.top", "")
	}
}

func BenchmarkLookupServerByName(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lookupServer("vault-12.owocdn.top", "uwu")
	}
}

// A miss walks every label and finds nothing — the worst case.
func BenchmarkLookupServerMiss(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lookupServer("a.b.c.d.cdn.example.test", "")
	}
}

func buildPlaylist(segments int) []byte {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n")
	sb.WriteString(`#EXT-X-KEY:METHOD=AES-128,URI="https://cdn.example.test/key/mon.key"` + "\n")
	for i := 0; i < segments; i++ {
		fmt.Fprintf(&sb, "#EXTINF:10.010,\nhttps://cdn.example.test/stream/abcdef0123456789/file-%d-f1-v1-a1.ts\n", i)
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	return []byte(sb.String())
}

// One Piece-scale playlists are ~1200 segments; each child URL gets re-encoded.
func BenchmarkRewritePlaylist1200(b *testing.B) {
	body := buildPlaylist(1200)
	base, _ := url.Parse("https://cdn.example.test/stream/abcdef0123456789/index.m3u8")
	encode := func(uri string) string { return "/stream/" + EncodePayload(uri, "", "uwu") }
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := rewritePlaylist(body, base, encode); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}

func BenchmarkEncodePayload(b *testing.B) {
	u := "https://cdn.example.test/stream/abcdef0123456789/file-100-f1-v1-a1.ts"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		EncodePayload(u, "", "uwu")
	}
}

// The 7-byte manifest sniff now runs on every extension-less 200, so prove the
// stitched-back stream still yields the original bytes exactly — including the
// awkward sizes right around the peek length.
func TestSniffStitchPreservesBody(t *testing.T) {
	for _, size := range []int{0, 1, 3, 6, 7, 8, 4096} {
		original := bytes.Repeat([]byte{0x47, 0x01, 0x02, 0x03}, size)[:size]
		body := io.NopCloser(bytes.NewReader(original))

		peek := make([]byte, len("#EXTM3U"))
		n, _ := io.ReadFull(body, peek)
		stitched := readerCloser{io.MultiReader(bytes.NewReader(peek[:n]), body), body}

		got, err := io.ReadAll(stitched)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("size %d: stitched body differs (got %d bytes, want %d)", size, len(got), len(original))
		}
	}
}

// Concurrent requests share defaultHeaders' value slices; the race detector
// should stay quiet.
func TestConcurrentHeaderStamping(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw := fmt.Sprintf("https://host-%d.owocdn.top/a.m3u8", i)
			u, _ := url.Parse(raw)
			req, _ := http.NewRequest(http.MethodGet, raw, nil)
			applyUpstreamHeaders(req, u, "", "uwu")
			req.Header.Set("Range", "bytes=0-1")
			if req.Header.Get("Origin") != "https://kwik.cx" {
				t.Errorf("goroutine %d got Origin %q", i, req.Header.Get("Origin"))
			}
		}(i)
	}
	wg.Wait()
}
