package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// authenticatedBodyLimit covers four 20 MiB inline references after base64
// expansion (about 27 MiB each) with room for JSON framing.
const authenticatedBodyLimit = 128 << 20

// New creates a new Fiber app instance
func NewGeminiWebToAPI(log *zap.Logger, cfg *configs.Config) *fiber.App {
	// Authenticated NexAI deployments send inline reference images that can
	// exceed Fiber's 4 MiB default. The limit only rises when API_KEY is set:
	// the bearer gate then sits in front of the inference routes, which is the
	// private-service boundary this larger body depends on.
	bodyLimit := fiber.DefaultBodyLimit
	if cfg.Server.APIKey != "" {
		bodyLimit = authenticatedBodyLimit
	}
	app := fiber.New(fiber.Config{
		AppName:   "Gemini Web To API",
		BodyLimit: bodyLimit,
	})

	app.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With", "x-api-key", "anthropic-version"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowCredentials: false,
	}))

	app.Use(recover.New())

	if cfg.RateLimit.Enabled {
		app.Use(limiter.New(limiter.Config{
			Max:        cfg.RateLimit.MaxRequests,
			Expiration: time.Duration(cfg.RateLimit.WindowMs) * time.Millisecond,
		}))
	}

	// Inference route groups only. Health, docs and /admin keep their own rules.
	if key := cfg.Server.APIKey; key != "" {
		gate := requireAPIKey(key)
		for _, prefix := range []string{"/openai", "/gemini", "/claude"} {
			app.Use(prefix, gate)
		}
	}

	app.Get("/docs", ScalarUI)
	app.Get("/openapi.json", OpenAPISpec)

	app.Get("/health", HealthCheck)

	return app
}

// requireAPIKey accepts only `Authorization: Bearer <key>`. The comparison uses
// SHA-256 digests so the check does not leak the key length or prefix timing.
func requireAPIKey(key string) fiber.Handler {
	want := sha256.Sum256([]byte(key))
	return func(c fiber.Ctx) error {
		token, ok := strings.CutPrefix(c.Get(fiber.HeaderAuthorization), "Bearer ")
		if !ok {
			return unauthorized(c)
		}
		got := sha256.Sum256([]byte(strings.TrimSpace(token)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return unauthorized(c)
		}
		return c.Next()
	}
}

func unauthorized(c fiber.Ctx) error {
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
		"status":  fiber.StatusUnauthorized,
		"error":   "Unauthorized",
		"message": "missing or invalid API key",
	})
}

func HealthCheck(c fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"status":  "ok",
		"service": "gemini-web-to-api",
	})
}

// Register404Handler registers the 404 handler for unmatched routes
// This must be called AFTER all other routes are registered
func Register404Handler(app *fiber.App) {
	app.All("*", func(c fiber.Ctx) error {
		method := c.Method()
		path := c.Path()
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"status":  fiber.StatusNotFound,
			"error":   "Not Found",
			"message": fmt.Sprintf("Cannot %s %s", method, path),
		})
	})
}

// RegisterFiberLifecycle registers the Fiber app lifecycle hooks
func RegisterFiberLifecycle(lc fx.Lifecycle, app *fiber.App, cfg *configs.Config, log *zap.Logger) {
	port := cfg.Server.Port
	address := fmt.Sprintf(":%s", port)

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			Register404Handler(app)
			log.Info("Starting server", zap.String("address", address))
			// Start server in a goroutine to not block
			go func() {
				if err := app.Listen(address); err != nil {
					log.Error("Server error", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("Shutting down server")
			return app.ShutdownWithContext(ctx)
		},
	})
}
