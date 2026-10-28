package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var genIDPattern = regexp.MustCompile(`^[a-z0-9]{16}$`)

// captureRPC answers the RPC with a valid upscale frame and records the request
// it received. The fixture uses only synthetic identifiers.
func captureRPC(t *testing.T) *upscaleCounts {
	t.Helper()
	counts := &upscaleCounts{}
	stubUpscaleTransport(t, upscaleFrame(testUpscaleGateway), http.StatusOK, pngBytes(t, 8, 4), "image/png", counts)
	return counts
}

// innerGenerationIDs returns the two generation-ID slots from the c8o8Fe inner
// payload, so the test can compare them with the header and each other.
func innerGenerationIDs(t *testing.T, fReq string) (string, string) {
	t.Helper()
	var envelope [][][]interface{}
	if err := json.Unmarshal([]byte(fReq), &envelope); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	inner := envelope[0][0][1].(string)
	var payload []interface{}
	if err := json.Unmarshal([]byte(inner), &payload); err != nil {
		t.Fatalf("inner: %v", err)
	}
	first := payload[0].([]interface{})
	second := payload[1].([]interface{})
	return first[len(first)-1].(string), second[len(second)-1].(string)
}

// TestUpscaleRPCWireShape pins the header, query and identifier shape the
// protocol facts require, using only synthetic data.
func TestUpscaleRPCWireShape(t *testing.T) {
	counts := captureRPC(t)
	client := upscaleTestClient("session=test-cookie")
	if _, err := client.fetchUpscaledImage(context.Background(), testRef(), "a cat", "session=test-cookie"); err != nil {
		t.Fatalf("fetchUpscaledImage: %v", err)
	}
	req := counts.rpcRequest

	if got := req.Header.Get(geminiModelHeaderKey); got != `[1,null,null,null,null,null,null,0,[4,4]]` {
		t.Fatalf("model header = %s", got)
	}
	if req.Header.Get("x-goog-ext-73010989-jspb") != "[0]" {
		t.Fatalf("x-goog-ext-73010989-jspb = %q", req.Header.Get("x-goog-ext-73010989-jspb"))
	}

	query := req.URL.Query()
	wantKeys := []string{"_reqid", "bl", "f.sid", "rpcids", "rt", "source-path"}
	gotKeys := make([]string, 0, len(query))
	for key := range query {
		gotKeys = append(gotKeys, key)
	}
	if strings.Join(sortedKeys(gotKeys), ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("query keys = %v, want %v", sortedKeys(gotKeys), wantKeys)
	}
	if query.Get("_reqid") == "" || !isFiveDigit(query.Get("_reqid")) {
		t.Fatalf("_reqid = %q, want a five-digit value", query.Get("_reqid"))
	}
	if query.Get("source-path") != geminiSourcePath("2") {
		t.Fatalf("source-path = %q, want the account source path with no CID suffix", query.Get("source-path"))
	}
	if strings.Contains(req.URL.RawQuery, "hl=") {
		t.Fatal("query must not carry hl")
	}

	first, second := innerGenerationIDs(t, counts.rpcForm["f.req"])
	if !genIDPattern.MatchString(first) || first != second {
		t.Fatalf("inner generation IDs %q and %q must match and be 16 lowercase alphanumerics", first, second)
	}
}

// TestUpscaleRPCGenerationIDIsFreshPerRequest checks each RPC gets a new ID,
// not the client's lifetime generation ID.
func TestUpscaleRPCGenerationIDIsFreshPerRequest(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		counts := captureRPC(t)
		client := upscaleTestClient("session=test-cookie")
		client.generationID = "lifetime-client-id"
		if _, err := client.fetchUpscaledImage(context.Background(), testRef(), "a cat", "session=test-cookie"); err != nil {
			t.Fatalf("fetchUpscaledImage: %v", err)
		}
		first, _ := innerGenerationIDs(t, counts.rpcForm["f.req"])
		if first == "lifetime-client-id" {
			t.Fatal("RPC reused the client lifetime generation ID")
		}
		if seen[first] {
			t.Fatalf("generation ID %q repeated across RPCs", first)
		}
		seen[first] = true
	}
}

// TestUpscaleRPCCapsSkipRequest keeps overlong or incomplete input from ever
// reaching the wire.
func TestUpscaleRPCCapsSkipRequest(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name   string
		ref    *upscaleRef
		prompt string
	}{
		{name: "prompt over cap", ref: testRef(), prompt: long(maxUpscalePromptBytes + 1)},
		{name: "token over cap", ref: &upscaleRef{CID: "c_abc", RID: "r_def", RCID: "rc_1", Token: long(maxUpscaleTokenBytes + 1), ContentRef: testContentRef}, prompt: "a cat"},
		{name: "content ref over cap", ref: &upscaleRef{CID: "c_abc", RID: "r_def", RCID: "rc_1", Token: "tok-a", ContentRef: long(maxUpscaleContentBytes + 1)}, prompt: "a cat"},
		{name: "incomplete ref", ref: &upscaleRef{CID: "c_abc"}, prompt: "a cat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counts := &upscaleCounts{}
			stubUpscaleTransport(t, upscaleFrame(testUpscaleGateway), http.StatusOK, pngBytes(t, 2, 2), "image/png", counts)
			client := upscaleTestClient("session=test-cookie")
			if _, err := client.fetchUpscaledImage(context.Background(), tc.ref, tc.prompt, "session=test-cookie"); err == nil {
				t.Fatal("expected fallback")
			}
			if counts.rpc != 0 || counts.download != 0 || counts.generate != 0 {
				t.Fatalf("requests rpc=%d download=%d generate=%d, want none", counts.rpc, counts.download, counts.generate)
			}
		})
	}
}

func isFiveDigit(value string) bool {
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
		n++
	}
	return n == 5
}

func sortedKeys(keys []string) []string {
	sort.Strings(keys)
	return keys
}
