package providers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

const testContentRef = "http://googleusercontent.com/image_generation_content/7"

// syntheticCandidate builds one candidate with one generated entry, using the
// same index layout as the parser: candidate[12][7][0][entry].
func syntheticCandidate(token, imageURL string) []interface{} {
	entry := []interface{}{
		[]interface{}{nil, nil, nil, []interface{}{nil, nil, nil, imageURL, nil, token}},
		[]interface{}{testContentRef},
	}
	media := []interface{}{nil, nil, nil, nil, nil, nil, nil, []interface{}{[]interface{}{entry}}}
	return []interface{}{"rc_1", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, media}
}

func TestUpscaleRefBoundToItsOwnCandidateAndPayload(t *testing.T) {
	candidate := syntheticCandidate("tok-a", "https://lh3.googleusercontent.com/a.png")
	refs := upscaleRefsByURL(candidate, "c_123", "r_456")
	ref := refs["https://lh3.googleusercontent.com/a.png"]
	if ref == nil {
		t.Fatalf("expected a complete ref, got nil in %#v", refs)
	}
	want := upscaleRef{CID: "c_123", RID: "r_456", RCID: "rc_1", Token: "tok-a", ContentRef: testContentRef}
	if *ref != want {
		t.Fatalf("ref = %+v, want %+v", *ref, want)
	}
}

func TestUpscaleRefMissingMetadataFallsBack(t *testing.T) {
	// No payload pair means no RID, so the ref must be unusable.
	candidate := syntheticCandidate("tok-a", "https://lh3.googleusercontent.com/a.png")
	refs := upscaleRefsByURL(candidate, "c_123", "")
	if ref := refs["https://lh3.googleusercontent.com/a.png"]; ref.complete() {
		t.Fatalf("ref without RID must not be complete: %+v", ref)
	}
}

func TestUpscaleRefMIMETypeInTokenSlotIsNotAToken(t *testing.T) {
	// Some responses put the MIME type where the per-image token would be.
	candidate := syntheticCandidate("image/png", "https://lh3.googleusercontent.com/a.png")
	refs := upscaleRefsByURL(candidate, "c_123", "r_456")
	if ref := refs["https://lh3.googleusercontent.com/a.png"]; ref.complete() {
		t.Fatalf("MIME in the token slot must not produce a usable ref: %+v", ref)
	}
}

func TestUpscaleRefConflictingDuplicatesFallBack(t *testing.T) {
	first := syntheticCandidate("tok-a", "https://lh3.googleusercontent.com/a.png")
	second := syntheticCandidate("tok-b", "https://lh3.googleusercontent.com/a.png")
	merged := append([]interface{}{}, first...)
	// Attach the second candidate's entry to the first candidate's media list.
	mediaFirst := first[12].([]interface{})
	mediaSecond := second[12].([]interface{})
	entries := mediaFirst[7].([]interface{})[0].([]interface{})
	entries = append(entries, mediaSecond[7].([]interface{})[0].([]interface{})...)
	mediaFirst[7] = []interface{}{entries}
	merged[12] = mediaFirst

	refs := upscaleRefsByURL(merged, "c_123", "r_456")
	if ref, seen := refs["https://lh3.googleusercontent.com/a.png"]; !seen || ref != nil {
		t.Fatalf("conflicting duplicate refs must map to nil, got %+v (seen=%v)", ref, seen)
	}
}

func TestResponsePairFromPayload(t *testing.T) {
	cid, rid := responsePairFromPayload([]interface{}{nil, []interface{}{"c_1", "r_2"}})
	if cid != "c_1" || rid != "r_2" {
		t.Fatalf("pair = (%q, %q), want (c_1, r_2)", cid, rid)
	}
	cid, rid = responsePairFromPayload([]interface{}{nil, "c_only"})
	if cid != "c_only" || rid != "" {
		t.Fatalf("bare pair = (%q, %q), want (c_only, \"\")", cid, rid)
	}
}

func TestUpscaleRequestPayloadSchema(t *testing.T) {
	ref := &upscaleRef{CID: "c_1", RID: "r_2", RCID: "rc_3", Token: "tok", ContentRef: testContentRef}
	raw, err := json.Marshal(upscaleRequestPayload(ref, "a cat", "gen-9"))
	if err != nil {
		t.Fatal(err)
	}
	var got []interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	inner := got[0].([]interface{})
	if inner[1].([]interface{})[0] != testContentRef || inner[3].([]interface{})[1] != "a cat" {
		t.Fatalf("content ref or prompt in wrong slot: %s", raw)
	}
	pair := got[1].([]interface{})
	if pair[0] != "r_2" || pair[1] != "rc_3" || pair[2] != "c_1" || pair[4] != "gen-9" {
		t.Fatalf("CID/RID/RCID slots wrong: %s", raw)
	}
	if got[2] != float64(1) || got[3] != float64(0) || got[4] != float64(1) {
		t.Fatalf("trailing scalars wrong: %s", raw)
	}
}

func TestParseUpscaleURLIgnoresOtherRPCs(t *testing.T) {
	other := `[["wrb.fr","hNvQHb","[\"https://evil.example/x.png\"]"]]`
	match := `[["wrb.fr","c8o8Fe","[\"https://lh3.googleusercontent.com/up=s0\"]"]]`
	body := ")]}'\n\n" + other + "\n" + match
	got, err := parseUpscaleURL([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://lh3.googleusercontent.com/up=s0" {
		t.Fatalf("url = %q", got)
	}
	if _, err := parseUpscaleURL([]byte(other)); err == nil {
		t.Fatal("body without a c8o8Fe frame must fall back")
	}
}

func TestUpscaleImageURLRules(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"gateway gets s0", "https://lh3.googleusercontent.com/abc", "https://lh3.googleusercontent.com/abc=s0"},
		{"gateway keeps existing size", "https://lh3.googleusercontent.com/abc=s512", "https://lh3.googleusercontent.com/abc=s512"},
		{"padding stays", "https://lh3.googleusercontent.com/abc==", "https://lh3.googleusercontent.com/abc==" + "=s0"},
		{"signed redirect exact", "https://contribution.usercontent.google.com/download?c=AB%3D%3D&sig=x%2By", "https://contribution.usercontent.google.com/download?c=AB%3D%3D&sig=x%2By"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := upscaleImageURL(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := upscaleImageURL("https://evil.example/abc"); err == nil {
		t.Fatal("untrusted host must be refused")
	}
}

func TestValidateUpscaledImageDetectsFormatAndDimensions(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 5, 3))); err != nil {
		t.Fatal(err)
	}
	pngBytes := buf.Bytes()
	got, err := validateUpscaledImage(pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.MimeType != "image/png" || got.Width != 5 || got.Height != 3 {
		t.Fatalf("got %+v", got)
	}
	if !bytes.Equal(got.Bytes, pngBytes) {
		t.Fatal("bytes must be returned unchanged")
	}
	if _, err := validateUpscaledImage([]byte("<html>not an image</html>")); err == nil {
		t.Fatal("non-image body must be rejected")
	}
	if _, err := validateUpscaledImage([]byte("RIFF\x00\x00\x00\x00WEBP")); err == nil {
		t.Fatal("WebP header without image chunk must be rejected")
	}
	if mime := detectedImageMIME(base64.StdEncoding.EncodeToString(pngBytes)); mime != "image/png" {
		t.Fatalf("detectedImageMIME = %q", mime)
	}
}

func TestSyntheticPNGEncodesAsExpected(t *testing.T) {
	// Guards the helper above: a solid-colored image still validates.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "\x89PNG") {
		t.Fatal("expected PNG signature")
	}
}
