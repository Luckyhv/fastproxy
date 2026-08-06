package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	// Dev helper:  `fastproxy encode <url> [referer] [server]`  prints a path token
	// you can paste after /stream/ to test. Not part of the server path.
	if len(os.Args) > 1 && os.Args[1] == "encode" {
		ref, server := "", ""
		if len(os.Args) > 3 {
			ref = os.Args[3]
		}
		if len(os.Args) > 4 {
			server = os.Args[4]
		}
		fmt.Println(EncodePayload(os.Args[2], ref, server))
		return
	}

	// Detect cores + RAM and size runtime defaults (GOMAXPROCS, MAX_CONCURRENT).
	autoTune()

	if len(allowedHosts) > 0 {
		log.Printf("origin allow-list active (incl. subdomains): %v", allowedHosts)
	} else {
		log.Printf("origin allow-list: OPEN — set ALLOWED_ORIGINS to restrict")
	}
	if upstreamProxyPool.len() > 0 {
		log.Printf("upstream proxy pool active: %d proxies, servers=%v domains=%v, cooldowns rate=%s error=%s 5xx=%s",
			upstreamProxyPool.len(), upstreamProxyServers, upstreamProxyDomains,
			proxyRatePenalty, proxyErrorPenalty, proxyServerPenalty)
	} else {
		log.Printf("upstream proxy pool: disabled")
	}

	// BIND_ADDR lets you restrict the listen interface. Behind a reverse proxy
	// (Caddy/nginx) on the same box, set BIND_ADDR=127.0.0.1 so the proxy is not
	// reachable from the public internet directly. Empty = all interfaces.
	addr := getenv("BIND_ADDR", "") + ":" + getenv("PORT", "3847")

	mux := http.NewServeMux()
	mux.Handle("/", withCORS(handleProxy))

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,

		// Time allowed to read just the request *headers*. Protects against a
		// client that opens a connection and dribbles bytes forever (Slowloris).
		ReadHeaderTimeout: 10 * time.Second,

		// NOTE: we deliberately set NO WriteTimeout. WriteTimeout caps the whole
		// response, which for a 2-hour video stream would guillotine the download
		// mid-play. We bound *stalled* connections via the request context + the
		// idle timeout instead, not by a hard response deadline.
		IdleTimeout: 120 * time.Second,
	}

	// Run the server on its own goroutine so main() can sit and wait for a
	// shutdown signal below.
	go func() {
		log.Printf("fastproxy listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Graceful shutdown: wait for Ctrl-C / SIGTERM (what your process manager or
	// `docker stop` sends), then stop accepting new connections and give
	// in-flight streams up to 10s to finish before exiting.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func getenv(k, def string) string {
	ensureEnv() // load .env once, before the first config read
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getenvDuration reads a duration knob. Accepts a Go duration ("90s", "5m") or
// a bare number of seconds; anything unparseable falls back to def.
func getenvDuration(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(getenv(k, ""))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
