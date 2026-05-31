package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"google.golang.org/genai"
)

// Multimodal content support — image parts in particular.
//
// Cline and other modern LLM clients ship images using one of two wire forms:
//
//  1. OpenAI parts-array form (the form Cline's "OpenAI Compatible" provider
//     uses, and the form recommended by OpenAI / Anthropic / Vertex MaaS
//     vision models):
//
//       "content": [
//         { "type": "text",      "text": "what is this?" },
//         { "type": "image_url", "image_url": { "url": "data:image/png;base64,iVBORw0..." } }
//       ]
//
//  2. Ollama-native form (per-message `images` array, bare base64 — no
//     `data:image/...;base64,` prefix, MIME inferred from the magic bytes):
//
//       { "role": "user", "content": "what is this?", "images": [ "iVBORw..." ] }
//
// Both forms decode to the same intermediate `mediaPart` slice on the
// gateway side, which is then folded into `*genai.Part` (with `InlineData`
// populated) and handed to the per-publisher adapters. Adapters that
// support images encode the `InlineData` payload into their native shape;
// adapters that don't return a clear error rather than silently dropping
// the image.
//
// Why inline base64 only (no remote URL fetching): Cline embeds the image
// directly; we never need to fetch one. Fetching arbitrary `https://` URLs
// from the gateway turns it into an SSRF vector. Add `GW_ALLOW_IMAGE_URL_HTTP`
// later if a real caller asks.

// Per-media size guards (which cover images, audio, video, PDF). Defaults are roomy enough
// for any realistic Cline screenshot but tight enough to bound memory.
var (
	maxMediaBytesPerPart    = envIntDefault("GW_MAX_MEDIA_BYTES_PER_PART", 10*1024*1024)
	maxMediaBytesPerRequest = envIntDefault("GW_MAX_MEDIA_BYTES_PER_REQUEST", 32*1024*1024)
)

// supportedImageMIMETypes is the allowlist of MIME types we accept inline.
var supportedImageMIMETypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/gif":  {},
}

// supportedAudioMIMETypes is the allowlist of audio MIME types we accept inline.
var supportedAudioMIMETypes = map[string]struct{}{
	"audio/wav":     {},
	"audio/x-wav":   {},
	"audio/mp3":     {},
	"audio/mpeg":    {},
	"audio/mpeg3":   {},
	"audio/x-mpeg3": {},
	"audio/ogg":     {},
	"audio/flac":    {},
	"audio/aac":     {},
	"audio/webm":    {},
	"audio/mp4":     {},
	"audio/m4a":     {},
	"audio/x-m4a":   {},
	"audio/opus":    {},
}

// supportedVideoMIMETypes is the allowlist of video MIME types we accept inline.
var supportedVideoMIMETypes = map[string]struct{}{
	"video/mp4":       {},
	"video/webm":      {},
	"video/quicktime": {},
	"video/mpeg":      {},
	"video/x-mpeg":    {},
	"video/x-msvideo": {},
	"video/x-flv":     {},
	"video/ogg":       {},
	"video/3gpp":      {},
	"video/mov":       {},
}

// supportedDocumentMIMETypes is the allowlist of document MIME types we accept inline.
var supportedDocumentMIMETypes = map[string]struct{}{
	"application/pdf": {},
}

// isSupportedMIME reports whether the given MIME type is in our allowed sets.
func isSupportedMIME(mime string) bool {
	if _, ok := supportedImageMIMETypes[mime]; ok {
		return true
	}
	if _, ok := supportedAudioMIMETypes[mime]; ok {
		return true
	}
	if _, ok := supportedVideoMIMETypes[mime]; ok {
		return true
	}
	if _, ok := supportedDocumentMIMETypes[mime]; ok {
		return true
	}
	return false
}

// isImageMIME reports whether the given MIME type is an image format.
func isImageMIME(mime string) bool {
	return strings.HasPrefix(mime, "image/")
}

// envIntDefault returns the integer value of `name` (if set and >0) else def.
// Negative or unparseable values fall back to the default so a typo in a knob
// doesn't quietly disable a size guard.
func envIntDefault(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// mediaPart is the gateway-internal representation of one inline image (or,
// in the future, audio) part attached to a message. Either Text OR (MIME +
// Data) is populated, never both.
type mediaPart struct {
	Text string // populated for text parts
	MIME string // populated for binary parts ("image/png" etc.)
	Data []byte // raw decoded bytes
}

// isText reports whether the part is a text part (vs an inline media part).
func (m mediaPart) isText() bool { return m.MIME == "" }

// oaiContentPart mirrors the OpenAI content-parts wire shape for both
// decoding and re-emission tests. Polymorphic via the Type discriminator.
type oaiInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"` // e.g. "wav", "mp3"
}

type oaiContentPart struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	ImageURL   *oaiImageURL   `json:"image_url,omitempty"`
	InputAudio *oaiInputAudio `json:"input_audio,omitempty"`
}

