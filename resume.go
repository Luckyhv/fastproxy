package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// maxResumes bounds how many times one body is stitched back together. The
// upstream "unexpected EOF" rate is ~1 per few thousand segments; a host that
// cuts the same body three times in a row is not going to finish it.
const maxResumes = 2

// resumingBody turns an upstream that drops the connection mid-segment into a
// seamless stream: on a read error it asks for the missing tail with a Range
// request and carries on, so the viewer never sees the cut. Without it every
// drop aborted the viewer's response (see handler step 7) and hls.js had to
// re-fetch the whole segment from zero after a stall.
//
// It only resumes when the continuation is provably the same object: the 206
// must start exactly where we stopped, report the same total size, and carry
// the same ETag when the first response had one. Anything else surfaces the
// original error — a spliced body from a different file would be worse than a
// retry.
type resumingBody struct {
	body    io.ReadCloser
	fetch   func(rangeHeader string) (*http.Response, error)
	ctx     context.Context
	next    int64 // absolute offset of the next byte we owe the viewer
	end     int64 // absolute offset of the last byte (inclusive)
	total   int64 // object size, -1 if unknown
	etag    string
	resumes int
}

// newResumingBody wraps resp.Body when resp describes a byte range we can
// re-request; otherwise it returns the body untouched. consumed is how many
// body bytes were already read (the manifest sniff).
func newResumingBody(resp *http.Response, consumed int64, ctx context.Context, fetch func(string) (*http.Response, error)) io.ReadCloser {
	start, end, total, ok := responseSpan(resp)
	if !ok {
		return resp.Body
	}
	return &resumingBody{
		body:  resp.Body,
		fetch: fetch,
		ctx:   ctx,
		next:  start + consumed,
		end:   end,
		total: total,
		etag:  strongETag(resp.Header.Get("Etag")),
	}
}

// responseSpan reports the absolute byte span a 200/206 response covers.
func responseSpan(resp *http.Response) (start, end, total int64, ok bool) {
	switch resp.StatusCode {
	case http.StatusOK:
		// Range on a 200 needs the server to support it; a length is required
		// so we know what "the rest" is.
		if resp.ContentLength <= 0 || !strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
			return 0, 0, 0, false
		}
		return 0, resp.ContentLength - 1, resp.ContentLength, true
	case http.StatusPartialContent:
		return parseContentRange(resp.Header.Get("Content-Range"))
	}
	return 0, 0, 0, false
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
		if err == io.EOF && b.next > b.end {
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
	if b.resumes >= maxResumes || b.ctx.Err() != nil || b.next > b.end {
		return false
	}
	b.resumes++
	resp, err := b.fetch("bytes=" + strconv.FormatInt(b.next, 10) + "-" + strconv.FormatInt(b.end, 10))
	if err != nil {
		return false
	}
	start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	sameObject := resp.StatusCode == http.StatusPartialContent && ok &&
		start == b.next && end == b.end &&
		(b.total < 0 || total == b.total) &&
		(b.etag == "" || resp.Header.Get("Etag") == b.etag)
	if !sameObject {
		resp.Body.Close()
		return false
	}
	b.body.Close()
	b.body = resp.Body
	log.Printf("stream resumed at byte %d (attempt %d)", start, b.resumes)
	return true
}

func (b *resumingBody) Close() error { return b.body.Close() }
