package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

// gatedApp builds the real app with a route in each inference group and in the
// open groups, so the test exercises the same middleware order as production.
func gatedApp(t *testing.T, apiKey string) *fiber.App {
	t.Helper()
	cfg := &configs.Config{}
	cfg.Server.APIKey = apiKey
	app := NewGeminiWebToAPI(zap.NewNop(), cfg)
	ok := func(c fiber.Ctx) error { return c.SendString("ok") }
	app.Get("/openai/v1/models", ok)
	app.Get("/gemini/v1beta/models", ok)
	app.Post("/claude/v1/messages", ok)
	return app
}

func statusOf(t *testing.T, app *fiber.App, method, path, authorization string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestAPIKeyGatesInferenceGroupsOnly(t *testing.T) {
	app := gatedApp(t, "secret-key")
	routes := []struct{ method, path string }{
		{http.MethodGet, "/openai/v1/models"},
		{http.MethodGet, "/gemini/v1beta/models"},
		{http.MethodPost, "/claude/v1/messages"},
	}
	for _, route := range routes {
		if got := statusOf(t, app, route.method, route.path, ""); got != http.StatusUnauthorized {
			t.Fatalf("%s without key = %d, want 401", route.path, got)
		}
		if got := statusOf(t, app, route.method, route.path, "Bearer wrong-key"); got != http.StatusUnauthorized {
			t.Fatalf("%s with wrong key = %d, want 401", route.path, got)
		}
		if got := statusOf(t, app, route.method, route.path, "Basic secret-key"); got != http.StatusUnauthorized {
			t.Fatalf("%s with non-bearer scheme = %d, want 401", route.path, got)
		}
		if got := statusOf(t, app, route.method, route.path, "Bearer secret-key"); got != http.StatusOK {
			t.Fatalf("%s with correct key = %d, want 200", route.path, got)
		}
	}
	if got := statusOf(t, app, http.MethodGet, "/health", ""); got != http.StatusOK {
		t.Fatalf("/health must stay open, got %d", got)
	}
}

func TestAPIKeyUnsetLeavesInferenceOpen(t *testing.T) {
	app := gatedApp(t, "")
	if got := statusOf(t, app, http.MethodGet, "/openai/v1/models", ""); got != http.StatusOK {
		t.Fatalf("unset API_KEY must not gate inference, got %d", got)
	}
}
