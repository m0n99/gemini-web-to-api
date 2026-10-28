package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

// liveUpscalePrompt is a natural Vietnamese image request. It names no size,
// so the run reflects an ordinary user request.
const liveUpscalePrompt = "Vẽ một con mèo nhỏ đang ngồi bên cửa sổ, ánh nắng buổi sáng, phong cách tranh màu nước."

// liveStageTimeout bounds Init and generation separately. Each stage gets its
// own budget and the run never retries.
const liveStageTimeout = 50 * time.Second

// liveDiagnosticsRoot is relative to the fork root, where the test runs after
// t.Chdir. .gitignore and .dockerignore already exclude .cookies/, so live
// output never reaches Git or the image context.
const liveDiagnosticsRoot = ".cookies"

// countingTransport wraps http.DefaultTransport, which generation and the
// upscale RPC/download use. It counts each wire send, so retries are visible,
// and keeps bootstrap traffic in a separate counter.
type countingTransport struct {
	base       http.RoundTripper
	generate   atomic.Int32
	upscaleRPC atomic.Int32
	bootstrap  atomic.Int32
	lastError  atomic.Value
	lastStatus atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case strings.Contains(r.URL.Path, "StreamGenerate"):
		c.generate.Add(1)
	case r.URL.Query().Get("rpcids") == geminiUpscaleRPC:
		c.upscaleRPC.Add(1)
	default:
		c.bootstrap.Add(1)
	}
	response, err := c.base.RoundTrip(r)
	if err != nil {
		c.lastError.Store(fmt.Sprintf("%T", err))
	}
	if response != nil {
		c.lastStatus.Store(int32(response.StatusCode))
	}
	return response, err
}

