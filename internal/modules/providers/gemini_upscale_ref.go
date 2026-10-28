package providers

import "strings"

// upscaleContentRefPrefix is the only content-reference shape the upscale RPC
// accepts. Anything else is treated as missing metadata.
const upscaleContentRefPrefix = "http://googleusercontent.com/image_generation_content/"

// upscaleRef is the response-local metadata needed to request Gemini's native
// upscaled bytes for one generated image. Every field comes from the same
// candidate of the same response frame. Mixing identifiers across candidates
// or turns would upscale the wrong image, so an incomplete ref is never used.
type upscaleRef struct {
	CID        string // conversation ID from the payload CID/RID pair
	RID        string // response ID from the same payload pair
	RCID       string // candidate ID from candidate[0]
	Token      string // per-image token at entry[0][3][5]
	ContentRef string // image content reference at entry[1][0]
}

// complete reports whether every identifier the upscale RPC needs is set.
func (r *upscaleRef) complete() bool {
	return r != nil && r.CID != "" && r.RID != "" && r.RCID != "" && r.Token != "" && r.ContentRef != ""
}

// responsePairFromPayload reads the CID/RID pair from payload[1]. The slot is
// either a bare conversation ID or a [cid, rid] list; RID is empty for the former.
func responsePairFromPayload(payload []interface{}) (cid, rid string) {
	if len(payload) < 2 {
		return "", ""
	}
	switch v := payload[1].(type) {
	case string:
		return v, ""
	case []interface{}:
		if len(v) > 0 {
			cid, _ = v[0].(string)
		}
		if len(v) > 1 {
			rid, _ = v[1].(string)
		}
	}
	return cid, rid
}

// upscaleRefsByURL maps each generated image URL in one candidate to its ref.
// A URL with a missing or incomplete ref maps to nil, and so does a URL whose
// duplicate entries disagree, so both fall back to the standard download.
func upscaleRefsByURL(candidate []interface{}, cid, rid string) map[string]*upscaleRef {
	refs := make(map[string]*upscaleRef)
	for _, entry := range generatedEntries(candidate) {
		url := entryImageURL(entry)
		if url == "" {
			continue
		}
		ref := extractUpscaleRef(candidate, cid, rid, entry)
		if prev, seen := refs[url]; seen && !sameUpscaleRef(prev, ref) {
			refs[url] = nil
			continue
		}
		refs[url] = ref
	}
	return refs
}

// generatedEntries returns the raw entries of the candidate[12][7][0] slot.
func generatedEntries(candidate []interface{}) [][]interface{} {
	if len(candidate) <= 12 {
		return nil
	}
	candidateMedia, ok := candidate[12].([]interface{})
	if !ok || len(candidateMedia) <= 7 {
		return nil
	}
	mediaGroups, ok := candidateMedia[7].([]interface{})
	if !ok || len(mediaGroups) == 0 {
		return nil
	}
	generated, ok := mediaGroups[0].([]interface{})
	if !ok {
		return nil
	}
	entries := make([][]interface{}, 0, len(generated))
	for _, raw := range generated {
		if entry, ok := raw.([]interface{}); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// entryImageURL returns the normalized image URL stored at entry[0][3][3].
func entryImageURL(entry []interface{}) string {
	if len(entry) == 0 {
		return ""
	}
	wrapper, ok := entry[0].([]interface{})
	if !ok || len(wrapper) <= 3 {
		return ""
	}
	metadata, ok := wrapper[3].([]interface{})
	if !ok || len(metadata) <= 3 {
		return ""
	}
	raw, _ := metadata[3].(string)
	return normalizeImageURL(raw)
}

// extractUpscaleRef builds the ref for one generated entry from the same
// candidate and payload as the entry itself. It returns nil when any
// identifier is missing or malformed.
func extractUpscaleRef(candidate []interface{}, cid, rid string, entry []interface{}) *upscaleRef {
	if len(candidate) == 0 || len(entry) < 2 {
		return nil
	}
	rcid, _ := candidate[0].(string)
	wrapper, ok := entry[0].([]interface{})
	if !ok || len(wrapper) <= 3 {
		return nil
	}
	metadata, ok := wrapper[3].([]interface{})
	if !ok || len(metadata) <= 5 {
		return nil
	}
	token, _ := metadata[5].(string)
	// metadata[5] can hold a MIME type in some responses. A MIME value is not a
	// per-image token, so it must never permit the upscale RPC.
	if looksLikeMIME(token) {
		return nil
	}
	refNode, ok := entry[1].([]interface{})
	if !ok || len(refNode) == 0 {
		return nil
	}
	contentRef, _ := refNode[0].(string)
	if !strings.HasPrefix(contentRef, upscaleContentRefPrefix) {
		return nil
	}
	ref := &upscaleRef{CID: cid, RID: rid, RCID: rcid, Token: token, ContentRef: contentRef}
	if ref.CID == "" || ref.RID == "" || ref.RCID == "" || ref.Token == "" {
		return nil
	}
	return ref
}

// looksLikeMIME reports whether a value is a media type such as image/png.
func looksLikeMIME(value string) bool {
	major, _, found := strings.Cut(value, "/")
	if !found || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	switch major {
	case "image", "video", "audio", "text", "application":
		return true
	}
	return false
}

// sameUpscaleRef reports whether two refs name the same image. A nil ref
// matches only nil.
func sameUpscaleRef(a, b *upscaleRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
