package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// cutBody serves data but dies with a connection error after `cut` bytes, the
// way an upstream that drops mid-segment looks to net/http.
type cutBody struct {
	r    io.Reader
	left int
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= n
	return n, err
}
func (c *cutBody) Close() error { return nil }

const object = "0123456789abcdefghijklmnopqrstuvwxyz" // 36 bytes

func fullResp(cut int, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	if h.Get("Accept-Ranges") == "" {
		h.Set("Accept-Ranges", "bytes")
	}
	return &http.Response{StatusCode: 200, ContentLength: int64(len(object)), Header: h,
		Body: &cutBody{r: strings.NewReader(object), left: cut}}
}

// rangeServer answers Range requests for `object`, optionally lying about it.
func rangeServer(t *testing.T, calls *[]string, mutate func(*http.Response)) func(string) (*http.Response, error) {
	return func(rng string) (*http.Response, error) {
		*calls = append(*calls, rng)
		a, b := 0, len(object)-1
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err != nil {
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &a); err != nil {
				t.Fatalf("bad range %q", rng)
			}
		}
		resp := &http.Response{StatusCode: 206, Header: http.Header{
			"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", a, b, len(object))},
			"Etag":          {`"v1"`},
		}, Body: io.NopCloser(strings.NewReader(object[a : b+1]))}
		if mutate != nil {
			mutate(resp)
		}
		return resp, nil
	}
}

func TestResumeHealsMidBodyCut(t *testing.T) {
	var calls []string
	resp := fullResp(10, http.Header{"Etag": {`"v1"`}})
	body := newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, nil))
	got, err := io.ReadAll(body)
	if err != nil || string(got) != object {
		t.Fatalf("got %q, %v; want the whole object", got, err)
	}
	if len(calls) != 1 || calls[0] != "bytes=10-35" {
		t.Fatalf("range requests = %v, want [bytes=10-35]", calls)
	}
}

// The sniffed prefix was already consumed from the body, so the resume offset
// must count it, or the viewer gets those bytes twice.
func TestResumeCountsSniffedPrefix(t *testing.T) {
	var calls []string
	resp := fullResp(10, nil)
	io.ReadFull(resp.Body, make([]byte, 7)) // the #EXTM3U sniff
	got, err := io.ReadAll(newResumingBody(resp, 7, context.Background(), rangeServer(t, &calls, nil)))
	if err != nil || string(got) != object[7:] {
		t.Fatalf("got %q, %v; want %q", got, err, object[7:])
	}
	if calls[0] != "bytes=10-35" {
		t.Fatalf("resumed at %v, want bytes=10-35", calls)
	}
}

func TestResumeOfPartialContent(t *testing.T) {
	var calls []string
	resp := &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 5-20/36"}},
		Body: &cutBody{r: strings.NewReader(object[5:21]), left: 4}}
	got, err := io.ReadAll(newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, nil)))
	if err != nil || string(got) != object[5:21] {
		t.Fatalf("got %q, %v; want %q", got, err, object[5:21])
	}
	if calls[0] != "bytes=9-20" {
		t.Fatalf("resumed at %v, want bytes=9-20", calls)
	}
}

// Never splice a different object onto the viewer's bytes.
func TestResumeRefusesAnotherObject(t *testing.T) {
	cases := map[string]func(*http.Response){
		"etag changed":   func(r *http.Response) { r.Header.Set("Etag", `"v2"`) },
		"size changed":   func(r *http.Response) { r.Header.Set("Content-Range", "bytes 10-35/99") },
		"wrong offset":   func(r *http.Response) { r.Header.Set("Content-Range", "bytes 0-35/36") },
		"upstream error": func(r *http.Response) { r.StatusCode = 403 },
	}
	for name, mutate := range cases {
		var calls []string
		resp := fullResp(10, http.Header{"Etag": {`"v1"`}})
		got, err := io.ReadAll(newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, mutate)))
		if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != object[:10] {
			t.Errorf("%s: got %q, %v; want the cut surfaced after 10 bytes", name, got, err)
		}
	}
}

