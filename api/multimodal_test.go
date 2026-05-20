package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Magic-byte fixtures for the four image formats we support. Tiny enough to
// not blow up test binaries, but real headers so sniffImageMIME isn't
// pattern-matching against bespoke garbage.
var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\nIHDR....")
	jpegMagic = []byte("\xFF\xD8\xFFE0JFIF....")
	webpMagic = bytes.Join([][]byte{[]byte("RIFF"), []byte("xxxx"), []byte("WEBPVP8 ....")}, nil)
	gif89aMag = []byte("GIF89a..............")
	gif87aMag = []byte("GIF87a..............")
)

func dataURL(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestSniffImageMIME(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"png", pngMagic, "image/png"},
		{"jpeg", jpegMagic, "image/jpeg"},
		{"webp", webpMagic, "image/webp"},
		{"gif89a", gif89aMag, "image/gif"},
		{"gif87a", gif87aMag, "image/gif"},
		{"unknown", []byte("hello world"), ""},
		{"too short", []byte("ab"), ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffImageMIME(tc.in); got != tc.want {
				t.Errorf("sniffImageMIME(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestParseDataURL_AllSupportedMIMETypes(t *testing.T) {
	for _, mime := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		t.Run(mime, func(t *testing.T) {
			payload := []byte("hello-" + mime)
			gotMime, gotData, err := parseDataURL(dataURL(mime, payload))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotMime != mime {
				t.Errorf("mime = %q, want %q", gotMime, mime)
			}
			if !bytes.Equal(gotData, payload) {
				t.Errorf("data round-trip mismatch: got %q, want %q", gotData, payload)
			}
		})
	}
}

func TestParseDataURL_RejectsUnsupportedMIME(t *testing.T) {
	_, _, err := parseDataURL(dataURL("image/svg+xml", []byte("<svg/>")))
	if err == nil {
		t.Fatal("expected error for image/svg+xml; got nil")
	}
	if !strings.Contains(err.Error(), "unsupported image mime type") {
		t.Errorf("error mentions unsupported type? got: %v", err)
	}
}

func TestParseDataURL_RejectsRemoteURL(t *testing.T) {
	for _, u := range []string{
		"https://example.com/cat.png",
		"http://example.com/cat.png",
		"file:///etc/passwd",
	} {
		t.Run(u, func(t *testing.T) {
			_, _, err := parseDataURL(u)
			if err == nil {
				t.Fatalf("expected SSRF-guard rejection for %q; got nil", u)
			}
			if !strings.Contains(err.Error(), "data: URL") {
				t.Errorf("expected error to mention data: URL; got: %v", err)
			}
		})
	}
}

func TestParseDataURL_RequiresBase64(t *testing.T) {
	// data:image/png,xxx  — URL-encoded, not base64; we don't support.
	_, _, err := parseDataURL("data:image/png,iVBORw0KGgo")
	if err == nil {
		t.Fatal("expected error for non-base64 data URL; got nil")
	}
}

func TestParseDataURL_RequiresComma(t *testing.T) {
	_, _, err := parseDataURL("data:image/png;base64")
	if err == nil {
		t.Fatal("expected error for missing comma; got nil")
	}
}

func TestParseDataURL_EmptyPayload(t *testing.T) {
	_, _, err := parseDataURL("data:image/png;base64,")
	if err == nil {
		t.Fatal("expected error for empty payload; got nil")
	}
}

func TestParseDataURL_OversizedRejected(t *testing.T) {
	// Just over the per-part cap. We use the magic bytes prefix so the
	// payload is at least format-valid; the cap check comes first.
	original := maxImageBytesPerPart
	maxImageBytesPerPart = 32 // tighten for the test
	defer func() { maxImageBytesPerPart = original }()
	oversized := append(append([]byte{}, pngMagic...), bytes.Repeat([]byte("A"), 64)...)
	_, _, err := parseDataURL(dataURL("image/png", oversized))
	if err == nil {
		t.Fatal("expected per-part-cap rejection; got nil")
	}
	if !strings.Contains(err.Error(), "GW_MAX_IMAGE_BYTES_PER_PART") {
		t.Errorf("error should reference the env knob; got: %v", err)
	}
}

func TestParseDataURL_AcceptsUrlSafeAndUnpaddedBase64(t *testing.T) {
	// "hi" -> "aGk=" std; raw url-safe is "aGk".
	cases := []string{
		"data:image/png;base64," + base64.StdEncoding.EncodeToString(pngMagic),
		"data:image/png;base64," + base64.RawStdEncoding.EncodeToString(pngMagic),
		"data:image/png;base64," + base64.URLEncoding.EncodeToString(pngMagic),
		"data:image/png;base64," + base64.RawURLEncoding.EncodeToString(pngMagic),
	}
	for _, u := range cases {
		_, data, err := parseDataURL(u)
		if err != nil {
			t.Errorf("unexpected error for %q: %v", u, err)
			continue
		}
		if !bytes.Equal(data, pngMagic) {
			t.Errorf("round-trip mismatch for %q", u)
		}
	}
}

func TestParseDataURL_ToleratesWhitespaceInPayload(t *testing.T) {
	// Some pretty-printers wrap base64 at 76 cols. Make sure we strip whitespace.
	clean := base64.StdEncoding.EncodeToString(pngMagic)
	withWS := clean[:4] + "\n" + clean[4:10] + " " + clean[10:]
	_, data, err := parseDataURL("data:image/png;base64," + withWS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(data, pngMagic) {
		t.Errorf("data mismatch after whitespace strip")
	}
}

func TestContentParts_StringForm(t *testing.T) {
	m := OAIChatMessage{Content: json.RawMessage(`"hello"`)}
	parts, err := m.ContentParts()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(parts) != 1 || parts[0].Text != "hello" || !parts[0].isText() {
		t.Errorf("unexpected parts: %+v", parts)
	}
}

func TestContentParts_EmptyContent(t *testing.T) {
	var m OAIChatMessage
	parts, err := m.ContentParts()
	if err != nil || parts != nil {
		t.Errorf("expected (nil,nil) for empty content; got (%v,%v)", parts, err)
	}
}

func TestContentParts_EmptyStringContent(t *testing.T) {
	m := OAIChatMessage{Content: json.RawMessage(`""`)}
	parts, err := m.ContentParts()
	if err != nil || parts != nil {
		t.Errorf("expected (nil,nil) for \"\"; got (%v,%v)", parts, err)
	}
}

func TestContentParts_TextAndImage(t *testing.T) {
	body := fmt.Sprintf(
		`[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"%s"}}]`,
		dataURL("image/png", pngMagic),
	)
	m := OAIChatMessage{Content: json.RawMessage(body)}
	parts, err := m.ContentParts()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d (%+v)", len(parts), parts)
	}
	if !parts[0].isText() || parts[0].Text != "hi" {
		t.Errorf("part0 text mismatch: %+v", parts[0])
	}
	if parts[1].isText() || parts[1].MIME != "image/png" || !bytes.Equal(parts[1].Data, pngMagic) {
		t.Errorf("part1 image mismatch: %+v", parts[1])
	}
}

func TestContentParts_PreservesOrder(t *testing.T) {
	// Two images sandwiching some text — order MUST be preserved so the
	// model sees images in the same sequence the client supplied.
	body := fmt.Sprintf(`[
		{"type":"image_url","image_url":{"url":"%s"}},
		{"type":"text","text":"between"},
		{"type":"image_url","image_url":{"url":"%s"}}
	]`, dataURL("image/png", pngMagic), dataURL("image/jpeg", jpegMagic))
	m := OAIChatMessage{Content: json.RawMessage(body)}
	parts, err := m.ContentParts()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	if parts[0].MIME != "image/png" || parts[1].Text != "between" || parts[2].MIME != "image/jpeg" {
		t.Errorf("order not preserved: %+v", parts)
	}
}

func TestContentParts_RejectsMissingImageURL(t *testing.T) {
	body := `[{"type":"image_url","image_url":{"url":""}}]`
	m := OAIChatMessage{Content: json.RawMessage(body)}
	_, err := m.ContentParts()
	if err == nil {
		t.Fatal("expected error for empty url; got nil")
	}
}

func TestContentParts_SkipsUnknownTypes(t *testing.T) {
	body := `[
		{"type":"text","text":"keep"},
		{"type":"input_audio","input_audio":{"data":"..."}},
		{"type":"text","text":"alsokeep"}
	]`
	m := OAIChatMessage{Content: json.RawMessage(body)}
	parts, err := m.ContentParts()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(parts) != 2 || parts[0].Text != "keep" || parts[1].Text != "alsokeep" {
		t.Errorf("unknown-type parts not skipped cleanly: %+v", parts)
	}
}

func TestContentParts_DefaultTypeIsText(t *testing.T) {
	// Some clients omit `type` on text parts.
	body := `[{"text":"bare"}]`
	m := OAIChatMessage{Content: json.RawMessage(body)}
	parts, err := m.ContentParts()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(parts) != 1 || parts[0].Text != "bare" {
		t.Errorf("default-type text not handled: %+v", parts)
	}
}

func TestContentParts_RejectsRemoteImageURL(t *testing.T) {
	body := `[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]`
	m := OAIChatMessage{Content: json.RawMessage(body)}
	_, err := m.ContentParts()
	if err == nil {
		t.Fatal("expected SSRF guard to reject remote URL; got nil")
	}
}

func TestDecodeOllamaImage_AllFormats(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		mime string
	}{
		{"png", pngMagic, "image/png"},
		{"jpeg", jpegMagic, "image/jpeg"},
		{"webp", webpMagic, "image/webp"},
		{"gif", gif89aMag, "image/gif"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, data, err := decodeOllamaImage(base64.StdEncoding.EncodeToString(tc.raw))
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if mime != tc.mime {
				t.Errorf("mime = %q, want %q", mime, tc.mime)
			}
			if !bytes.Equal(data, tc.raw) {
				t.Errorf("data round-trip mismatch")
			}
		})
	}
}

