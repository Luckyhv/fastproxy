package main

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"
)

// Metrics are plain atomic counters — a few nanoseconds per request, no locks,
// no allocation. The dashboard polls /stats and does all the math (rates,
// sparklines) client-side, so the server stays as cheap as the proxy itself.
var (
	mTotalReq    int64 // every accepted request (slot acquired)
	mActive      int64 // currently in-flight requests
	mBytesDown   int64 // bytes sent to clients — egress (the bandwidth bill)
	mBytesUp     int64 // bytes read from source CDNs — ingress (origin pull)
	mManifests   int64 // .m3u8 manifests rewritten
	mRedirects   int64 // 3xx re-wrapped back through us
	mUpstreamErr int64 // upstream fetch / gateway failures
	mRejected    int64 // bad/missing/forbidden token (4xx client rejects)
	mBusy        int64 // shed with 503 because MAX_CONCURRENT was full
	startTime    = time.Now()
)

// countingReadCloser tallies bytes as they're read from an upstream response
// body, so a single wrap at the fetch site covers both the streaming path and
// the manifest read. Adds one atomic per Read — negligible.
type countingReadCloser struct {
	rc io.ReadCloser
	n  *int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		atomic.AddInt64(c.n, int64(n))
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

// snapshot is the JSON shape the dashboard consumes.
type snapshot struct {
	Active       int64  `json:"active"`
	Capacity     int    `json:"capacity"` // 0 = unlimited
	TotalReq     int64  `json:"total_requests"`
	BytesDown    int64  `json:"bytes_down"`
	BytesUp      int64  `json:"bytes_up"`
	Manifests    int64  `json:"manifests"`
	Redirects    int64  `json:"redirects"`
	UpstreamErr  int64  `json:"upstream_errors"`
	Rejected     int64  `json:"rejected"`
	Busy         int64  `json:"busy"`
	Goroutines   int    `json:"goroutines"`
	HeapBytes    uint64 `json:"heap_bytes"`
	UptimeSec    int64  `json:"uptime_sec"`
	NowUnixMilli int64  `json:"now_ms"`
}

func takeSnapshot() snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	cap0 := 0
	if inFlight != nil {
		cap0 = cap(inFlight)
	}
	return snapshot{
		Active:       atomic.LoadInt64(&mActive),
		Capacity:     cap0,
		TotalReq:     atomic.LoadInt64(&mTotalReq),
		BytesDown:    atomic.LoadInt64(&mBytesDown),
		BytesUp:      atomic.LoadInt64(&mBytesUp),
		Manifests:    atomic.LoadInt64(&mManifests),
		Redirects:    atomic.LoadInt64(&mRedirects),
		UpstreamErr:  atomic.LoadInt64(&mUpstreamErr),
		Rejected:     atomic.LoadInt64(&mRejected),
		Busy:         atomic.LoadInt64(&mBusy),
		Goroutines:   runtime.NumGoroutine(),
		HeapBytes:    ms.HeapAlloc,
		UptimeSec:    int64(time.Since(startTime).Seconds()),
		NowUnixMilli: time.Now().UnixMilli(),
	}
}

// statsToken gates /stats and /dashboard. If STATS_TOKEN is unset, both 404 —
// we never expose metrics by accident on a public domain.
func statsToken() string { return getenv("STATS_TOKEN", "") }

func authed(r *http.Request) bool {
	tok := statsToken()
	if tok == "" {
		return false
	}
	return r.URL.Query().Get("token") == tok
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.NotFound(w, r) // hide existence when token is wrong/unset
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(takeSnapshot())
}

//go:embed dashboard.html
var dashboardHTML []byte

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	// The page is served without the token (the token rides in the URL #hash,
	// which never reaches the server) but only when STATS_TOKEN is configured.
	if statsToken() == "" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardHTML)
}
