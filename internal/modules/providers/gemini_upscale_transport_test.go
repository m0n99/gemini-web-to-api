package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
)

const testUpscaleGateway = "https://lh3.googleusercontent.com/gen-image"

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// upscaleFrame builds a batchexecute body whose c8o8Fe record names rawURL.
func upscaleFrame(rawURL string) string {
	payload, _ := json.Marshal([]interface{}{rawURL})
	body, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", geminiUpscaleRPC, string(payload)}})
	return ")]}'\n\n" + string(body)
}

func upscaleTestClient(cookie string) *Client {
	return &Client{at: "test-token", cookieHeader: cookie, language: "en", buildLabel: "test-build",
		sessionID: "google-session", authUser: "2", generationID: "gen-7", log: zap.NewNop()}
}

// testRef matches what upscaleRefsByURL builds from a synthetic response.
func testRef() *upscaleRef {
	return &upscaleRef{CID: "c_abc", RID: "r_def", RCID: "rc_1", Token: "tok-a", ContentRef: testContentRef}
}

// upscaleCounts records each kind of request the transport saw.
type upscaleCounts struct {
	rpc, download, generate int
	rpcRequest              *http.Request
	rpcForm                 map[string]string
}

// stubUpscaleTransport answers the RPC with rpcBody and the download with the
// given bytes. A generation request fails the test case through its error.
func stubUpscaleTransport(t *testing.T, rpcBody string, rpcStatus int, download []byte, downloadType string, counts *upscaleCounts) {
	t.Helper()
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = generationTestTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Scheme+"://"+r.URL.Host+r.URL.Path == EndpointGenerate:
			counts.generate++
			return nil, errors.New("generation must not run during upscale")
		case r.Method == http.MethodPost && r.URL.Scheme+"://"+r.URL.Host+r.URL.Path == geminiAccountURL(EndpointBatchExec, "2"):
			counts.rpc++
			counts.rpcRequest = r
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			counts.rpcForm = map[string]string{"at": r.PostForm.Get("at"), "f.req": r.PostForm.Get("f.req")}
			return &http.Response{StatusCode: rpcStatus, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(rpcBody)), Request: r}, nil
		case r.Method == http.MethodGet:
			counts.download++
			header := make(http.Header)
			header.Set("Content-Type", downloadType)
			return &http.Response{StatusCode: http.StatusOK, Header: header,
				Body: io.NopCloser(bytes.NewReader(download)), Request: r}, nil
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		return nil, nil
	})
}

func TestFetchUpscaledImageUsesRPCOnceAndReturnsValidatedBytes(t *testing.T) {
	img := pngBytes(t, 8, 4)
	counts := &upscaleCounts{}
	stubUpscaleTransport(t, upscaleFrame(testUpscaleGateway), http.StatusOK, img, "image/png", counts)

	client := upscaleTestClient("session=test-cookie")
	got, err := client.fetchUpscaledImage(context.Background(), testRef(), "a cat", "session=test-cookie")
	if err != nil {
		t.Fatalf("fetchUpscaledImage: %v", err)
	}
	if !bytes.Equal(got.Bytes, img) || got.MimeType != "image/png" || got.Width != 8 || got.Height != 4 {
		t.Fatalf("result = mime %q %dx%d, bytes changed=%v", got.MimeType, got.Width, got.Height, !bytes.Equal(got.Bytes, img))
	}
	if counts.rpc != 1 || counts.download != 1 || counts.generate != 0 {
		t.Fatalf("requests rpc=%d download=%d generate=%d, want 1/1/0", counts.rpc, counts.download, counts.generate)
	}
	if counts.rpcRequest.URL.Query().Get("rpcids") != geminiUpscaleRPC || !strings.HasPrefix(counts.rpcRequest.URL.Path, "/u/2/") {
		t.Fatalf("RPC query = %s", counts.rpcRequest.URL.RawQuery)
	}
	if counts.rpcRequest.Header.Get("Cookie") != "session=test-cookie" || counts.rpcForm["at"] != "test-token" {
		t.Fatal("RPC must use the same session cookie and token")
	}
	if !strings.Contains(counts.rpcForm["f.req"], `"c8o8Fe"`) || !strings.Contains(counts.rpcForm["f.req"], "tok-a") {
		t.Fatalf("f.req missing RPC id or per-image token: %s", counts.rpcForm["f.req"])
	}
}

func TestFetchUpscaledImageFallsBackOnFailures(t *testing.T) {
	img := pngBytes(t, 2, 2)
	cases := []struct {
		name   string
		body   string
		status int
		dl     []byte
		dlType string
		ref    *upscaleRef
	}{
		{name: "RPC denied", body: "denied", status: http.StatusForbidden, dl: img, dlType: "image/png", ref: testRef()},
		{name: "malformed RPC", body: ")]}'\n[not json", status: http.StatusOK, dl: img, dlType: "image/png", ref: testRef()},
		{name: "untrusted URL", body: upscaleFrame("https://evil.example/x.png"), status: http.StatusOK, dl: img, dlType: "image/png", ref: testRef()},
		{name: "non-image download", body: upscaleFrame(testUpscaleGateway), status: http.StatusOK, dl: []byte("<html>"), dlType: "text/html", ref: testRef()},
		{name: "incomplete metadata", body: upscaleFrame(testUpscaleGateway), status: http.StatusOK, dl: img, dlType: "image/png", ref: &upscaleRef{CID: "c_abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counts := &upscaleCounts{}
			stubUpscaleTransport(t, tc.body, tc.status, tc.dl, tc.dlType, counts)
			client := upscaleTestClient("session=test-cookie")
			if _, err := client.fetchUpscaledImage(context.Background(), tc.ref, "a cat", "session=test-cookie"); err == nil {
				t.Fatal("expected fallback error")
			}
			if counts.generate != 0 {
				t.Fatalf("fallback must never regenerate; generate requests=%d", counts.generate)
			}
		})
	}
}

func TestFetchUpscaledImageSendsNoRPCWithoutCompleteMetadata(t *testing.T) {
	counts := &upscaleCounts{}
	stubUpscaleTransport(t, upscaleFrame(testUpscaleGateway), http.StatusOK, nil, "image/png", counts)
	client := upscaleTestClient("session=test-cookie")
	if _, err := client.fetchUpscaledImage(context.Background(), &upscaleRef{CID: "c_abc"}, "a cat", "session=test-cookie"); err == nil {
		t.Fatal("incomplete ref must fall back")
	}
	if counts.rpc != 0 {
		t.Fatalf("incomplete metadata must not send RPC; got %d", counts.rpc)
	}
}

func TestUpscaleRequestUsesTokenPaddingAndQueryExactly(t *testing.T) {
	signed := "https://contribution.usercontent.google.com/download?c=AB%3D%3D&sig=x%2By"
	got, err := upscaleImageURL(signed)
	if err != nil || got != signed {
		t.Fatalf("signed redirect rewritten: %q (err %v)", got, err)
	}
}
