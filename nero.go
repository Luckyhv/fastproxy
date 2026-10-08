package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ─── nero (animex) ───────────────────────────────────────────────────────────
//
// nero's master and media playlists are locked to the IP + User-Agent that
// called animex's /sources endpoint (measured Oct 7 2026: same IP + UA → 200,
// a different UA or a different IP → 403, whatever the Origin). Its segments are
// NOT locked and allow anikage.cc. So:
//
//   - the API's token for a nero source is animex's /sources URL itself;
//   - this box calls it, then fetches the master and the media playlist, all
//     direct (never through the egress pool) with one fixed User-Agent;
//   - the viewer gets the media playlist with every segment pointing straight
//     at nero's CDN, so no video bytes go through the proxy.
//
// It is ONE request on purpose. Cloudflare may send the next request to a
// different box, and that box's IP would not match the lock.

// isNeroMint reports whether a decoded token is a nero source to mint here.
// Only the /sources URL is: subtitles and anything else tagged "nero" stream
// through the normal path.
func isNeroMint(target *url.URL, server string) bool {
	server, _ = splitServer(server)
	return strings.EqualFold(strings.TrimSpace(server), "nero") &&
		strings.EqualFold(target.Hostname(), animexRESTEndpoint.Hostname())
}

func serveNero(w http.ResponseWriter, r *http.Request, mint *url.URL) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	playlist, err := neroPlaylist(r, mint)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("nero: %v", err)
		}
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.apple.mpegurl")
	// Browser-only: the segment URLs carry no token, but the playlist was minted
	// for this box and must never be pinned at the edge.
	setCC(h, "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(playlist)
	}
}

// neroPlaylist runs the three locked hops and returns the rewritten media playlist.
func neroPlaylist(r *http.Request, mint *url.URL) ([]byte, error) {
	body, _, err := neroGet(r, mint, true)
	if err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	var payload struct {
		Sources []struct {
			URL string `json:"url"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("sources: bad json")
	}
	var masterURL *url.URL
	for _, s := range payload.Sources {
		if u, err := url.Parse(s.URL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && isPublicHost(u.Hostname()) {
			masterURL = u
			break
		}
	}
	if masterURL == nil {
		return nil, fmt.Errorf("sources: no playable url")
	}

	media, mediaURL, err := neroGet(r, masterURL, false)
	if err != nil {
		return nil, fmt.Errorf("master: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(media), []byte("#EXTM3U")) {
		return nil, fmt.Errorf("master: not a playlist")
	}
	if variant := bestVariant(media, mediaURL); variant != nil {
		if media, mediaURL, err = neroGet(r, variant, false); err != nil {
			return nil, fmt.Errorf("variant: %w", err)
		}
		if !bytes.HasPrefix(bytes.TrimSpace(media), []byte("#EXTM3U")) {
			return nil, fmt.Errorf("variant: not a playlist")
		}
	}
	// Identity encoder: every child URL is made absolute and left pointing at
	// nero's CDN. The player fetches segments from there directly.
	return rewritePlaylist(media, mediaURL, func(u string) string { return u }), nil
}

// bestVariant returns the highest-bandwidth rendition of a master playlist, or
// nil when the playlist is already a media playlist. nero ships one rung today.
func bestVariant(master []byte, base *url.URL) *url.URL {
	lines := strings.Split(string(master), "\n")
	var best *url.URL
	bestBW := -1
	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") || i+1 >= len(lines) {
			continue
		}
		uri := strings.TrimSpace(lines[i+1])
		if uri == "" || strings.HasPrefix(uri, "#") {
			continue
		}
		bw := 0
		if _, rest, ok := strings.Cut(line, "BANDWIDTH="); ok {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			bw, _ = strconv.Atoi(strings.TrimSpace(rest[:end]))
		}
		if bw > bestBW {
			best, bestBW = resolve(uri, base), bw
		}
	}
	return best
}

// neroGet fetches one hop DIRECT with the fixed browser baseline (the lock
// includes the User-Agent, so every hop must send the same one). animex's API
// additionally wants its key and its own Origin. Follows up to 3 redirects,
// re-checking each target.
func neroGet(r *http.Request, target *url.URL, animexAPI bool) ([]byte, *url.URL, error) {
	for hops := 0; ; hops++ {
		if (target.Scheme != "https" && target.Scheme != "http") || !isPublicHost(target.Hostname()) {
			return nil, nil, fmt.Errorf("forbidden target")
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range defaultHeaders {
			req.Header[k] = v
		}
		if animexAPI {
			req.Header.Set("x-api-key", animexAPIKey)
			req.Header.Set("Origin", animexOrigin)
			req.Header.Set("Referer", animexOrigin+"/")
			req.Header.Set("Accept", "application/json")
		}
		// httpClient.Do, not fetchUpstream: an egress exit would change the IP
		// between hops and break the lock.
		resp, err := httpClient.Do(req)
		recordUpstream(strings.ToLower(target.Hostname()), resp, err, false)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && hops < 3 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return nil, nil, fmt.Errorf("redirect without location")
			}
			target = resolve(loc, target)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
		resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("%s answered %d", target.Hostname(), resp.StatusCode)
		}
		if len(body) > maxManifestSize {
			return nil, nil, fmt.Errorf("%s: body too large", target.Hostname())
		}
		return body, target, nil
	}
}
