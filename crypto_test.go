package main

import "testing"

func TestPayloadRoundTripsServerName(t *testing.T) {
	url, ref, sv, ok := DecodePayload(EncodePayload("https://cdn.test/a.m3u8", "https://player.test/", "uwu"))
	if !ok {
		t.Fatal("decode failed")
	}
	if url != "https://cdn.test/a.m3u8" || ref != "https://player.test/" || sv != "uwu" {
		t.Fatalf("round trip = %q, %q, %q", url, ref, sv)
	}
}

// Tokens minted by the API before it started sending a provider key have only
// two fields. They must keep working — the hostname table covers them.
func TestTwoFieldPayloadStillDecodes(t *testing.T) {
	legacy := EncodePayload("https://cdn.test/a.m3u8", "https://player.test/", "")
	url, ref, sv, ok := DecodePayload(legacy)
	if !ok || url != "https://cdn.test/a.m3u8" || ref != "https://player.test/" || sv != "" {
		t.Fatalf("legacy round trip = %q, %q, %q, ok=%v", url, ref, sv, ok)
	}
}

func TestEmptyRefererWithServerName(t *testing.T) {
	url, ref, sv, ok := DecodePayload(EncodePayload("https://cdn.test/a.m3u8", "", "wave"))
	if !ok || url != "https://cdn.test/a.m3u8" || ref != "" || sv != "wave" {
		t.Fatalf("round trip = %q, %q, %q, ok=%v", url, ref, sv, ok)
	}
}