func TestDecodeOllamaImage_RejectsUnknownMagic(t *testing.T) {
	_, _, err := decodeOllamaImage(base64.StdEncoding.EncodeToString([]byte("not an image")))
	if err == nil {
		t.Fatal("expected magic-byte rejection; got nil")
	}
}

func TestDecodeOllamaImage_AcceptsDataURLForm(t *testing.T) {
	// Tolerance for clients that send the OAI-style data: URL on the
	// Ollama-native slot.
	mime, data, err := decodeOllamaImage(dataURL("image/png", pngMagic))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if mime != "image/png" || !bytes.Equal(data, pngMagic) {
		t.Errorf("data-URL form not handled cleanly")
	}
}

func TestDecodeOllamaImage_EmptyInputRejected(t *testing.T) {
	_, _, err := decodeOllamaImage("")
	if err == nil {
		t.Fatal("expected error for empty input; got nil")
	}
}

func TestMediaPartsToGenai(t *testing.T) {
	in := []mediaPart{
		{Text: "hi"},
		{MIME: "image/png", Data: pngMagic},
		{Text: ""}, // empty text part: must be dropped
	}
	out := mediaPartsToGenai(in)
	if len(out) != 2 {
		t.Fatalf("want 2 parts, got %d", len(out))
	}
	if out[0].Text != "hi" {
		t.Errorf("out[0].Text = %q, want hi", out[0].Text)
	}
	if out[1].InlineData == nil ||
		out[1].InlineData.MIMEType != "image/png" ||
		!bytes.Equal(out[1].InlineData.Data, pngMagic) {
		t.Errorf("out[1] image part malformed: %+v", out[1])
	}
}

