package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// benchOrigin serves `size` bytes of an opaque media body, like a CDN segment.
func benchOrigin(size int) *httptest.Server {
	body := make([]byte, size)
	_, _ = rand.Read(body)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
}

// The proxy's downstream copy is what moves every byte of video. It runs
// io.CopyBuffer(w, upstreamBody, pooled-buffer) against a real http.ResponseWriter
// over a real TCP socket — the only setup that exercises the ReaderFrom path
// net/http takes, so a Recorder-based benchmark would measure the wrong code.
func benchmarkStream(b *testing.B, size int, forceBuf bool) {
	origin := benchOrigin(size)
	defer origin.Close()

	// Plain client: the production transport's dialControl blocks loopback (SSRF
	// guard), and the upstream fetch is not what we are measuring here.
	up := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := up.Get(origin.URL + "/seg.ts")
		if err != nil {
			http.Error(w, "upstream", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
		w.WriteHeader(200)
		buf := bufPool.Get().(*[]byte)
		defer bufPool.Put(buf)
		if forceBuf {
			_, _ = io.CopyBuffer(writerOnly{w}, resp.Body, *buf)
		} else {
			_, _ = io.CopyBuffer(w, resp.Body, *buf)
		}
	}))
	defer proxy.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(proxy.URL + "/stream")
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || n != int64(size) {
			b.Fatalf("copied %d/%d: %v", n, size, err)
		}
	}
}

func BenchmarkStreamReaderFrom4MB(b *testing.B)  { benchmarkStream(b, 4<<20, false) }
func BenchmarkStreamPooledBuf4MB(b *testing.B)   { benchmarkStream(b, 4<<20, true) }
func BenchmarkStreamReaderFrom32MB(b *testing.B) { benchmarkStream(b, 32<<20, false) }
func BenchmarkStreamPooledBuf32MB(b *testing.B)  { benchmarkStream(b, 32<<20, true) }

// BenchmarkRewritePlaylist measures the manifest hot loop: a 1200-segment media
// playlist with relative URIs, the shape every VOD episode starts with.
func benchPlaylist(segments int, absolute bool) []byte {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n")
	sb.WriteString(`#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x00000000000000000000000000000000` + "\n")
	for i := 0; i < segments; i++ {
		sb.WriteString("#EXTINF:6.000,\n")
		if absolute {
			fmt.Fprintf(&sb, "https://cdn.example.com/v/abcdef0123456789/seg-%05d.ts\n", i)
		} else {
			fmt.Fprintf(&sb, "seg-%05d.ts\n", i)
		}
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	return []byte(sb.String())
}

func benchmarkRewrite(b *testing.B, absolute bool) {
	benchmarkRewriteSize(b, 1200, absolute)
}

func benchmarkRewriteSize(b *testing.B, segments int, absolute bool) {
	body := benchPlaylist(segments, absolute)
	base, _ := url.Parse("https://cdn.example.com/v/abcdef0123456789/index.m3u8")
	r := httptest.NewRequest("GET", "http://proxy.example.com/m3u8/tok", nil)
	self := proxyBaseURL(r)
	encode := func(uri string) string {
		return self + proxyRoute(uri) + EncodePayload(uri, "https://referer.site/", "wave")
	}

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := rewritePlaylist(body, base, encode); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}

func BenchmarkRewriteRelative(b *testing.B) { benchmarkRewrite(b, false) }
func BenchmarkRewriteAbsolute(b *testing.B) { benchmarkRewrite(b, true) }

// BenchmarkBufferSize re-runs the "does buffer size buy throughput?" question,
// which was previously measured as "no" (1 MiB and 64 KiB within noise of each
// other). That answer was an artifact: the buffer was being discarded by
// io.CopyBuffer's ReaderFrom shortcut, so BOTH arms actually ran at net/http's
// 32 KB. With writerOnly in place the buffer is finally the thing under test.
func benchmarkBufferSize(b *testing.B, bufSize int) {
	const size = 8 << 20
	origin := benchOrigin(size)
	defer origin.Close()
	up := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := up.Get(origin.URL + "/seg.ts")
		if err != nil {
			http.Error(w, "upstream", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
		w.WriteHeader(200)
		buf := make([]byte, bufSize)
		_, _ = io.CopyBuffer(writerOnly{w}, resp.Body, buf)
	}))
	defer proxy.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(proxy.URL)
		if err != nil {
			b.Fatal(err)
		}
		if n, err := io.Copy(io.Discard, resp.Body); err != nil || n != size {
			b.Fatalf("copied %d: %v", n, err)
		}
		resp.Body.Close()
	}
}

func BenchmarkBuffer32K(b *testing.B)  { benchmarkBufferSize(b, 32<<10) }
func BenchmarkBuffer64K(b *testing.B)  { benchmarkBufferSize(b, 64<<10) }
func BenchmarkBuffer256K(b *testing.B) { benchmarkBufferSize(b, 256<<10) }
func BenchmarkBuffer1M(b *testing.B)   { benchmarkBufferSize(b, 1<<20) }

func BenchmarkEncodePayload(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if EncodePayload("https://cdn.example.com/v/abcdef0123456789/seg-00001.ts", "https://referer.site/", "wave") == "" {
			b.Fatal("empty token")
		}
	}
}

func BenchmarkDecodePayload(b *testing.B) {
	token := encodePayloadAt("https://cdn.example.com/v/abcdef0123456789/seg-00001.ts", "https://referer.site/", "wave", 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, ok := DecodePayload(token); !ok {
			b.Fatal("invalid token")
		}
	}
}

func BenchmarkExtractPayload(b *testing.B) {
	path := "/stream/signed-token/segment.ts"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if extractPayload(path) != "signed-token" {
			b.Fatal("wrong token")
		}
	}
}

// Exercise the complete production handler over TCP, including token decoding,
// response headers, the resume wrapper, and the pooled streaming copy.
func benchmarkProxySegment(b *testing.B, size int) {
	origin := benchOrigin(size)
	defer origin.Close()
	oldClient, oldEgress := httpClient, egress
	defer func() { httpClient, egress = oldClient, oldEgress }()
	egress = newEgress(nil, nil, nil)
	transport := newClient().Transport.(*http.Transport)
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	httpClient = &http.Client{Transport: transport}
	defer httpClient.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(handleProxy))
	defer proxy.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	defer client.CloseIdleConnections()
	target := proxy.URL + "/stream/" + encodePayloadAt("http://cdn.test/segment.ts", "https://player.test/", "wave", 0)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(target)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || n != int64(size) {
			b.Fatalf("status=%d, copied %d/%d: %v", resp.StatusCode, n, size, err)
		}
	}
}

func BenchmarkProxySegment4MB(b *testing.B)  { benchmarkProxySegment(b, 4<<20) }
func BenchmarkProxySegment32MB(b *testing.B) { benchmarkProxySegment(b, 32<<20) }

func BenchmarkRewriteLiveRelative(b *testing.B) { benchmarkRewriteSize(b, 6, false) }
func BenchmarkRewriteLiveAbsolute(b *testing.B) { benchmarkRewriteSize(b, 6, true) }

func BenchmarkEncodePayloadParallel(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if EncodePayload("https://cdn.test/seg.ts", "https://player.test/", "wave") == "" {
				b.Error("empty token")
				return
			}
		}
	})
}
