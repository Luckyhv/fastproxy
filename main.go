package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if msg := tokenConfigError(); msg != "" {
		log.Fatal(msg)
	}

	// Dev helper:  `fastproxy encode <url> [referer]`  prints a path token you can
	// paste after /stream/ to test. Not part of the server path.
	if len(os.Args) > 1 && os.Args[1] == "encode" {
		if len(os.Args) < 3 {
			log.Fatal("usage: fastproxy encode <url> [referer] [server]")
		}
		ref := ""
		if len(os.Args) > 3 {
			ref = os.Args[3]
		}
		srv := ""
		if len(os.Args) > 4 {
			srv = os.Args[4]
		}
		fmt.Println(EncodePayload(os.Args[2], ref, srv))
		return
	}

	if egressErr != nil {
		log.Fatal(egressErr)
	}
	log.Printf("egress: %d configured proxies", len(egress.proxies))
	go logUpstreamStats(time.Minute)
	if tokenTTL > 0 {
		log.Printf("tokens: signed, minted with %s expiry", tokenTTL)
	} else {
		log.Printf("tokens: signed, no expiry (TOKEN_TTL_SEC=0)")
	}

	// Detect cores + RAM and size runtime defaults (GOMAXPROCS, MAX_CONCURRENT).
	autoTune()

	if len(allowedHosts) > 0 {
		log.Printf("origin allow-list active (incl. subdomains): %v", allowedHosts)
	} else {
		log.Printf("origin allow-list: OPEN — set ALLOWED_ORIGINS to restrict")
	}

	// BIND_ADDR lets you restrict the listen interface. Behind a reverse proxy
	// (Caddy/nginx) on the same box, set BIND_ADDR=127.0.0.1 so the proxy is not
	// reachable from the public internet directly. Empty = all interfaces.
	addr := getenv("BIND_ADDR", "") + ":" + getenv("PORT", "3847")

	// Everything goes to the proxy handler; there are no side endpoints.
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
		// mid-play. Client disconnection cancels the upstream request. IdleTimeout
		// below covers idle keep-alive connections, not active stalled streams.
		IdleTimeout: 120 * time.Second,
	}

	ln, err := listen(addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// TLS_CERT_FILE/TLS_KEY_FILE make fastproxy terminate Cloudflare's origin
	// TLS itself. Caddy in front used as much CPU as fastproxy and 3-5x its RAM
	// (measured Oct 2026) just to decrypt and copy every byte across a second hop.
	certFile, keyFile := getenv("TLS_CERT_FILE", ""), getenv("TLS_KEY_FILE", "")
	if certFile != "" {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			log.Fatalf("tls: %v", err)
		}
	}

	// Run the server on its own goroutine so main() can sit and wait for a
	// shutdown signal below.
	go func() {
		log.Printf("fastproxy listening on %s (tls=%t)", ln.Addr(), certFile != "")
		var err error
		if certFile != "" {
			err = srv.ServeTLS(ln, certFile, keyFile)
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// listen uses the socket systemd hands over (deploy/fastproxy.socket) when
// present. systemd keeps that socket open across restarts, so connections that
// arrive mid-restart wait in its queue instead of being refused; that is what
// Caddy's lb_try_duration used to do. Otherwise it binds addr itself.
func listen(addr string) (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd-socket")
		defer f.Close() // FileListener dups it
		return net.FileListener(f)
	}
	return net.Listen("tcp", addr)
}

func getenv(k, def string) string {
	ensureEnv() // load .env once, before the first config read
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