func TestConcatText_OnlyTextParts(t *testing.T) {
	in := []mediaPart{
		{Text: "one"},
		{MIME: "image/png", Data: pngMagic},
		{Text: "two"},
	}
	if got := concatText(in); got != "one\ntwo" {
		t.Errorf("got %q, want %q", got, "one\ntwo")
	}
}

func TestTotalImageBytes(t *testing.T) {
	in := []mediaPart{
		{Text: "ignored"},
		{MIME: "image/png", Data: make([]byte, 100)},
		{MIME: "image/jpeg", Data: make([]byte, 200)},
	}
	if got := totalImageBytes(in); got != 300 {
		t.Errorf("got %d, want 300", got)
	}
}

func TestEnvIntDefault(t *testing.T) {
	const k = "GW_TEST_INT_DEFAULT_UNUSED_KEY"
	t.Setenv(k, "")
	if got := envIntDefault(k, 42); got != 42 {
		t.Errorf("unset env: got %d, want 42", got)
	}
	t.Setenv(k, "100")
	if got := envIntDefault(k, 42); got != 100 {
		t.Errorf("set env: got %d, want 100", got)
	}
	t.Setenv(k, "garbage")
	if got := envIntDefault(k, 42); got != 42 {
		t.Errorf("garbage env: got %d, want 42 (fallback)", got)
	}
	t.Setenv(k, "-1")
	if got := envIntDefault(k, 42); got != 42 {
		t.Errorf("negative env: got %d, want 42 (fallback)", got)
	}
}
