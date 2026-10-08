package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One request mints, fetches the master and the media playlist with a single
// User-Agent, and returns segments pointing straight at nero's CDN.
func TestNeroMintsAndServesDirectSegments(t *testing.T) {
	t.Setenv("ANIMEX_API_KEY", "") // Nero must work without an environment key.
	old := httpClient
	httpClient = newClient()
	t.Cleanup(func() {
		httpClient.Transport.(*http.Transport).CloseIdleConnections()
		httpClient = old
	})

	var mintUA string
	lockedTo := func(w http.ResponseWriter, r *http.Request) bool {
		if r.UserAgent() != mintUA {
			http.Error(w, "locked", http.StatusForbidden)
			return false
		}
		return true
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "pp.animex.one" && r.URL.Path == "/rest/api/sources":
			if r.Header.Get("x-api-key") != animexAPIKey || r.Header.Get("Origin") != animexOrigin {
				http.Error(w, "no key", http.StatusUnauthorized)
				return
			}
			mintUA = r.UserAgent()
			io.WriteString(w, `{"sources":[{"url":"http://vault.test/i/a/s/deeznuts.txt?token=m"}]}`)
		case r.Host == "vault.test" && r.URL.Path == "/i/a/s/deeznuts.txt":
			if lockedTo(w, r) {
				io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000\n1080p/nut-1.txt?token=v\n")
			}
		case r.Host == "vault.test" && r.URL.Path == "/i/a/s/1080p/nut-1.txt":
			if lockedTo(w, r) {
				io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n/i/a/s/c/seg1.jpg\n#EXTINF:4,\n/i/a/s/c/seg2.jpg\n#EXT-X-ENDLIST\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	tr := httpClient.Transport.(*http.Transport)
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}

	proxy := httptest.NewServer(http.HandlerFunc(handleProxy))
	defer proxy.Close()
	mint := "http://pp.animex.one/rest/api/sources?id=x&epNum=1&type=sub&providerId=nero"
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(proxy.URL + "/m3u8/" + EncodePayload(mint, "", "nero"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("content-type %q", ct)
	}
	got := string(body)
	for _, want := range []string{"http://vault.test/i/a/s/c/seg1.jpg", "http://vault.test/i/a/s/c/seg2.jpg", "#EXT-X-ENDLIST"} {
		if !strings.Contains(got, want) {
			t.Fatalf("playlist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, proxy.URL) || strings.Contains(got, "token=") {
		t.Fatalf("segments must be direct and the locked URLs must not leak:\n%s", got)
	}
}

// Only the animex /sources URL is minted; other nero-tagged URLs (subtitles)
// take the normal path.
func TestIsNeroMintOnlyForSourcesHost(t *testing.T) {
	for _, tc := range []struct {
		raw, server string
		want        bool
	}{
		{"https://pp.animex.one/rest/api/sources?id=x", "nero", true},
		{"https://pp.animex.one/rest/api/sources?id=x", " NERO ", true},
		{"https://cdn.kryntal.top/en.vtt", "nero", false},
		{"https://pp.animex.one/rest/api/sources?id=x", "wave", false},
	} {
		u, _ := http.NewRequest("GET", tc.raw, nil)
		if got := isNeroMint(u.URL, tc.server); got != tc.want {
			t.Errorf("isNeroMint(%q, %q) = %v", tc.raw, tc.server, got)
		}
	}
}

func TestBestVariantPicksHighestBandwidth(t *testing.T) {
	base, _ := http.NewRequest("GET", "https://v.test/i/a/master.txt", nil)
	master := []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500000\nlow.txt\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=1920x1080\nhigh.txt\n")
	if got := bestVariant(master, base.URL); got == nil || got.String() != "https://v.test/i/a/high.txt" {
		t.Fatalf("got %v", got)
	}
	if bestVariant([]byte("#EXTM3U\n#EXTINF:4,\nseg.jpg\n"), base.URL) != nil {
		t.Fatal("a media playlist has no variant")
	}
}
