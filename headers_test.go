package main

import (
	"net/http"
	"testing"
)

func TestResponseContentTypeFixesKiwiSegments(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/vnd.ms-excel")

	got := responseContentType(
		"hls.anidb.app",
		"/stream/id/file-1-f1-v1-a1.xls",
		h,
	)
	if got != "video/mp2t" {
		t.Fatalf("content type = %q, want video/mp2t", got)
	}
}

func TestResponseContentTypeLeavesNormalResponsesAlone(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "video/mp4")

	got := responseContentType("cdn.example", "/video.mp4", h)
	if got != "video/mp4" {
		t.Fatalf("content type = %q, want video/mp4", got)
	}
}

// wave serves MPEG-TS segments as image/jpeg. We already hold the first bytes
// from the manifest sniff, so correcting the type costs nothing.
func TestMpegTSContentTypeCorrectsMislabeledSegments(t *testing.T) {
	ts := []byte{0x47, 0x40, 0x11, 0x10}
	if got := mpegTSContentType(ts, "image/jpeg"); got != "video/mp2t" {
		t.Fatalf("mislabeled TS = %q, want video/mp2t", got)
	}
	if got := mpegTSContentType(ts, "application/octet-stream"); got != "video/mp2t" {
		t.Fatalf("octet-stream TS = %q, want video/mp2t", got)
	}
	// Never second-guess a type that is already video/audio.
	if got := mpegTSContentType(ts, "video/mp4"); got != "" {
		t.Fatalf("video/mp4 = %q, want no override", got)
	}
	// Not TS, and nothing sniffed, both mean "leave it alone".
	if got := mpegTSContentType([]byte("#EXTM3"), "image/jpeg"); got != "" {
		t.Fatalf("non-TS = %q, want no override", got)
	}
	if got := mpegTSContentType(nil, "image/jpeg"); got != "" {
		t.Fatalf("unsniffed = %q, want no override", got)
	}
}

// headersToForward is copied with direct map indexing, which only matches when
// the keys are in Go's canonical form. "ETag" (the spelling everyone reaches
// for) canonicalizes to "Etag" and would silently never be forwarded.
func TestHeadersToForwardAreCanonical(t *testing.T) {
	for _, k := range headersToForward {
		if got := http.CanonicalHeaderKey(k); got != k {
			t.Errorf("headersToForward has non-canonical key %q; use %q", k, got)
		}
	}
}

func TestMaskedContentType(t *testing.T) {
	if !maskSegmentType {
		t.Fatal("masking should be ON by default (MASK_SEGMENT_TYPE=0 disables)")
	}
	cases := []struct {
		name, ct, want string
		status         int
	}{
		{"ts segment masked", "video/mp2t", "image/jpeg", 200},
		{"range response masked", "video/mp4", "image/jpeg", 206},
		{"already an image", "image/jpeg", "image/jpeg", 200},
		{"manifest type untouched", "application/vnd.apple.mpegurl", "application/vnd.apple.mpegurl", 200},
		{"error body untouched", "video/mp2t", "video/mp2t", 403},
		{"html untouched", "text/html", "text/html", 200},
		{"empty untouched", "", "", 200},
	}
	for _, c := range cases {
		if got := maskedContentType(c.ct, c.status); got != c.want {
			t.Errorf("%s: maskedContentType(%q, %d) = %q, want %q", c.name, c.ct, c.status, got, c.want)
		}
	}
}
