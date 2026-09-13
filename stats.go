package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream status counters. Without these a 403 (headers/protocol — a proxy
// can't fix it) and a 429 (rate limit — a proxy can) are indistinguishable in
// the logs. Once a minute, one line per host that answered anything but success.

type hostCounts struct {
	total, proxied, forbidden, limited, serverErr, failed atomic.Int64
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
		}
		return true
	})
}

func logUpstreamStats(every time.Duration) {
	for range time.Tick(every) {
		flushUpstreamStats()
	}
}
