package providers

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestLiveCountingTransportSeparatesTraffic checks the smoke's counting rule
// offline. Each wire send lands in exactly one bucket, and retries count once
// per send because the counter sits on the transport, not on the logical call.
func TestLiveCountingTransportSeparatesTraffic(t *testing.T) {
	base := generationTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	counter := &countingTransport{base: base}

	send := func(rawURL string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := counter.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	// Two generation sends model one retry; they must count as two wire sends.
	send(EndpointGenerate)
	send(EndpointGenerate)
	send(EndpointBatchExec + "?rpcids=" + geminiUpscaleRPC)
	send(EndpointBatchExec + "?rpcids=ZwBzgf")

	if got := counter.generate.Load(); got != 2 {
		t.Fatalf("generate = %d, want 2 wire sends", got)
	}
	if got := counter.upscaleRPC.Load(); got != 1 {
		t.Fatalf("upscale RPC = %d, want 1", got)
	}
	if got := counter.bootstrap.Load(); got != 1 {
		t.Fatalf("bootstrap = %d, want 1", got)
	}
}
