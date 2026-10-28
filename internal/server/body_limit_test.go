package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

// bodyLengthApp reports the received body length, so a test can tell an accepted
// body from a rejected one without echoing any content.
func bodyLengthApp(t *testing.T, apiKey string) *fiber.App {
	t.Helper()
	cfg := &configs.Config{}
	cfg.Server.APIKey = apiKey
	app := NewGeminiWebToAPI(zap.NewNop(), cfg)
	app.Post("/openai/v1/chat/completions", func(c fiber.Ctx) error {
		return c.SendString(strconv.Itoa(len(c.Body())))
	})
	return app
}

// postBody sends size bytes. Fiber's app.Test returns an error instead of a
// response when the body exceeds the limit, so that error is reported as
// rejected = true.
func postBody(t *testing.T, app *fiber.App, size int, authorization string) (status int, body string, rejected bool) {
	t.Helper()
	payload := bytes.Repeat([]byte("a"), size)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", io.NopCloser(bytes.NewReader(payload)))
	req.ContentLength = int64(size)
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		return 0, "", strings.Contains(err.Error(), "exceeds the given limit")
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), false
}

// TestAuthenticatedBodyLimitAcceptsInlineReferences proves a payload above
// Fiber's 4 MiB default reaches the handler once the request is authenticated.
func TestAuthenticatedBodyLimitAcceptsInlineReferences(t *testing.T) {
	app := bodyLengthApp(t, "secret-key")
	const size = 5 << 20
	status, body, rejected := postBody(t, app, size, "Bearer secret-key")
	if rejected || status != http.StatusOK || body != strconv.Itoa(size) {
		t.Fatalf("5 MiB authenticated body = %d %q rejected=%v, want 200 and the full length", status, body, rejected)
	}
}

// TestAuthenticatedBodyLimitRejectsOversizedPayload proves the cap still applies
// when the request is authenticated.
func TestAuthenticatedBodyLimitRejectsOversizedPayload(t *testing.T) {
	app := bodyLengthApp(t, "secret-key")
	_, _, rejected := postBody(t, app, authenticatedBodyLimit+1, "Bearer secret-key")
	if !rejected {
		t.Fatal("oversized authenticated body was accepted")
	}
}

// TestUnconfiguredBodyLimitKeepsFiberDefault keeps the 4 MiB default when no
// API key is set, so an open deployment does not get the larger limit.
func TestUnconfiguredBodyLimitKeepsFiberDefault(t *testing.T) {
	app := bodyLengthApp(t, "")
	_, _, rejected := postBody(t, app, fiber.DefaultBodyLimit+1, "")
	if !rejected {
		t.Fatal("open deployment accepted a body above the default limit")
	}
}