// oaiImageURL is the nested object on an image_url part. We only consume
// URL; the `detail` ("low"/"high"/"auto") hint is accepted but ignored
// because none of the upstream adapters we ship today expose a way to plumb
// it through (Gemini doesn't have it; Anthropic, Cohere, OpenAI-compat each
// have their own quality knob that doesn't translate one-to-one).
type oaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ContentParts decodes the polymorphic `content` field on an OAIChatMessage
// into an ordered slice of mediaParts (text + image/audio/video/document). It accepts:
//
//   - bare string                       → []{Text:s}
//   - parts array with type:"text"      → text part
//   - parts array with type:"image_url" → image or media part (data URL decoded)
//   - parts array with type:"input_audio" → audio part
//
// Unknown part types are skipped. On any parse failure for an individual part the function
// returns the parts decoded so far plus an error — handlers should treat
// this as a 400 because silently dropping requested media would mislead
// the caller about what the model actually saw/heard.
//
// Returns (nil, nil) for a missing/empty content field — same semantics as
// the existing ContentString.
func (m OAIChatMessage) ContentParts() ([]mediaPart, error) {
	if len(m.Content) == 0 {
		return nil, nil
	}
	// Fast-path: bare string.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []mediaPart{{Text: s}}, nil
	}
	// Parts array.
	var rawParts []oaiContentPart
	if err := json.Unmarshal(m.Content, &rawParts); err != nil {
		return nil, fmt.Errorf("decode content parts: %w", err)
	}
	out := make([]mediaPart, 0, len(rawParts))
	for i, p := range rawParts {
		switch p.Type {
		case "", "text":
			if p.Text == "" {
				continue
			}
			out = append(out, mediaPart{Text: p.Text})
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return out, fmt.Errorf("content part %d: image_url missing url", i)
			}
			mime, data, err := parseDataURL(p.ImageURL.URL)
			if err != nil {
				return out, fmt.Errorf("content part %d: %w", i, err)
			}
			out = append(out, mediaPart{MIME: mime, Data: data})
		case "input_audio":
			if p.InputAudio == nil || p.InputAudio.Data == "" || p.InputAudio.Format == "" {
				return out, fmt.Errorf("content part %d: input_audio missing data or format", i)
			}
			mime := "audio/" + strings.ToLower(strings.TrimSpace(p.InputAudio.Format))
			if mime == "audio/mp3" {
				mime = "audio/mpeg"
			}
			if !isSupportedMIME(mime) {
				return out, fmt.Errorf("content part %d: unsupported audio format %q", i, p.InputAudio.Format)
			}
			data, err := decodeBase64Lenient(p.InputAudio.Data)
			if err != nil {
				return out, fmt.Errorf("content part %d decode base64: %w", i, err)
			}
			if len(data) == 0 {
				return out, fmt.Errorf("content part %d: empty audio payload", i)
			}
			if len(data) > maxMediaBytesPerPart {
				return out, fmt.Errorf("content part %d: audio too large: %d bytes exceeds per-part cap of %d (GW_MAX_MEDIA_BYTES_PER_PART)",
					i, len(data), maxMediaBytesPerPart)
			}
			out = append(out, mediaPart{MIME: mime, Data: data})
		default:
			// Unknown / future type — skip silently.
			continue
		}
	}
	return out, nil
}

// parseDataURL parses a `data:<mime>;base64,<payload>` URL into the MIME
// type and decoded bytes. Strict — non-data scheme URLs are rejected so
// we don't accidentally become an HTTP proxy (SSRF vector).
//
// The base64 payload is accepted in either standard or url-safe alphabet
// and with or without padding (some browsers strip it).
func parseDataURL(s string) (string, []byte, error) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", nil, errors.New("url must be a data: URL (remote URLs not supported)")
	}
	rest := s[len(prefix):]
	commaIdx := strings.Index(rest, ",")
	if commaIdx < 0 {
		return "", nil, errors.New("malformed data url: missing ','")
	}
	meta := rest[:commaIdx]
	payload := rest[commaIdx+1:]

	// meta is "<mime>;base64" or "<mime>". We require base64 encoding.
	if !strings.Contains(meta, ";base64") {
		return "", nil, errors.New("malformed data url: only base64-encoded payloads supported")
	}
	mime := strings.TrimSuffix(meta, ";base64")
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "" {
		return "", nil, errors.New("malformed data url: missing mime type")
	}
	if !isSupportedMIME(mime) {
		return "", nil, fmt.Errorf("unsupported mime type %q", mime)
	}
	data, err := decodeBase64Lenient(payload)
	if err != nil {
		return "", nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(data) == 0 {
		return "", nil, errors.New("empty payload")
	}
	if len(data) > maxMediaBytesPerPart {
		return "", nil, fmt.Errorf("payload too large: %d bytes exceeds per-part cap of %d (GW_MAX_MEDIA_BYTES_PER_PART)",
			len(data), maxMediaBytesPerPart)
	}
	return mime, data, nil
}