// TestLiveUpscaleSmoke is an opt-in live check. It never runs in the normal
// suite: it requires GEMINI_LIVE_UPSCALE=1 and only then loads the fork-root
// .env. It makes at most one generation and writes a sanitized summary and the
// returned image into a unique run directory under .cookies/live-upscale/.
func TestLiveUpscaleSmoke(t *testing.T) {
	if os.Getenv("GEMINI_LIVE_UPSCALE") != "1" {
		t.Skip("live smoke test; set GEMINI_LIVE_UPSCALE=1 to run it")
	}
	// Moving to the fork root before loading .env and creating the client makes
	// the cookie cache and diagnostics land in the ignored root .cookies/.
	// t.Chdir restores the working directory when the test ends.
	t.Chdir("../../..")
	_ = godotenv.Load(".env")

	cfg, err := configs.New()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// Pin the retry and upscale settings so the run does not depend on .env.
	cfg.Gemini.MaxRetries = 1
	cfg.Gemini.ImageUpscale = true

	counter := &countingTransport{base: http.DefaultTransport}
	original := http.DefaultTransport
	http.DefaultTransport = counter
	t.Cleanup(func() { http.DefaultTransport = original })

	runDir := liveRunDirectory(t)
	summary := liveSummary{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	defer func() {
		summary.GenerateRequests = int(counter.generate.Load())
		summary.UpscaleRPCRequests = int(counter.upscaleRPC.Load())
		summary.BootstrapRequests = int(counter.bootstrap.Load())
		if kind, ok := counter.lastError.Load().(string); ok {
			summary.TransportErrorType = kind
		}
		summary.LastHTTPStatus = int(counter.lastStatus.Load())
		writeLiveSummary(t, runDir, summary)
	}()

	client := NewClient(cfg, zap.NewNop())
	t.Cleanup(func() { _ = client.Close() })

	initCtx, cancelInit := context.WithTimeout(context.Background(), liveStageTimeout)
	defer cancelInit()
	initStart := time.Now()
	if err := client.Init(initCtx); err != nil {
		summary.InitMillis = time.Since(initStart).Milliseconds()
		summary.Error = "init_error"
		switch {
		case initCtx.Err() != nil:
			summary.Error = "init_timeout"
		case strings.Contains(err.Error(), "failed to reach gemini app"):
			summary.Error = "init_network_error"
		case strings.Contains(err.Error(), "failed to discover Gemini models"):
			summary.Error = "init_model_discovery_error"
		case strings.Contains(err.Error(), "SNlM0e"):
			summary.Error = "init_missing_session_token"
		case strings.Contains(err.Error(), "GEMINI_COOKIES"):
			summary.Error = "init_cookies_rejected"
		}
		t.Fatalf("init failed: %s", summary.Error)
	}
	summary.InitMillis = time.Since(initStart).Milliseconds()
	if os.Getenv("GEMINI_LIVE_INIT_ONLY") == "1" {
		return
	}

	// Check the model against what this account discovered before generating.
	// A missing model fails here, so no request is sent under a guessed name.
	model, err := selectLiveModel(client.ListModels(), os.Getenv("GEMINI_LIVE_MODEL"))
	if err != nil {
		summary.Error = "model_unavailable"
		t.Fatalf("model selection failed: %s", summary.Error)
	}
	summary.SelectedModel = model

	genCtx, cancelGen := context.WithTimeout(context.Background(), liveStageTimeout)
	defer cancelGen()
	genStart := time.Now()
	response, err := client.GenerateContent(genCtx, liveUpscalePrompt,
		WithModel(model),
		WithGeneratedImageDownload(true),
	)
	summary.GenerateMillis = time.Since(genStart).Milliseconds()
	if counter.generate.Load() != 1 {
		summary.Error = "generate_count_mismatch"
		t.Fatalf("expected exactly one generation send, saw %d", counter.generate.Load())
	}
	if err != nil {
		summary.Error = "generation_error"
		t.Fatalf("generation failed: %s", summary.Error)
	}

	for _, img := range response.Images {
		if !img.Generated {
			continue
		}
		applied, metaW, metaH := img.upscaleApplied, img.metaWidth, img.metaHeight
		entry := liveImageSummary{
			MimeType:       img.MimeType,
			UpscaleApplied: applied,
			MetaWidth:      metaW,
			MetaHeight:     metaH,
		}
		if data, decodeErr := base64.StdEncoding.DecodeString(img.B64JSON); decodeErr == nil {
			if result, validErr := validateUpscaledImage(data); validErr == nil {
				entry.FinalWidth, entry.FinalHeight, entry.DecodeOK = result.Width, result.Height, true
			}
			writeLiveImage(t, runDir, img.MimeType, data)
		}
		summary.Images = append(summary.Images, entry)
	}
	summary.GeneratedImages = len(summary.Images)
	if summary.GeneratedImages == 0 {
		summary.Error = "no_generated_image"
		t.Fatal("the response contained no generated image")
	}
	// A plain fallback is a valid response, but it is not proof of native
	// upscale. Success also needs a verified larger image: an RPC and download
	// that return the same or smaller dimensions do not count.
	for i := range summary.Images {
		entry := &summary.Images[i]
		if !entry.UpscaleApplied {
			summary.Error = "upscale_not_applied"
			t.Fatal("native upscale was not applied; the image is the standard fallback")
		}
		entry.ResolutionCheck = resolutionCheck(entry.MetaWidth, entry.MetaHeight, entry.FinalWidth, entry.FinalHeight, entry.DecodeOK)
		if entry.ResolutionCheck != resolutionIncreased {
			summary.Error = entry.ResolutionCheck
			t.Fatalf("resolution check %s: original %dx%d, final %dx%d", entry.ResolutionCheck,
				entry.MetaWidth, entry.MetaHeight, entry.FinalWidth, entry.FinalHeight)
		}
	}
}

const (
	resolutionIncreased    = "resolution_increased"
	resolutionNotIncreased = "resolution_not_increased"
	resolutionUnverified   = "resolution_unverified"
)

// resolutionCheck decides whether the final image is verifiably larger than
// the dimensions Gemini reported. Both axes must grow. Without a positive
// original size, or without a decoded final image, the result is unverified,
// never an assumed enlargement.
func resolutionCheck(metaW, metaH, finalW, finalH int, decodeOK bool) string {
	if metaW <= 0 || metaH <= 0 || !decodeOK {
		return resolutionUnverified
	}
	if finalW > metaW && finalH > metaH {
		return resolutionIncreased
	}
	return resolutionNotIncreased
}

// selectLiveModel returns the requested model when the account discovered it,
// or the first discovered model when none is requested. A request that the
// account did not discover is an error, not a silent substitution.
func selectLiveModel(discovered []ModelInfo, requested string) (string, error) {
	if len(discovered) == 0 {
		return "", fmt.Errorf("no models discovered")
	}
	if requested == "" {
		return discovered[0].ID, nil
	}
	for _, model := range discovered {
		if model.ID == requested {
			return model.ID, nil
		}
	}
	return "", fmt.Errorf("requested model not discovered")
}

// liveSummary holds only sanitized fields. It never carries cookies, tokens,
// conversation or response identifiers, or raw protocol bodies. Model names are
// the account-discovered public identifiers.
type liveSummary struct {
	GeneratedAt        string             `json:"generated_at"`
	SelectedModel      string             `json:"selected_model,omitempty"`
	InitMillis         int64              `json:"init_ms"`
	GenerateMillis     int64              `json:"generate_ms"`
	GenerateRequests   int                `json:"generate_requests"`
	UpscaleRPCRequests int                `json:"upscale_rpc_requests"`
	BootstrapRequests  int                `json:"bootstrap_requests"`
	GeneratedImages    int                `json:"generated_images"`
	Images             []liveImageSummary `json:"images,omitempty"`
	Error              string             `json:"error,omitempty"`
	TransportErrorType string             `json:"transport_error_type,omitempty"`
	LastHTTPStatus     int                `json:"last_http_status,omitempty"`
}

type liveImageSummary struct {
	MimeType        string `json:"mime_type"`
	UpscaleApplied  bool   `json:"upscale_applied"`
	MetaWidth       int    `json:"metadata_width"`
	MetaHeight      int    `json:"metadata_height"`
	FinalWidth      int    `json:"final_width"`
	FinalHeight     int    `json:"final_height"`
	DecodeOK        bool   `json:"decode_ok"`
	ResolutionCheck string `json:"resolution_check,omitempty"`
}

// liveRunDirectory creates a unique directory per run so reruns never overwrite
// an earlier summary or image.
func liveRunDirectory(t *testing.T) string {
	t.Helper()
	base := filepath.Join(liveDiagnosticsRoot, "live-upscale")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("create diagnostics dir: %v", err)
	}
	dir, err := os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405")+"-")
	if err != nil {
		t.Fatalf("create run directory: %v", err)
	}
	return dir
}

func writeLiveSummary(t *testing.T, dir string, summary liveSummary) {
	t.Helper()
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatalf("encode summary: %v", err)
	}
	path := filepath.Join(dir, "summary.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	t.Logf("sanitized summary written to %s", path)
}

func writeLiveImage(t *testing.T, dir, mimeType string, data []byte) {
	t.Helper()
	name := map[string]string{"image/png": "image.png", "image/jpeg": "image.jpg", "image/webp": "image.webp"}[mimeType]
	if name == "" {
		name = "image.bin"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}
	t.Logf("generated image written to %s", path)
}
