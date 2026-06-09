package main

import (
	"bytes"
	"net/url"
	"strings"
)

// isM3U8ContentType / isM3U8Path: two independent signals that a response is an
// HLS manifest. We check both because some hosts serve playlists with a generic
// content-type, and some serve them from extension-less URLs.
func isM3U8ContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "mpegurl") || strings.Contains(ct, "vnd.apple.mpegurl")
}

func isM3U8Path(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u")
}

// resolve turns a (possibly relative) URI from inside a manifest into an
// absolute URL, using the manifest's own URL as the base. ResolveReference does
// the right thing for both: if ref is already absolute it's returned as-is;
// if it's relative ("seg1.ts", "../audio/x.m3u8") it's joined onto base.
func resolve(ref string, base *url.URL) *url.URL {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return base
	}
	return base.ResolveReference(u)
}

// rewritePlaylist walks an HLS manifest line by line and rewrites every child
// URL so the player fetches it back through us. `encode` maps an absolute
// upstream URL to a local proxy path (with the referer baked in).
func rewritePlaylist(body []byte, base *url.URL, encode func(*url.URL) string) []byte {
	lines := bytes.Split(body, []byte("\n"))
	var out bytes.Buffer
	out.Grow(len(body) * 2) // rewritten URLs are longer; pre-grow to avoid reallocs

	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n') // restore the newline Split removed
		}
		s := string(bytes.TrimRight(line, "\r")) // tolerate Windows CRLF endings

		switch {
		case len(s) == 0:
			// blank line — keep as-is
		case s[0] == '#':
			// a tag line. Most pass through untouched; a few embed a URI="...".
			out.WriteString(rewriteTagLine(s, base, encode))
		default:
			// a bare URL line: a segment (.ts/.m4s) or a variant playlist (.m3u8)
			out.WriteString(encode(resolve(s, base)))
		}
	}
	return out.Bytes()
}

// rewriteTagLine rewrites every URI="..." value inside a single #EXT tag. Tags
// like #EXT-X-KEY (decryption key), #EXT-X-MAP (init segment), #EXT-X-MEDIA
// (alternate audio/subtitle renditions) and #EXT-X-I-FRAME-STREAM-INF all point
// at child resources that must also go through us.
func rewriteTagLine(line string, base *url.URL, encode func(*url.URL) string) string {
	const marker = `URI="`
	if !strings.Contains(line, marker) {
		return line // fast path: no URI to rewrite
	}

	var b strings.Builder
	i := 0
	for {
		idx := strings.Index(line[i:], marker)
		if idx == -1 {
			b.WriteString(line[i:]) // no more URIs; flush the tail
			break
		}
		start := i + idx + len(marker) // first char of the quoted value
		end := strings.IndexByte(line[start:], '"')
		if end == -1 {
			b.WriteString(line[i:]) // malformed (no closing quote); leave as-is
			break
		}
		end += start

		b.WriteString(line[i:start])                       // everything up to & incl. URI="
		b.WriteString(encode(resolve(line[start:end], base))) // rewritten value
		b.WriteByte('"')                                   // closing quote
		i = end + 1                                         // continue after it
	}
	return b.String()
}
