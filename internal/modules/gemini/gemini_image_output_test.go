package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"gemini-web-to-api/internal/modules/providers"
)

const imageOutputRequest = `{"contents":[{"role":"user","parts":[{"text":"draw a cat"}]}],` +
	`"generationConfig":{"responseModalities":["IMAGE"]}}`

// imageReply returns one generated image. An empty encoded value stands for a
// download the provider could not complete.
func imageReply(encoded string, mime string) *providers.Response {
	return &providers.Response{
		Model: "gemini-served-model",
		Images: []providers.Image{{
			Generated: true,
			URL:       "https://lh3.googleusercontent.com/gg/signed-url-that-must-not-leak",
			B64JSON:   encoded,
			MimeType:  mime,
		}},
	}
}

func imageTestService(response *providers.Response) *GeminiService {
	return &GeminiService{generate: func(_ context.Context, _ string, _ ...providers.GenerateOption) (*providers.Response, error) {
		return response, nil
	}}
}

func TestGenerateContentReturnsGeneratedImageAsInlineBytes(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake"))
	req := decodeGenerateRequest(t, imageOutputRequest)
	resp, err := imageTestService(imageReply(payload, "image/png")).GenerateContent(context.Background(), "gemini-auto", req)
	if err != nil {
		t.Fatal(err)
	}
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].InlineData == nil {
		t.Fatalf("parts = %+v, want one inlineData part", parts)
	}
	inline := parts[0].InlineData
	if inline.MimeType != "image/png" || inline.Data != payload || inline.DisplayName != "" {
		t.Fatalf("inlineData = %+v, want image/png bytes with empty displayName", inline)
	}
	if resp.ModelVersion != "gemini-served-model" {
		t.Fatalf("modelVersion = %q, want the provider's served model", resp.ModelVersion)
	}
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "fileUri") || strings.Contains(string(raw), "googleusercontent") {
		t.Fatalf("image response leaked a Google URL: %s", raw)
	}
}

func TestGenerateContentFailsWhenGeneratedImageBytesMissing(t *testing.T) {
	req := decodeGenerateRequest(t, imageOutputRequest)
	_, err := imageTestService(imageReply("", "image/png")).GenerateContent(context.Background(), "gemini-auto", req)
	if err == nil || !strings.Contains(err.Error(), "generated image bytes were not returned") {
		t.Fatalf("err = %v, want a clear missing-bytes failure rather than a URL", err)
	}
}

func TestGenerateContentPlainChatKeepsURLOutput(t *testing.T) {
	req := decodeGenerateRequest(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	resp, err := imageTestService(imageReply("", "image/png")).GenerateContent(context.Background(), "gemini-auto", req)
	if err != nil {
		t.Fatal(err)
	}
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 1 || parts[0].FileData == nil || parts[0].InlineData != nil {
		t.Fatalf("plain chat image part = %+v, want the unchanged FileData URL output", parts)
	}
}
