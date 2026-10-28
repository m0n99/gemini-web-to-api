package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg" // registers the JPEG decoder used by image.DecodeConfig
	_ "image/png"  // registers the PNG decoder used by image.DecodeConfig
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	crand "crypto/rand"

	"go.uber.org/zap"
)

// geminiUpscaleRPC is the batchexecute RPC that returns the native upscaled
// image URL for one generated image.
const geminiUpscaleRPC = "c8o8Fe"

// upscaleTimeout bounds the RPC. The byte download uses the generated-image
// HTTP client, which has its own timeout.
const upscaleTimeout = time.Minute

// errUpscaleUnavailable marks a native upscale that cannot be used. The caller
// falls back to the standard download. Nothing here regenerates an image.
var errUpscaleUnavailable = errors.New("native upscale unavailable")

// upscaleResult is one validated native upscale output. Bytes are kept as the
// server sent them; only the detected format and dimensions are added.
type upscaleResult struct {
	Bytes    []byte
	MimeType string
	Width    int
	Height   int
}

// Protocol caps. An oversized prompt or reference skips the RPC and keeps the
// standard download, so no request is built from unbounded input.
const (
	maxUpscalePromptBytes  = 32 << 10
	maxUpscaleTokenBytes   = 8 << 10
	maxUpscaleIDsBytes     = 512
	maxUpscaleContentBytes = 256
	maxUpscaleInnerBytes   = 256 << 10
)

// upscaleModelHeader is the exact model header for the c8o8Fe RPC.
const upscaleModelHeader = `[1,null,null,null,null,null,null,0,[4,4]]`

// checkUpscaleCaps rejects incomplete or overlong inputs before any RPC is sent.
func checkUpscaleCaps(ref *upscaleRef, prompt string) error {
	if !ref.complete() {
		return errUpscaleUnavailable
	}
	if len(prompt) > maxUpscalePromptBytes || len(ref.Token) > maxUpscaleTokenBytes || len(ref.ContentRef) > maxUpscaleContentBytes {
		return errUpscaleUnavailable
	}
	if len(ref.CID)+len(ref.RID)+len(ref.RCID) > maxUpscaleIDsBytes {
		return errUpscaleUnavailable
	}
	return nil
}

// freshUpscaleGenerationID returns 16 random lowercase alphanumeric characters
// from crypto/rand, one per RPC.
func freshUpscaleGenerationID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 16)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

// upscaleRequestPayload builds the c8o8Fe message for the image identified by
// the response metadata and its generation prompt.
func upscaleRequestPayload(ref *upscaleRef, prompt, genID string) []interface{} {
	return []interface{}{
		[]interface{}{
			[]interface{}{nil, nil, nil, []interface{}{nil, nil, nil, nil, nil, ref.Token}},
			[]interface{}{ref.ContentRef, 0},
			nil,
			[]interface{}{19, prompt},
			nil, nil, nil, nil, nil,
			genID,
		},
		[]interface{}{ref.RID, ref.RCID, ref.CID, nil, genID},
		1,
		0,
		1,
	}
}

// parseUpscaleURL finds the c8o8Fe record in a batchexecute body and returns
// the URL in its payload[0]. Other RPC records and malformed frames are
// skipped, so a missing or unusable record returns errUpscaleUnavailable.
func parseUpscaleURL(body []byte) (string, error) {
	body = bytes.TrimPrefix(bytes.TrimSpace(body), []byte(")]}'"))
	decoder := json.NewDecoder(bytes.NewReader(body))
	for {
		var value any
		if err := decoder.Decode(&value); err != nil {
			break
		}
		if rawURL, ok := upscaleURLFromValue(value); ok {
			return rawURL, nil
		}
	}
	return "", errUpscaleUnavailable
}

// upscaleURLFromValue walks nested arrays for a wrb.fr record of the upscale RPC.
func upscaleURLFromValue(value any) (string, bool) {
	items, ok := value.([]interface{})
	if !ok {
		return "", false
	}
	if len(items) >= 3 && items[0] == "wrb.fr" && items[1] == geminiUpscaleRPC {
		payloadText, ok := items[2].(string)
		if !ok {
			return "", false
		}
		var payload []interface{}
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil || len(payload) == 0 {
			return "", false
		}
		rawURL, ok := payload[0].(string)
		return rawURL, ok && rawURL != ""
	}
	for _, child := range items {
		if rawURL, ok := upscaleURLFromValue(child); ok {
			return rawURL, true
		}
	}
	return "", false
}

