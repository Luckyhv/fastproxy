package main

import (
	"net/url"
	"strings"
	"testing"
)

// With an identity encode, rewritePlaylist must reproduce the input byte for
// byte. This pins down line splitting — trailing newlines, blank lines, CRLF,
// and the empty body — which a hand-rolled scanner is easy to get subtly wrong.
func TestRewritePlaylistPreservesLineStructure(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.test/a/index.m3u8")
	identity := func(uri string) string { return uri }

	cases := map[string]string{
		"empty":              "",
		"single line":        "#EXTM3U",
		"trailing newline":   "#EXTM3U\n#EXT-X-ENDLIST\n",
		"no trailing":        "#EXTM3U\n#EXT-X-ENDLIST",
		"blank lines":        "#EXTM3U\n\n#EXT-X-ENDLIST\n",
		"consecutive blanks": "#EXTM3U\n\n\n\n",
		"just newlines":      "\n\n",
		"absolute url":       "#EXTM3U\nhttps://cdn.example.test/a/seg1.ts\n",
	}
	for name, in := range cases {
		if got := string(rewritePlaylist([]byte(in), base, identity)); got != in {
			t.Fatalf("%s: round trip changed the body\n got %q\nwant %q", name, got, in)
		}
	}
}

// CRLF input is normalised to LF (unchanged from the original behaviour), but
// the line count must still be right.
func TestRewritePlaylistHandlesCRLF(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.test/a/index.m3u8")
	identity := func(uri string) string { return uri }
	got := string(rewritePlaylist([]byte("#EXTM3U\r\nseg1.ts\r\n"), base, identity))
	want := "#EXTM3U\nhttps://cdn.example.test/a/seg1.ts\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Relative URIs still have to be resolved against the manifest's own URL — the
// absolute fast path must not swallow them.
func TestRewritePlaylistResolvesRelativeURIs(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.test/a/b/index.m3u8")
	identity := func(uri string) string { return uri }
	in := strings.Join([]string{
		"#EXTM3U",
		"seg1.ts",
		"../up.ts",
		"/root.ts",
		"//other.example/proto-relative.ts",
		"https://absolute.example/x.ts",
		`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`,
		"",
	}, "\n")
	want := strings.Join([]string{
		"#EXTM3U",
		"https://cdn.example.test/a/b/seg1.ts",
		"https://cdn.example.test/a/up.ts",
		"https://cdn.example.test/root.ts",
		"https://other.example/proto-relative.ts",
		"https://absolute.example/x.ts",
		`#EXT-X-KEY:METHOD=AES-128,URI="https://cdn.example.test/a/b/key.bin"`,
		"",
	}, "\n")
	if got := string(rewritePlaylist([]byte(in), base, identity)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The absolute fast path skips url.Parse, so make sure it only fires on real
// absolute http(s) URLs and never on something that merely starts similarly.
func TestIsAbsoluteHTTP(t *testing.T) {
	cases := map[string]bool{
		"https://a.test/x.ts": true,
		"http://a.test/x.ts":  true,
		"HTTPS://A.TEST/x.ts": false, // uppercase scheme: take the slow path, don't guess
		"httpx://a.test/x.ts": false,
		"//a.test/x.ts":       false,
		"/x.ts":               false,
		"x.ts":                false,
		"":                    false,
		"http":                false,
		"https:/a.test":       false,
	}
	for in, want := range cases {
		if got := isAbsoluteHTTP(in); got != want {
			t.Fatalf("isAbsoluteHTTP(%q) = %v, want %v", in, got, want)
		}
	}
}
