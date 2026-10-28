package providers

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
)

// webPVP8X builds a minimal VP8X header with a valid VP8 chunk, so the
// dimension parser runs on a file that also passes the container check. The
// canvas fields store width-1 and height-1 as 24-bit little-endian values.
func webPVP8X(width, height int) []byte {
	vp8 := []byte{0x30, 0x01, 0x00, 0x9d, 0x01, 0x2a, 0x01, 0x00, 0x01, 0x00}
	var body bytes.Buffer
	body.WriteString("WEBP")
	body.WriteString("VP8X")
	binary.Write(&body, binary.LittleEndian, uint32(10))
	body.Write([]byte{0x00, 0x00, 0x00, 0x00})
	putUint24(&body, width-1)
	putUint24(&body, height-1)
	body.WriteString("VP8 ")
	binary.Write(&body, binary.LittleEndian, uint32(len(vp8)))
	body.Write(vp8)
	out := new(bytes.Buffer)
	out.WriteString("RIFF")
	binary.Write(out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

func putUint24(buf *bytes.Buffer, value int) {
	buf.Write([]byte{byte(value), byte(value >> 8), byte(value >> 16)})
}

// TestWebPVP8XDimensionsCarryAcrossByteBoundary guards the carry case where the
// stored canvas width ends in 0xff. Adding one to a single byte before OR-ing
// the higher bytes gave the wrong size there.
func TestWebPVP8XDimensionsCarryAcrossByteBoundary(t *testing.T) {
	for _, tc := range []struct{ width, height int }{
		{width: 255, height: 256},
		{width: 256, height: 255},
		{width: 511, height: 512},
		{width: 65536, height: 1},
	} {
		body := webPVP8X(tc.width, tc.height)
		result, err := validateUpscaledImage(body)
		if err != nil {
			t.Fatalf("%dx%d: validate: %v", tc.width, tc.height, err)
		}
		if result.Width != tc.width || result.Height != tc.height {
			t.Fatalf("%dx%d decoded as %dx%d", tc.width, tc.height, result.Width, result.Height)
		}
	}
}

// TestWebPHeaderOnlyIsRejected ensures a RIFF/WEBP header with no image chunk is
// not reported as an image.
func TestWebPHeaderOnlyIsRejected(t *testing.T) {
	headerOnly := []byte("RIFF\x1a\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	if _, err := validateUpscaledImage(headerOnly); err == nil {
		t.Fatal("header-only WebP was accepted")
	}
	// A RIFF size larger than the body is a truncated container.
	truncated := webPVP8X(4, 4)
	truncated = append([]byte(nil), truncated[:len(truncated)-4]...)
	if _, err := validateUpscaledImage(truncated); err == nil {
		t.Fatal("truncated WebP container was accepted")
	}
}

// TestSizeSuffixRequiresDigits keeps a bare =s or =w from counting as a size
// directive. Padding alone is not a size either.
func TestSizeSuffixRequiresDigits(t *testing.T) {
	for _, path := range []string{"/gg/x=s", "/gg/x=w", "/gg/x=sabc"} {
		if hasSizeSuffix(path) {
			t.Fatalf("%s must not count as a size suffix", path)
		}
	}
	for _, path := range []string{"/gg/x=s0", "/gg/x=w2048"} {
		if !hasSizeSuffix(path) {
			t.Fatalf("%s must count as a size suffix", path)
		}
	}
}

// TestUpscaleRPCRedirectFallsBackWithoutSecondRequest proves a 3xx from the RPC
// ends the call as a fallback. The token, cookies and form must not be sent to
// the redirect target.
func TestUpscaleRPCRedirectFallsBackWithoutSecondRequest(t *testing.T) {
	requests := 0
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = generationTestTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		header := make(http.Header)
		header.Set("Location", "https://evil.example/collect")
		if requests > 1 {
			t.Fatalf("redirect was followed to %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: header,
			Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})

	client := upscaleTestClient("session=test-cookie")
	if _, err := client.fetchUpscaledImage(context.Background(), testRef(), "a cat", "session=test-cookie"); err == nil {
		t.Fatal("redirected RPC must fall back")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly one RPC and no follow-up", requests)
	}
}
