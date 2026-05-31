package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// End-to-end handler-layer tests for the multimodal paths: assert that
// buildContents / buildContentsOAI produce the right InlineData parts and
// surface decode errors for the caller to map to 400s.

// --- Ollama-side `images` array support ------------------------------------

func TestBuildContents_OllamaImagesField(t *testing.T) {
	msgs := []Message{{
		Role:    "user",
		Content: "what is this",
		Images:  []string{base64.StdEncoding.EncodeToString(pngMagic)},
	}}
	contents, sys, err := buildContents(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sys != "" {
		t.Errorf("unexpected system prompt: %q", sys)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("want 1 turn with 2 parts; got contents=%+v", contents)
	}
	if contents[0].Parts[0].Text != "what is this" {
		t.Errorf("text part not preserved: %+v", contents[0].Parts[0])
	}
	img := contents[0].Parts[1]
	if img.InlineData == nil || img.InlineData.MIMEType != "image/png" {
		t.Errorf("image part malformed: %+v", img)
	}
}

func TestBuildContents_OllamaImagesField_BadBytes(t *testing.T) {
	msgs := []Message{{
		Role:    "user",
		Content: "what is this",
		Images:  []string{base64.StdEncoding.EncodeToString([]byte("not an image"))},
	}}
	_, _, err := buildContents(msgs)
	if err == nil {
		t.Fatal("expected error for non-image bytes")
	}
	if !strings.Contains(err.Error(), "message 0 image 0") {
		t.Errorf("error should point at the failing message+image index: %v", err)
	}
}

func TestBuildContents_OllamaImagesField_RespectsPerRequestCap(t *testing.T) {
	prev := maxMediaBytesPerRequest
	maxMediaBytesPerRequest = 5 // ridiculously tight for the test
	defer func() { maxMediaBytesPerRequest = prev }()
	msgs := []Message{{
		Role:    "user",
		Content: "x",
		Images:  []string{base64.StdEncoding.EncodeToString(pngMagic)},
	}}
	_, _, err := buildContents(msgs)
	if err == nil {
		t.Fatal("expected per-request cap rejection")
	}
	if !strings.Contains(err.Error(), "GW_MAX_MEDIA_BYTES_PER_REQUEST") {
		t.Errorf("error should reference the env knob: %v", err)
	}
}

func TestBuildContents_NoImagesStillWorks(t *testing.T) {
	// Regression: making sure the multimodal refactor didn't break the
	// classic text-only happy path.
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "how are you"},
	}
	contents, _, err := buildContents(msgs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(contents) != 3 {
		t.Errorf("want 3 contents, got %d", len(contents))
	}
}

// --- OpenAI surface: buildContentsOAI image decoding ----------------------

func TestBuildContentsOAI_ImageURLDataPart(t *testing.T) {
	body := fmt.Sprintf(`[
		{"type":"text","text":"what is this"},
		{"type":"image_url","image_url":{"url":"%s"}}
	]`, dataURL("image/png", pngMagic))
	msgs := []OAIChatMessage{{Role: "user", Content: json.RawMessage(body)}}
	contents, _, err := buildContentsOAI(msgs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 2 {
		t.Fatalf("want 1 turn with 2 parts; got %+v", contents)
	}
	if contents[0].Parts[0].Text != "what is this" {
		t.Errorf("text not preserved: %+v", contents[0].Parts[0])
	}
	if contents[0].Parts[1].InlineData == nil ||
		contents[0].Parts[1].InlineData.MIMEType != "image/png" {
		t.Errorf("image part malformed: %+v", contents[0].Parts[1])
	}
}

func TestBuildContentsOAI_BadImageReturnsError(t *testing.T) {
	body := `[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]`
	msgs := []OAIChatMessage{{Role: "user", Content: json.RawMessage(body)}}
	_, _, err := buildContentsOAI(msgs)
	if err == nil {
		t.Fatal("expected error for remote URL")
	}
	if !strings.Contains(err.Error(), "message 0") {
		t.Errorf("error should reference message index: %v", err)
	}
}

func TestBuildContentsOAI_PerRequestImageCap(t *testing.T) {
	prev := maxMediaBytesPerRequest
	maxMediaBytesPerRequest = 5
	defer func() { maxMediaBytesPerRequest = prev }()
	body := fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"%s"}}]`,
		dataURL("image/png", pngMagic),
	)
	msgs := []OAIChatMessage{{Role: "user", Content: json.RawMessage(body)}}
	_, _, err := buildContentsOAI(msgs)
	if err == nil {
		t.Fatal("expected per-request image cap rejection")
	}
	if !strings.Contains(err.Error(), "GW_MAX_MEDIA_BYTES_PER_REQUEST") {
		t.Errorf("error should reference env knob: %v", err)
	}
}

func TestBuildContentsOAI_SystemMessagesIgnoreImages(t *testing.T) {
	// Images on system messages are silently dropped (no upstream supports
	// them as system context). The text portion still flows into the
	// system prompt.
	body := fmt.Sprintf(`[
		{"type":"text","text":"be terse"},
		{"type":"image_url","image_url":{"url":"%s"}}
	]`, dataURL("image/png", pngMagic))
	msgs := []OAIChatMessage{
		{Role: "system", Content: json.RawMessage(body)},
		{Role: "user", Content: json.RawMessage(`"hi"`)},
	}
	contents, sys, err := buildContentsOAI(msgs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(sys, "be terse") {
		t.Errorf("system prompt missing text part: %q", sys)
	}
	if len(contents) != 1 {
		t.Fatalf("want 1 user turn, got %d", len(contents))
	}
	// System-side image silently dropped — only the user text remains.
	for _, p := range contents[0].Parts {
		if p.InlineData != nil {
			t.Errorf("image leaked from system to user contents: %+v", p)
		}
	}
}
