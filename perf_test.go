package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

// origin serves a fixed-size body, standing in for a CDN.
func origin(size int) *httptest.Server {
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
}

// proxyFor wires the real handler in front of an origin. Raw-URL mode keeps the
// benchmark focused on the copy path rather than token decoding.
//
// The origin is on loopback, which the SSRF guard rejects by design — at the
// hostname check and again at dial time. Both are relaxed for the duration of
// the benchmark and restored afterwards.
func proxyFor(t testing.TB, o *httptest.Server) *httptest.Server {
	t.Helper()

	savedCheck := isPublicHostFn
	isPublicHostFn = func(string) bool { return true }

	// The repo .env is loaded during tests, so ALLOWED_ORIGINS would 403 every
	// benchmark request (which carries no Origin/Referer). Open it for the run.
	savedOrigins := allowedHosts
	allowedHosts = nil

	savedTransport := httpClient.Transport
	tr := savedTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	if benchTransportBuf > 0 {
		tr.ReadBufferSize, tr.WriteBufferSize = benchTransportBuf, benchTransportBuf
	}
	httpClient.Transport = tr

	t.Cleanup(func() {
		isPublicHostFn = savedCheck
		allowedHosts = savedOrigins
		httpClient.Transport = savedTransport
	})
	return httptest.NewServer(withCORS(handleProxy))
}

func streamOnce(b *testing.B, proxyURL, originURL string) int64 {
	resp, err := http.Get(proxyURL + "/stream?url=" + originURL + "/seg.ts")
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		b.Fatal(err)
	}
	return n
}

// BenchmarkStreamThroughput measures the backpressured copy end to end at the
// current buffer size. Run with -benchtime to vary load.
func BenchmarkStreamThroughput(b *testing.B) {
	const size = 8 << 20 // 8 MiB, roughly two HLS segments
	o := origin(size)
	defer o.Close()
	p := proxyFor(b, o)
	defer p.Close()

	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := streamOnce(b, p.URL, o.URL); n != size {
			b.Fatalf("short read: %d", n)
		}
	}
}

// BenchmarkStreamConcurrent is the shape that actually matters: many viewers at
// once. Reports peak heap so buffer-size changes show up as memory, not just
// throughput.
func BenchmarkStreamConcurrent(b *testing.B) {
	const (
		size    = 4 << 20
		viewers = 64
	)
	o := origin(size)
	defer o.Close()
	p := proxyFor(b, o)
	defer p.Close()

	b.SetBytes(size * viewers)
	b.ResetTimer()
	var peakHeap uint64
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for v := 0; v < viewers; v++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if n := streamOnce(b, p.URL, o.URL); n != size {
					b.Errorf("short read: %d", n)
				}
			}()
		}
		// Sample the heap mid-flight, while buffers are actually checked out.
		time.Sleep(2 * time.Millisecond)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapInuse > peakHeap {
			peakHeap = ms.HeapInuse
		}
		wg.Wait()
	}
	b.ReportMetric(float64(peakHeap)/(1<<20), "peakHeapMiB")
	b.ReportMetric(float64(peakHeap)/float64(viewers)/1024, "KiB/viewer")
}

// BenchmarkTimeToFirstByte is what a viewer feels when they press play: how long
// until the first byte of a segment arrives through us.
func BenchmarkTimeToFirstByte(b *testing.B) {
	o := origin(4 << 20)
	defer o.Close()
	p := proxyFor(b, o)
	defer p.Close()

	// Warm the connection pool so we measure the steady state, not the handshake.
	streamOnce(b, p.URL, o.URL)

	b.ReportAllocs()
	b.ResetTimer()
	var total time.Duration
	for i := 0; i < b.N; i++ {
		start := time.Now()
		resp, err := http.Get(p.URL + "/stream?url=" + o.URL + "/seg.ts")
		if err != nil {
			b.Fatal(err)
		}
		one := make([]byte, 1)
		if _, err := io.ReadFull(resp.Body, one); err != nil {
			b.Fatal(err)
		}
		total += time.Since(start)
		resp.Body.Close()
	}
	b.ReportMetric(float64(total.Microseconds())/float64(b.N), "µs/TTFB")
}

// slowOrigin models a real CDN rather than loopback: bounded bandwidth and a
// per-chunk delay, so buffer size is judged under the conditions that actually
// apply in production instead of at 3 GB/s.
func slowOrigin(size int, perChunkDelay time.Duration, chunk int) *httptest.Server {
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for off := 0; off < len(payload); off += chunk {
			end := min(off+chunk, len(payload))
			if _, err := w.Write(payload[off:end]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(perChunkDelay)
		}
	}))
}

// BenchmarkBufferSizeSweep is the evidence for streamBufSize: throughput and
// per-viewer memory at each candidate, under CDN-like conditions.
func BenchmarkBufferSizeSweep(b *testing.B) {
	const (
		size    = 2 << 20
		viewers = 32
	)
	saved := streamBufSize
	b.Cleanup(func() { streamBufSize = saved })

	for _, sz := range []int{32 << 10, 64 << 10, 128 << 10, 256 << 10, 512 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("buf=%dKiB", sz>>10), func(b *testing.B) {
			streamBufSize = sz
			// ~40 MB/s per stream with 1 ms of latency per 64 KiB chunk.
			o := slowOrigin(size, time.Millisecond, 64<<10)
			defer o.Close()
			p := proxyFor(b, o)
			defer p.Close()

			b.SetBytes(size * viewers)
			b.ResetTimer()
			var peak uint64
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				for v := 0; v < viewers; v++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if n := streamOnce(b, p.URL, o.URL); n != size {
							b.Errorf("short read: %d", n)
						}
					}()
				}
				time.Sleep(3 * time.Millisecond)
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak {
					peak = ms.HeapInuse
				}
				wg.Wait()
			}
			b.ReportMetric(float64(peak)/float64(viewers)/1024, "KiB/viewer")
		})
	}
}

