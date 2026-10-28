package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestGenerateContentSingleAttemptSendsOneRequest pins the GEMINI_MAX_RETRIES=1
// contract: a failing generation is sent exactly once, and the default of
// three keeps retrying. The failure is an unusable reply, which the retry loop
// would otherwise resend.
func TestGenerateContentSingleAttemptSendsOneRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxRetries int
		want       int
	}{
		{name: "one attempt", maxRetries: 1, want: 1},
		{name: "default retries", maxRetries: 3, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{at: "test-token", cookieHeader: "session=test-cookie", cachedModels: generationTestModels(),
				language: "en", buildLabel: "test-build", sessionID: "google-session", log: zap.NewNop(),
				maxRetries: tc.maxRetries, singleAttempt: tc.maxRetries <= 1}

			requests := 0
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = generationTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme+"://"+r.URL.Host+r.URL.Path != EndpointGenerate {
					t.Fatalf("unexpected request to %s", r.URL.Path)
				}
				requests++
				// An empty conversation body makes the reply unusable on every attempt.
				body, _ := json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, "[]"}})
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
			})

			_, _ = client.GenerateContent(t.Context(), "hello", WithModel("gemini-flash"))
			if requests != tc.want {
				t.Fatalf("generation requests = %d, want %d", requests, tc.want)
			}
		})
	}
}
