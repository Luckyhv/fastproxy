package main

import (
	"net/url"
	"testing"
)

// wave hands out `/cdn/<hash>?t.m3u8` — the extension lives in the query, so a
// path-only check reads it as an opaque blob and never rewrites the playlist.
func TestIsM3U8URLSeesQueryExtension(t *testing.T) {
	cases := map[string]bool{
		"https://ru-cdn1.echovideo.to/cdn/092e3d?t.m3u8":      true,
		"https://hls.anidb.app/stream/abc/master.m3u8":        true,
		"https://vault-12.owocdn.top/stream/12/13/x/uwu.m3u8": true,
		"https://hls.anidb.app/stream/abc/file-1-f1-v1.xls":   false,
		"https://cdn.example.test/segment0.ts":                false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := isM3U8URL(u); got != want {
			t.Fatalf("isM3U8URL(%q) = %v, want %v", raw, got, want)
		}
	}
}
