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

// hasSuffixFold is strings.HasSuffix, case-insensitively, without the ToLower
// allocation. It runs once per playlist line in the rewrite loop, so the
// throwaway lowercase copy it replaces was real garbage on every manifest.
func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

func isM3U8Path(p string) bool {
	return hasSuffixFold(p, ".m3u8") || hasSuffixFold(p, ".m3u")
}

// isM3U8URL also looks at the query string. Some hosts park the extension there
// instead of on the path — wave hands out `/cdn/<hash>?t.m3u8`, where the path
// alone looks like an opaque blob.
func isM3U8URL(u *url.URL) bool {
	return isM3U8Path(u.Path) || isM3U8Path(u.RawQuery)
}

// isM3U8Ref is isM3U8URL on a RAW uri string, for the rewrite loop. It answers
// the same question without url.Parse — which the loop was paying for on every
// single line of every playlist, purely to pick between two path prefixes.
// Splitting off the fragment and query by hand is exact here: both extensions
// we look for are suffixes, so where the string ends is all that matters.
func isM3U8Ref(ref string) bool {
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.IndexByte(ref, '?'); i >= 0 {
		return isM3U8Path(ref[:i]) || isM3U8Path(ref[i+1:])
	}
	return isM3U8Path(ref)
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

// isAbsoluteHTTP reports whether a playlist URI is already an absolute http(s)
// URL, without parsing it.
func isAbsoluteHTTP(ref string) bool {
	if len(ref) >= 7 && (ref[4] == ':' || ref[5] == ':') {
		return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
	}
	return false
}

// absoluteURI is resolve() specialised for the rewrite loop, where it runs once
// per playlist line. Most providers emit absolute URLs, and for those a
// url.Parse + ResolveReference + String() round trip is pure waste — it costs
// five allocations to hand back (very nearly) the string we started with. Take
// the raw string instead; handleProxy re-parses and revalidates it on the way
// back in anyway, so nothing skips a check.
func absoluteURI(ref string, base *url.URL) string {
	ref = strings.TrimSpace(ref)
	if isAbsoluteHTTP(ref) {
		return ref
	}
	return resolve(ref, base).String()
}

// perLineOverhead is what a rewritten line costs on top of its source text: our
// origin and route prefix, plus base64 (4/3) of the absolute URL and referer the
// token carries. Deliberately generous — over-reserving a manifest by a few tens
// of KB is cheaper than one realloc-and-copy of the whole buffer.
const perLineOverhead = 320

// rewritePlaylist walks an HLS manifest line by line and rewrites every child
// URL so the player fetches it back through us. `encode` maps an upstream URI
// (already made absolute) to a local proxy path, with the referer and server
// baked in.
//
// Scans the body in place rather than bytes.Split-ing it: a 1200-segment
// playlist is ~2400 lines, and we would otherwise allocate a slice header for
// every one of them before writing a single byte.
func rewritePlaylist(body []byte, base *url.URL, encode func(string) string) []byte {
	var out bytes.Buffer
	// A rewritten line is base64(absolute url + referer + server) behind our own
	// origin, so its length tracks the ENCODED payload, not the source line — a
	// playlist of short relative names ("seg-0001.ts") can grow 20x. Size from the
	// real per-line cost instead of a flat multiple of the body, so a big playlist
	// doesn't re-grow (and re-copy) a few hundred KB several times over.
	out.Grow(len(body) + bytes.Count(body, []byte("\n"))*perLineOverhead)

	// Mirrors bytes.Split exactly: a body ending in "\n" has a final empty line,
	// so the trailing newline survives the round trip. Driving the loop off
	// len(body) instead would silently eat it.
	for i := 0; ; i++ {
		if i > 0 {
			out.WriteByte('\n') // restore the newline the split removed
		}
		var line []byte
		nl := bytes.IndexByte(body, '\n')
		if nl >= 0 {
			line, body = body[:nl], body[nl+1:]
		} else {
			line, body = body, nil
		}
		line = bytes.TrimRight(line, "\r") // tolerate Windows CRLF endings

		switch {
		case len(line) == 0:
			// blank line — keep as-is
		case line[0] == '#':
			// a tag line. Most pass through untouched; a few embed a URI="...".
			out.WriteString(rewriteTagLine(string(line), base, encode))
		default:
			// a bare URL line: a segment (.ts/.m4s) or a variant playlist (.m3u8)
			out.WriteString(encode(absoluteURI(string(line), base)))
		}
		if nl < 0 {
			break // that was the last line
		}
	}
	return out.Bytes()
}

// rewriteTagLine rewrites every URI="..." value inside a single #EXT tag. Tags
// like #EXT-X-KEY (decryption key), #EXT-X-MAP (init segment), #EXT-X-MEDIA
// (alternate audio/subtitle renditions) and #EXT-X-I-FRAME-STREAM-INF all point
// at child resources that must also go through us.
func rewriteTagLine(line string, base *url.URL, encode func(string) string) string {
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

		b.WriteString(line[i:start])                              // everything up to & incl. URI="
		b.WriteString(encode(absoluteURI(line[start:end], base))) // rewritten value
		b.WriteByte('"')                                          // closing quote
		i = end + 1                                               // continue after it
	}
	return b.String()
}