func TestResumeStopsWhenViewerLeft(t *testing.T) {
	var calls []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	io.ReadAll(newResumingBody(fullResp(10, nil), 0, ctx, rangeServer(t, &calls, nil)))
	if len(calls) != 0 {
		t.Fatalf("resumed for a viewer who left: %v", calls)
	}
}

func TestResumeIsBounded(t *testing.T) {
	var calls []string
	// Every continuation is cut again after 2 bytes.
	fetch := rangeServer(t, &calls, func(r *http.Response) { r.Body = &cutBody{r: r.Body, left: 2} })
	_, err := io.ReadAll(newResumingBody(fullResp(10, nil), 0, context.Background(), fetch))
	if err == nil || len(calls) != maxResumes {
		t.Fatalf("err=%v after %d resumes, want an error after %d", err, len(calls), maxResumes)
	}
}

// yuki's tiktokcdn answers 200 with a length but no Accept-Ranges, yet honours
// Range: a missing header must not stop us from asking.
func TestResumeWithoutAcceptRangesHeader(t *testing.T) {
	var calls []string
	resp := fullResp(10, http.Header{"Accept-Ranges": {"none"}})
	resp.Header.Del("Accept-Ranges")
	got, err := io.ReadAll(newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, nil)))
	if err != nil || string(got) != object {
		t.Fatalf("got %q, %v", got, err)
	}
}

// wave and koto ignore Range and send the whole object again, chunked, with a
// strong ETag: prove identity by the tag and skip what the viewer already has.
func TestResumeReplaysWhenRangeIgnored(t *testing.T) {
	var calls []string
	resp := &http.Response{StatusCode: 200, ContentLength: -1, Header: http.Header{"Etag": {`"v1"`}},
		Body: &cutBody{r: strings.NewReader(object), left: 10}}
	full := func(r *http.Response) {
		r.StatusCode, r.ContentLength = 200, -1
		r.Header.Del("Content-Range")
		r.Body = io.NopCloser(strings.NewReader(object))
	}
	got, err := io.ReadAll(newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, full)))
	if err != nil || string(got) != object {
		t.Fatalf("got %q, %v; want the whole object once", got, err)
	}
	if calls[0] != "bytes=10-" {
		t.Fatalf("asked %v, want an open-ended range", calls)
	}
}

// Without a strong ETag a replayed 200 proves nothing (beep's playeng), and a
// matching length alone is not identity either.
func TestReplayNeedsStrongETag(t *testing.T) {
	for name, etag := range map[string]string{"none": "", "weak": `W/"v1"`} {
		var calls []string
		h := http.Header{}
		if etag != "" {
			h.Set("Etag", etag)
		}
		resp := &http.Response{StatusCode: 200, ContentLength: int64(len(object)), Header: h,
			Body: &cutBody{r: strings.NewReader(object), left: 10}}
		full := func(r *http.Response) {
			r.StatusCode, r.ContentLength = 200, int64(len(object))
			r.Header = h.Clone()
			r.Body = io.NopCloser(strings.NewReader(object))
		}
		got, err := io.ReadAll(newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, full)))
		if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != object[:10] {
			t.Errorf("%s etag: got %q, %v; want the cut surfaced", name, got, err)
		}
	}
}

// A chunked body that ends cleanly is complete; only a broken read resumes.
func TestUnknownLengthCleanEOFIsEnd(t *testing.T) {
	var calls []string
	resp := &http.Response{StatusCode: 200, ContentLength: -1, Header: http.Header{"Etag": {`"v1"`}},
		Body: io.NopCloser(strings.NewReader(object))}
	got, err := io.ReadAll(newResumingBody(resp, 0, context.Background(), rangeServer(t, &calls, nil)))
	if err != nil || string(got) != object || len(calls) != 0 {
		t.Fatalf("got %q, %v, %d resumes; want a plain read", got, err, len(calls))
	}
}

// Error statuses are passed through untouched.
func TestNoWrapForErrorStatus(t *testing.T) {
	resp := &http.Response{StatusCode: 403, Header: http.Header{}, Body: http.NoBody}
	if body := newResumingBody(resp, 0, context.Background(), nil); body != resp.Body {
		t.Fatal("wrapped an error response")
	}
}
