package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// maxResumes bounds how many times one body is stitched back together. A host
// that cuts the same body three times in a row is not going to finish it.
const maxResumes = 2

// resumingBody turns an upstream that drops the connection mid-segment into a
// seamless stream: on a broken read it re-requests the rest and carries on, so
// the viewer never sees the cut. Without it every drop aborted the viewer's
// response (see handler step 7) and hls.js re-fetched the segment after a stall.
//
// Measured Sept 2026, the segment CDNs split three ways:
//   - honour Range (megg's vidcache, yuki's tiktokcdn — which never advertises
//     Accept-Ranges but answers 206 anyway): ask for exactly the missing tail.
//   - ignore Range but send a strong ETag (wave, koto): re-fetch the object,
//     prove it is the same one by its ETag, skip what the viewer already has.
//   - neither (beep's playeng: no length, no ETag): nothing proves the replay
//     is the same bytes, so the cut is surfaced as before.
//
// The rule throughout: splice only onto a response that PROVES it is the same
// object at the same offset. A body spliced from a different file is worse than
// a retry.
type resumingBody struct {
	body    io.ReadCloser
	fetch   func(rangeHeader string) (*http.Response, error)
	ctx     context.Context
	next    int64 // absolute offset of the next byte we owe the viewer
	end     int64 // absolute offset of the last byte (inclusive); -1 = unknown
	total   int64 // object size; -1 = unknown
	etag    string
	resumes int
}

// newResumingBody wraps a 200/206 body so a mid-body cut can be healed;
// anything else is returned untouched. consumed is how many body bytes were
// already read (the manifest sniff).
func newResumingBody(resp *http.Response, consumed int64, ctx context.Context, fetch func(string) (*http.Response, error)) io.ReadCloser {
	b := &resumingBody{body: resp.Body, fetch: fetch, ctx: ctx, end: -1, total: -1,
		etag: strongETag(resp.Header.Get("Etag"))}
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.ContentLength > 0 {
			b.end, b.total = resp.ContentLength-1, resp.ContentLength
		}
	case http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			return resp.Body
		}
		b.next, b.end, b.total = start, end, total
	default:
		return resp.Body
	}
	b.next += consumed
	return b
}

// parseContentRange parses "bytes start-end/total" (total may be "*").
func parseContentRange(v string) (start, end, total int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	span, size, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}
	a, b, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, 0, false
	}
	total = -1
	if size != "*" {
		t, err := strconv.ParseInt(size, 10, 64)
		if err != nil || t <= end {
			return 0, 0, 0, false
		}
		total = t
	}
	return start, end, total, true
}

// strongETag returns the tag only when it is a strong validator; a weak one
// ("W/...") does not promise byte-identical content, so we cannot splice on it.
func strongETag(tag string) string {
	if strings.HasPrefix(tag, "W/") {
		return ""
	}
	return tag
}

func (b *resumingBody) Read(p []byte) (int, error) {
	for {
		n, err := b.body.Read(p)
		b.next += int64(n)
		if err == nil {
			return n, nil
		}
		// A clean EOF is the end when we either reached the promised length or
		// never knew one (a chunked body signals truncation as an error, not EOF).
		if err == io.EOF && (b.end < 0 || b.next > b.end) {
			return n, io.EOF
		}
		// Short body or a broken read. Hand over what we got first; the
		// resume happens on the next call with an empty buffer.
		if n > 0 {
			return n, nil
		}
		if !b.resume() {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF // fewer bytes than promised
			}
			return 0, err
		}
	}
}

func (b *resumingBody) resume() bool {
	if b.resumes >= maxResumes || b.ctx.Err() != nil || (b.end >= 0 && b.next > b.end) {
		return false
	}
	// With neither a known length (to check a 206's Content-Range against) nor
	// a strong ETag (to check a replay), no answer could be proven the same
	// object — so don't spend an upstream request finding that out.
	if b.end < 0 && b.etag == "" {
		return false
	}
	b.resumes++
	rng := "bytes=" + strconv.FormatInt(b.next, 10) + "-"
	if b.end >= 0 {
		rng += strconv.FormatInt(b.end, 10)
	}
	resp, err := b.fetch(rng)
	if err != nil {
		return false
	}
	body, how, ok := b.continuation(resp)
	if !ok {
		resp.Body.Close()
		return false
	}
	b.body.Close()
	b.body = body
	log.Printf("stream resumed at byte %d via %s (attempt %d)", b.next, how, b.resumes)
	return true
}

// continuation checks that resp carries the same object and positions its body
// at b.next.
func (b *resumingBody) continuation(resp *http.Response) (io.ReadCloser, string, bool) {
	if b.etag != "" && resp.Header.Get("Etag") != b.etag {
		return nil, "", false
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != b.next || (b.end >= 0 && end != b.end) || (b.total >= 0 && total != b.total) {
			return nil, "", false
		}
		return resp.Body, "range", true
	case http.StatusOK:
		// Range ignored: the whole object again. Only a strong ETag match proves
		// it is byte-identical (a matching length alone does not).
		if b.etag == "" || (b.total >= 0 && resp.ContentLength >= 0 && resp.ContentLength != b.total) {
			return nil, "", false
		}
		if _, err := io.CopyN(io.Discard, resp.Body, b.next); err != nil {
			return nil, "", false
		}
		return resp.Body, "replay", true
	}
	return nil, "", false
}

func (b *resumingBody) Close() error { return b.body.Close() }