// BenchmarkSingleStreamCeiling checks the opposite extreme from the sweep: one
// stream on loopback, where syscall count (not the network) is the limit. If a
// smaller buffer were going to cost throughput anywhere, it would be here.
func BenchmarkSingleStreamCeiling(b *testing.B) {
	const size = 8 << 20
	saved := streamBufSize
	b.Cleanup(func() { streamBufSize = saved })

	for _, sz := range []int{32 << 10, 64 << 10, 256 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("buf=%dKiB", sz>>10), func(b *testing.B) {
			streamBufSize = sz
			o := origin(size)
			defer o.Close()
			p := proxyFor(b, o)
			defer p.Close()
			b.SetBytes(size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if n := streamOnce(b, p.URL, o.URL); n != size {
					b.Fatalf("short read: %d", n)
				}
			}
		})
	}
}

// benchTransportBuf overrides the Transport's per-connection read/write buffers
// for the sweep below. 0 = leave the production values alone.
var benchTransportBuf int

// BenchmarkTransportBufferSweep sizes the per-connection buffers. These are
// charged per upstream connection, idle ones included, so with MaxIdleConns in
// the hundreds they are a real memory term — not just a per-stream one.
func BenchmarkTransportBufferSweep(b *testing.B) {
	const (
		size    = 2 << 20
		viewers = 32
	)
	b.Cleanup(func() { benchTransportBuf = 0 })

	for _, sz := range []int{16 << 10, 32 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("tbuf=%dKiB", sz>>10), func(b *testing.B) {
			benchTransportBuf = sz
			o := slowOrigin(size, time.Millisecond, 64<<10)
			defer o.Close()
			p := proxyFor(b, o)
			defer p.Close()

			b.SetBytes(size * viewers)
			b.ResetTimer()
			var peak uint64
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				for v := 0; v < viewers; v++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if n := streamOnce(b, p.URL, o.URL); n != size {
							b.Errorf("short read: %d", n)
						}
					}()
				}
				time.Sleep(3 * time.Millisecond)
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak {
					peak = ms.HeapInuse
				}
				wg.Wait()
			}
			b.ReportMetric(float64(peak)/float64(viewers)/1024, "KiB/viewer")
		})
	}
}

// BenchmarkBufferConfigs compares the shipped buffer sizing against the
// memory-lean alternative on one harness. Shipped is 1 MiB / 64 KiB, chosen
// deliberately; this benchmark exists so the cost of that choice stays visible
// and anyone revisiting it has numbers instead of intuition.
func BenchmarkBufferConfigs(b *testing.B) {
	const (
		size    = 2 << 20
		viewers = 32
	)
	savedStream := streamBufSize
	b.Cleanup(func() { streamBufSize = savedStream; benchTransportBuf = 0 })

	configs := []struct {
		name         string
		stream, tbuf int
	}{
		{"shipped_1MiB_stream_64KiB_transport", 1 << 20, 64 << 10},
		{"lean_64KiB_stream_32KiB_transport", 64 << 10, 32 << 10},
	}
	for _, c := range configs {
		b.Run(c.name, func(b *testing.B) {
			streamBufSize, benchTransportBuf = c.stream, c.tbuf
			o := slowOrigin(size, time.Millisecond, 64<<10)
			defer o.Close()
			p := proxyFor(b, o)
			defer p.Close()

			b.SetBytes(size * viewers)
			b.ResetTimer()
			var peak uint64
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				for v := 0; v < viewers; v++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if n := streamOnce(b, p.URL, o.URL); n != size {
							b.Errorf("short read: %d", n)
						}
					}()
				}
				time.Sleep(3 * time.Millisecond)
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak {
					peak = ms.HeapInuse
				}
				wg.Wait()
			}
			b.ReportMetric(float64(peak)/(1<<20), "peakHeapMiB")
			b.ReportMetric(float64(peak)/float64(viewers)/1024, "KiB/viewer")
		})
	}
}

// BenchmarkProxyOverheadTTFB isolates the latency the proxy itself adds, by
// timing the same first-byte fetch directly against the origin and through us.
// Everything else in a real stream's start-up time is network RTT we don't own.
func BenchmarkProxyOverheadTTFB(b *testing.B) {
	o := origin(4 << 20)
	defer o.Close()
	p := proxyFor(b, o)
	defer p.Close()

	ttfb := func(url string) time.Duration {
		start := time.Now()
		resp, err := http.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		defer resp.Body.Close()
		one := make([]byte, 1)
		if _, err := io.ReadFull(resp.Body, one); err != nil {
			b.Fatal(err)
		}
		return time.Since(start)
	}

	direct := o.URL + "/seg.ts"
	proxied := p.URL + "/stream?url=" + o.URL + "/seg.ts"
	ttfb(direct)  // warm both connection pools
	ttfb(proxied) //

	b.ResetTimer()
	var dTotal, pTotal time.Duration
	for i := 0; i < b.N; i++ {
		dTotal += ttfb(direct)
		pTotal += ttfb(proxied)
	}
	n := float64(b.N)
	b.ReportMetric(float64(dTotal.Microseconds())/n, "µs/direct")
	b.ReportMetric(float64(pTotal.Microseconds())/n, "µs/proxied")
	b.ReportMetric(float64((pTotal-dTotal).Microseconds())/n, "µs/overhead")
}
