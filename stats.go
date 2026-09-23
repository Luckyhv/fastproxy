package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream status counters. Without these a 403 (headers/protocol — a proxy
// can't fix it) and a 429 (rate limit — a proxy can) are indistinguishable in
// the logs. Once a minute, one line per host that answered anything but success.

type hostCounts struct {
	total, proxied, forbidden, limited, serverErr, failed atomic.Int64
	// forbidSample describes the first 403 of the window. A count says a host
	// is refusing us; this says WHO (Cloudflare vs the origin) and what we sent,
	// which is what decides the fix: headers, a fresh token, or a different IP.
	forbidSample atomic.Pointer[string]
}

var upstreamCounts sync.Map // host → *hostCounts

func recordUpstream(host string, resp *http.Response, err error, proxied bool) {
	if err != nil && errors.Is(err, context.Canceled) {
		return // viewer seeked or closed the tab
	}
	v, ok := upstreamCounts.Load(host)
	if !ok {
		v, _ = upstreamCounts.LoadOrStore(host, new(hostCounts))
	}
	c := v.(*hostCounts)
	c.total.Add(1)
	if proxied {
		c.proxied.Add(1)
	}
	switch {
	case err != nil:
		c.failed.Add(1)
	case resp.StatusCode == http.StatusForbidden:
		c.forbidden.Add(1)
		if c.forbidSample.Load() == nil {
			sample := describeForbidden(resp, proxied)
			c.forbidSample.CompareAndSwap(nil, &sample)
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		c.limited.Add(1)
	case resp.StatusCode >= 500:
		c.serverErr.Add(1)
	}
}

// flushUpstreamStats logs and resets the counters. A request racing the flush
// may land on a detached counter and go uncounted; fine for a log line.
func flushUpstreamStats() {
	upstreamCounts.Range(func(k, v any) bool {
		upstreamCounts.Delete(k)
		c := v.(*hostCounts)
		if bad := c.forbidden.Load() + c.limited.Load() + c.serverErr.Load() + c.failed.Load(); bad > 0 {
			log.Printf("upstream %s: %d reqs (%d proxied) 403=%d 429=%d 5xx=%d errors=%d",
				k, c.total.Load(), c.proxied.Load(), c.forbidden.Load(), c.limited.Load(), c.serverErr.Load(), c.failed.Load())
			if sample := c.forbidSample.Load(); sample != nil {
				log.Printf("upstream %s: first 403 %s", k, *sample)
			}
		}
		return true
	})
}

func logUpstreamStats(every time.Duration) {
	for range time.Tick(every) {
		flushUpstreamStats()
	}
}

// describeForbidden summarises a 403 without logging secrets: the query (signed
// tokens) is dropped, only its presence is noted, and long path segments are
// shortened.
func describeForbidden(resp *http.Response, proxied bool) string {
	by := resp.Header.Get("Server")
	if resp.Header.Get("Cf-Mitigated") != "" {
		by += " cf-mitigated=" + resp.Header.Get("Cf-Mitigated")
	}
	if by == "" {
		by = "?"
	}
	path, query, referer := "?", "none", "-"
	if req := resp.Request; req != nil {
		path = shortPath(req.URL.Path)
		if req.URL.RawQuery != "" {
			query = "present"
		}
		if ref := req.Header.Get("Referer"); ref != "" {
			referer = ref
		}
	}
	return fmt.Sprintf("by=%q type=%q path=%s query=%s referer=%s proxied=%t",
		by, resp.Header.Get("Content-Type"), path, query, referer, proxied)
}

func shortPath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if len(seg) > 24 {
			parts[i] = seg[:8] + "…"
		}
	}
	out := strings.Join(parts, "/")
	if len(out) > 80 {
		out = out[:80] + "…"
	}
	return out
}