// upscaleImageURL returns the URL the RPC handed back. Only the known initial
// gateway gets =s0, appended as raw text before any query or fragment. Every
// other URL, including signed redirects, is returned byte-for-byte so token
// padding and query values survive.
func upscaleImageURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !isTrustedGoogleMediaHost(parsed.Hostname()) {
		return "", errUpscaleUnavailable
	}
	if !isInitialUpscaleGateway(parsed.Hostname()) || hasSizeSuffix(parsed.EscapedPath()) {
		return rawURL, nil
	}
	pathEnd := len(rawURL)
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		pathEnd = i
	}
	return rawURL[:pathEnd] + "=s0" + rawURL[pathEnd:], nil
}

// hasSizeSuffix reports whether the last path segment already ends in a Google
// size directive (=sN or =wN). Padding such as a trailing "=" is not a size.
func hasSizeSuffix(escapedPath string) bool {
	segment := path.Base(escapedPath)
	idx := strings.LastIndex(segment, "=")
	if idx < 0 || idx+2 > len(segment) {
		return false
	}
	kind := segment[idx+1]
	if kind != 's' && kind != 'w' {
		return false
	}
	digits := segment[idx+2:]
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isInitialUpscaleGateway names the only host that receives =s0. It is the
// Google-hosted initial gateway; redirects it issues are never rewritten.
func isInitialUpscaleGateway(host string) bool {
	return strings.EqualFold(host, "lh3.googleusercontent.com")
}

// validateUpscaledImage accepts PNG, JPEG and WebP bytes as received. It
// returns the detected MIME and dimensions and never transcodes the bytes.
func validateUpscaledImage(body []byte) (upscaleResult, error) {
	if isWebPHeader(body) {
		width, height, ok := webPDimensions(body)
		if !ok {
			return upscaleResult{}, errUpscaleUnavailable
		}
		return upscaleResult{Bytes: body, MimeType: "image/webp", Width: width, Height: height}, nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return upscaleResult{}, errUpscaleUnavailable
	}
	switch format {
	case "png":
		return upscaleResult{Bytes: body, MimeType: "image/png", Width: cfg.Width, Height: cfg.Height}, nil
	case "jpeg":
		return upscaleResult{Bytes: body, MimeType: "image/jpeg", Width: cfg.Width, Height: cfg.Height}, nil
	}
	return upscaleResult{}, errUpscaleUnavailable
}

// detectedImageMIME returns the MIME of base64-encoded PNG, JPEG or WebP bytes,
// or "" when the bytes are none of these.
func detectedImageMIME(encoded string) string {
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	result, err := validateUpscaledImage(body)
	if err != nil {
		return ""
	}
	return result.MimeType
}

// webPContainerHasImage checks that the RIFF container is not truncated and
// contains a VP8 or VP8L chunk whose payload lies inside it. Header bytes alone
// are not an image, so a header-only file is rejected here.
func webPContainerHasImage(body []byte) bool {
	riffEnd := 8 + int(binary.LittleEndian.Uint32(body[4:8]))
	if riffEnd > len(body) {
		return false
	}
	// The RIFF size counts everything after its own 8-byte header.
	if len(body) < 12 || riffEnd < 12 {
		return false
	}
	found := false
	for offset := 12; offset+8 <= riffEnd; {
		size := int(binary.LittleEndian.Uint32(body[offset+4 : offset+8]))
		payloadEnd := offset + 8 + size
		if payloadEnd > riffEnd {
			return false
		}
		id := string(body[offset : offset+4])
		if (id == "VP8 " || id == "VP8L") && size > 0 {
			found = true
		}
		// Chunk payloads are padded to an even length.
		offset = payloadEnd + (size & 1)
	}
	return found
}

// webPDimensions reads width and height from the VP8, VP8L or VP8X header.
// All three formats are little-endian; VP8L packs the sizes into 14-bit fields.
func webPDimensions(body []byte) (int, int, bool) {
	if len(body) < 30 || !webPContainerHasImage(body) {
		return 0, 0, false
	}
	switch string(body[12:16]) {
	case "VP8X":
		// The stored value is canvas size minus one, so add one to the assembled
		// 24-bit number. Adding it to a single byte would carry incorrectly.
		width := int(uint32(body[24])|uint32(body[25])<<8|uint32(body[26])<<16) + 1
		height := int(uint32(body[27])|uint32(body[28])<<8|uint32(body[29])<<16) + 1
		return width, height, true
	case "VP8L":
		if body[20] != 0x2f {
			return 0, 0, false
		}
		bits := uint32(body[21]) | uint32(body[22])<<8 | uint32(body[23])<<16 | uint32(body[24])<<24
		width := int(bits&0x3fff) + 1
		height := int((bits>>14)&0x3fff) + 1
		return width, height, true
	case "VP8 ":
		if body[23] != 0x9d || body[24] != 0x01 || body[25] != 0x2a {
			return 0, 0, false
		}
		width := int(uint16(body[26])|uint16(body[27])<<8) & 0x3fff
		height := int(uint16(body[28])|uint16(body[29])<<8) & 0x3fff
		return width, height, width > 0 && height > 0
	}
	return 0, 0, false
}

// isWebPHeader requires the RIFF/WEBP container and a VP8, VP8L or VP8X chunk
// right after it. A bare header without image data is rejected.
func isWebPHeader(body []byte) bool {
	if len(body) < 16 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WEBP" {
		return false
	}
	chunk := string(body[12:16])
	return chunk == "VP8 " || chunk == "VP8L" || chunk == "VP8X"
}