// decodeBase64Lenient accepts std/url, padded/unpadded base64 (real-world
// clients ship all four combinations). Falls through encodings in order
// from most-common to least.
func decodeBase64Lenient(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	// Strip whitespace inside the payload — some pretty-printers wrap long
	// base64 strings at 76 cols. Doing this as a single Replacer avoids
	// allocating per-char.
	if strings.ContainsAny(s, " \t\r\n") {
		s = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(s)
	}
	if d, err := base64.StdEncoding.DecodeString(s); err == nil {
		return d, nil
	}
	if d, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return d, nil
	}
	if d, err := base64.URLEncoding.DecodeString(s); err == nil {
		return d, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// sniffImageMIME inspects the magic bytes of an image payload and returns
// the matching MIME type ("image/png" etc.). Kept for backward compatibility
// and existing unit tests.
func sniffImageMIME(data []byte) string {
	mime := sniffMediaMIME(data)
	if isImageMIME(mime) {
		return mime
	}
	return ""
}

// sniffMediaMIME inspects the magic bytes of a media payload (image, audio, video, pdf)
// and returns the matching MIME type. Used for the Ollama-native images/media array.
func sniffMediaMIME(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	// 1. PDF
	if len(data) >= 5 && string(data[0:5]) == "%PDF-" {
		return "application/pdf"
	}
	// 2. WAV & WebP (both use RIFF header)
	if len(data) >= 12 && string(data[0:4]) == "RIFF" {
		riffType := string(data[8:12])
		switch riffType {
		case "WEBP":
			return "image/webp"
		case "WAVE":
			return "audio/wav"
		}
	}
	// 3. FLAC
	if len(data) >= 4 && string(data[0:4]) == "fLaC" {
		return "audio/flac"
	}
	// 4. Ogg
	if len(data) >= 4 && string(data[0:4]) == "OggS" {
		return "audio/ogg"
	}
	// 5. PNG, JPEG, GIF
	if len(data) >= 8 && string(data[0:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	if len(data) >= 6 && (string(data[0:6]) == "GIF87a" || string(data[0:6]) == "GIF89a") {
		return "image/gif"
	}
	// 6. MP3
	if len(data) >= 3 && string(data[0:3]) == "ID3" {
		return "audio/mp3"
	}
	if len(data) >= 2 && data[0] == 0xFF && (data[1]&0xE0) == 0xE0 {
		return "audio/mp3"
	}
	// 7. MP4 / MOV (contains "ftyp" at index 4)
	if len(data) >= 8 && string(data[4:8]) == "ftyp" {
		return "video/mp4"
	}
	// 8. WebM / MKV
	if len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		return "video/webm"
	}

	return ""
}

// decodeOllamaImage decodes one entry from an Ollama-shape `images` array.
// Ollama's wire format is bare base64 with no MIME prefix — we sniff the
// magic bytes to pick a MIME. Returns an error if the bytes don't look
// like a supported image or media format.
func decodeOllamaImage(b64 string) (string, []byte, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return "", nil, errors.New("empty entry")
	}
	if strings.HasPrefix(b64, "data:") {
		return parseDataURL(b64)
	}
	data, err := decodeBase64Lenient(b64)
	if err != nil {
		return "", nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(data) == 0 {
		return "", nil, errors.New("empty payload after base64 decode")
	}
	mime := sniffMediaMIME(data)
	if mime == "" {
		return "", nil, errors.New("could not identify media format (expected image, audio, video, or pdf magic bytes)")
	}
	if len(data) > maxMediaBytesPerPart {
		return "", nil, fmt.Errorf("payload too large: %d bytes exceeds per-part cap of %d (GW_MAX_MEDIA_BYTES_PER_PART)",
			len(data), maxMediaBytesPerPart)
	}
	return mime, data, nil
}

// mediaPartsToGenai converts a slice of mediaParts into a slice of
// *genai.Part suitable for stuffing into a genai.Content.Parts list. Text
// parts become {Text:...} parts; image parts become {InlineData:{MIME,Data}}
// parts. Caller is responsible for grouping by role.
func mediaPartsToGenai(parts []mediaPart) []*genai.Part {
	out := make([]*genai.Part, 0, len(parts))
	for _, p := range parts {
		if p.isText() {
			if p.Text == "" {
				continue
			}
			out = append(out, &genai.Part{Text: p.Text})
			continue
		}
		out = append(out, &genai.Part{
			InlineData: &genai.Blob{
				MIMEType: p.MIME,
				Data:     p.Data,
			},
		})
	}
	return out
}

// concatText joins the text portions of a mediaPart slice with newlines.
// Used in code paths (logging, dedup-input prep) that want a flat text
// view of a message even when it carries images.
func concatText(parts []mediaPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if !p.isText() {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// totalMediaBytes sums the decoded byte count of every media part.
func totalMediaBytes(parts []mediaPart) int {
	n := 0
	for _, p := range parts {
		if !p.isText() {
			n += len(p.Data)
		}
	}
	return n
}

// totalImageBytes is a compatibility wrapper for totalMediaBytes.
func totalImageBytes(parts []mediaPart) int {
	return totalMediaBytes(parts)
}
