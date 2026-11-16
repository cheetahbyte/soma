package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/cheetahbyte/soma"
)

// CallArguments returns the argument text as the model sent it. Arguments
// that were not valid JSON are stored as a JSON string.
func CallArguments(args json.RawMessage) string {
	var malformed string
	if json.Unmarshal(args, &malformed) == nil {
		return malformed
	}
	if len(bytes.TrimSpace(args)) == 0 {
		return "{}"
	}
	return string(args)
}

// ToolOutputText joins the text blocks of a tool result.
func ToolOutputText(blocks []soma.Block) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == soma.BlockText {
			texts = append(texts, b.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if strings.TrimSpace(text) == "" {
		text = "(no tool output)"
	}
	return text
}

// DataURL loads an image block's blob and returns it as a data URL.
func DataURL(b soma.Block, load func(name string) ([]byte, error)) (string, error) {
	if load == nil {
		return "", errors.New("no blob store")
	}
	data, err := load(b.Blob)
	if err != nil {
		return "", err
	}
	return "data:" + b.MIME + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// Replayable reports whether reasoning and item ids of m may be sent back in
// a request to model on provider over api: they are valid only where they
// were produced, and an aborted reply may hold an unpaired reasoning item.
func Replayable(m soma.Message, provider, api, model string) bool {
	return m.Provider == provider && m.API == api && m.Model == model && m.StopReason != soma.StopAborted
}

// ItemRef is what a Responses API text or tool call block keeps in Opaque:
// the output item it came from.
type ItemRef struct {
	ID    string `json:"id,omitempty"`
	Phase string `json:"phase,omitempty"`
}

var idUnsafe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[^a-zA-Z0-9_-]`) })

// ResponsesCallID is a call id from another provider made acceptable to the
// Responses API, as Pi's normalizeIdPart does: allowed characters only, at
// most 64, no trailing underscores.
func ResponsesCallID(id string) string {
	s := idUnsafe().ReplaceAllString(id, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.TrimRight(s, "_")
}

// ChatCallID is the id of a Responses API call replayed to Chat Completions,
// as Pi's convertMessages builds it: the call and item ids joined, so calls
// that share a call id stay distinct, and hashed down to OpenAI's 40.
func ChatCallID(callID, itemID string) string {
	c := idUnsafe().ReplaceAllString(callID, "_")
	combined := c
	if itemID != "" {
		combined = c + "_" + idUnsafe().ReplaceAllString(itemID, "_")
	}
	if len(combined) <= 40 {
		return combined
	}
	hash := shortHash(callID + "|" + itemID)
	hash = hash[:min(len(hash), 8)]
	return c[:min(len(c), max(1, 40-len(hash)-1))] + "_" + hash
}

// shortHash is Pi's utils/hash.js shortHash, over UTF-16 code units.
func shortHash(s string) string {
	h1, h2 := uint32(0xdeadbeef), uint32(0x41c6ce57)
	for _, ch := range utf16.Encode([]rune(s)) {
		h1 = (h1 ^ uint32(ch)) * 2654435761
		h2 = (h2 ^ uint32(ch)) * 1597334677
	}
	h1 = (h1^h1>>16)*2246822507 ^ (h2^h2>>13)*3266489909
	h2 = (h2^h2>>16)*2246822507 ^ (h1^h1>>13)*3266489909
	return strconv.FormatUint(uint64(h2), 36) + strconv.FormatUint(uint64(h1), 36)
}