// fetchUpscaledImage runs the RPC and downloads the bytes it names. Any failure
// returns errUpscaleUnavailable and the caller uses the standard download.
func (c *Client) fetchUpscaledImage(ctx context.Context, ref *upscaleRef, prompt, cookieHeader string) (upscaleResult, error) {
	if !ref.complete() {
		return upscaleResult{}, errUpscaleUnavailable
	}
	rawURL, err := c.upscaleRPC(ctx, ref, prompt, cookieHeader)
	if err != nil {
		return upscaleResult{}, err
	}
	imageURL, err := upscaleImageURL(rawURL)
	if err != nil {
		return upscaleResult{}, err
	}
	body, err := downloadUpscaledBytes(ctx, imageURL, cookieHeader)
	if err != nil {
		return upscaleResult{}, err
	}
	return validateUpscaledImage(body)
}

// logUpscaleFallback records why native upscale was skipped. It logs only the
// reason, never the URL, token or cookies.
func (c *Client) logUpscaleFallback(err error) {
	c.log.Warn("Native upscale unavailable; using the standard generated image",
		zap.String("reason", err.Error()))
}

// upscaleRPC posts the c8o8Fe batchexecute request with the same session token,
// cookies, account slot, build label and session fields as generation does.
func (c *Client) upscaleRPC(ctx context.Context, ref *upscaleRef, prompt, cookieHeader string) (string, error) {
	c.mu.RLock()
	token, buildLabel, sessionID, authUser := c.at, c.buildLabel, c.sessionID, c.authUser
	c.mu.RUnlock()
	if token == "" || cookieHeader == "" {
		return "", errUpscaleUnavailable
	}
	if err := checkUpscaleCaps(ref, prompt); err != nil {
		return "", err
	}
	// Each RPC gets its own generated ID, sent in both inner slots, so it is not
	// tied to the client's lifetime generation ID.
	generationID, err := freshUpscaleGenerationID()
	if err != nil {
		return "", errUpscaleUnavailable
	}
	inner, err := json.Marshal(upscaleRequestPayload(ref, prompt, generationID))
	if err != nil || len(inner) > maxUpscaleInnerBytes {
		return "", errUpscaleUnavailable
	}
	envelope, err := json.Marshal([]interface{}{[]interface{}{[]interface{}{geminiUpscaleRPC, string(inner), nil, "generic"}}})
	if err != nil {
		return "", errUpscaleUnavailable
	}
	query := url.Values{
		"rpcids":      {geminiUpscaleRPC},
		"_reqid":      {strconv.Itoa(rand.Intn(90000) + 10000)},
		"rt":          {"c"},
		"source-path": {geminiSourcePath(authUser)},
	}
	if buildLabel != "" {
		query.Set("bl", buildLabel)
	}
	if sessionID != "" {
		query.Set("f.sid", sessionID)
	}
	form := url.Values{"at": {token}, "f.req": {string(envelope)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiAccountURL(EndpointBatchExec, authUser)+"?"+query.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", errUpscaleUnavailable
	}
	for name, value := range DefaultHeaders {
		request.Header.Set(name, value)
	}
	request.Header.Set("Cookie", cookieHeader)
	request.Header.Set(geminiModelHeaderKey, upscaleModelHeader)
	request.Header.Set("x-goog-ext-73010989-jspb", "[0]")

	// A redirect would re-send the form token and cookies to another location.
	// Returning the 3xx as the final response makes it an ordinary fallback.
	client := &http.Client{
		Timeout:       upscaleTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return "", errUpscaleUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errUpscaleUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxConversationReadBytes+1))
	if err != nil || len(body) > maxConversationReadBytes {
		return "", errUpscaleUnavailable
	}
	return parseUpscaleURL(body)
}

// downloadUpscaledBytes fetches the exact upscale URL with the generation's
// cookies. Redirects must stay on trusted Google media hosts.
func downloadUpscaledBytes(ctx context.Context, imageURL, cookieHeader string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, errUpscaleUnavailable
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("Cookie", cookieHeader)
	resp, err := generatedImageHTTPClient(cookieHeader).Do(req)
	if err != nil {
		return nil, errUpscaleUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errUpscaleUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGeneratedImageBytes+1))
	if err != nil || len(body) > maxGeneratedImageBytes {
		return nil, errUpscaleUnavailable
	}
	return body, nil
}
