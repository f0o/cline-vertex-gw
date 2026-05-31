package provider

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// Multimodal end-to-end provider-layer tests: we feed *genai.Content
// containing InlineData parts into each adapter's request builder and
// assert on the resulting wire body.

var fakePNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0xDE, 0xAD, 0xBE, 0xEF}

func mkImageContent(role string, mime string, data []byte) *genai.Content {
	return &genai.Content{
		Role: role,
		Parts: []*genai.Part{
			{Text: "what is this"},
			{InlineData: &genai.Blob{MIMEType: mime, Data: data}},
		},
	}
}

// --- Anthropic image block translation --------------------------------------

func TestBuildAnthropicRequest_ImageBlock(t *testing.T) {
	contents := []*genai.Content{mkImageContent(genai.RoleUser, "image/png", fakePNG)}
	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, nil, false)
	if len(req.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(req.Messages))
	}
	msg := req.Messages[0]
	// Multimodal forces the structured block path.
	if len(msg.Blocks) != 2 {
		t.Fatalf("want 2 blocks (text+image), got %d (%+v)", len(msg.Blocks), msg.Blocks)
	}
	if msg.Blocks[0].Type != "text" || msg.Blocks[0].Text != "what is this" {
		t.Errorf("text block malformed: %+v", msg.Blocks[0])
	}
	img := msg.Blocks[1]
	if img.Type != "image" {
		t.Fatalf("expected image block, got type=%q", img.Type)
	}
	if img.Source == nil {
		t.Fatal("image block missing Source")
	}
	if img.Source.Type != "base64" {
		t.Errorf("Source.Type = %q; want base64", img.Source.Type)
	}
	if img.Source.MediaType != "image/png" {
		t.Errorf("Source.MediaType = %q; want image/png", img.Source.MediaType)
	}
	want := base64.StdEncoding.EncodeToString(fakePNG)
	if img.Source.Data != want {
		t.Errorf("Source.Data round-trip mismatch")
	}
}

func TestBuildAnthropicRequest_ImageBlock_MarshalsCorrectly(t *testing.T) {
	// Wire-level check: the JSON the gateway actually sends to Anthropic
	// has to match {type, source:{type, media_type, data}}.
	contents := []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: []byte{0xFF, 0xD8, 0xFF}}},
		},
	}}
	req := buildAnthropicRequest("claude-3-5-sonnet", "", contents, nil, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"type":"image"`) {
		t.Errorf("wire body missing image block type: %s", s)
	}
	if !strings.Contains(s, `"media_type":"image/jpeg"`) {
		t.Errorf("wire body missing media_type: %s", s)
	}
	if !strings.Contains(s, `"source":{`) {
		t.Errorf("wire body missing nested source object: %s", s)
	}
}

// --- OpenAI-compat image_url translation ------------------------------------

func TestBuildOpenAIRequest_ImageURLPart(t *testing.T) {
	contents := []*genai.Content{mkImageContent(genai.RoleUser, "image/png", fakePNG)}
	req := buildOpenAIRequest("llama-3.2-90b-vision-instruct-maas", "", contents, nil, false)
	if len(req.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(req.Messages))
	}
	msg := req.Messages[0]
	// Multimodal turn → ContentParts populated, Content empty.
	if msg.Content != "" {
		t.Errorf("Content should be empty when ContentParts populated; got %q", msg.Content)
	}
	if len(msg.ContentParts) != 2 {
		t.Fatalf("want 2 content parts, got %d", len(msg.ContentParts))
	}
	if msg.ContentParts[0].Type != "text" || msg.ContentParts[0].Text != "what is this" {
		t.Errorf("text part malformed: %+v", msg.ContentParts[0])
	}
	img := msg.ContentParts[1]
	if img.Type != "image_url" || img.ImageURL == nil {
		t.Fatalf("expected image_url part, got %+v", img)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(fakePNG)
	if img.ImageURL.URL != wantURL {
		t.Errorf("image url mismatch")
	}
}

func TestBuildOpenAIRequest_TextOnlyUsesBareString(t *testing.T) {
	// The polymorphic Content path should ONLY use the parts array when
	// images are present — text-only turns retain the legacy bare-string
	// shape for compat with older OpenAI-compatible MaaS implementations
	// that haven't seen the parts array.
	contents := []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: "no image"}},
	}}
	req := buildOpenAIRequest("llama-3.3-70b-instruct-maas", "", contents, nil, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"content":"no image"`) {
		t.Errorf("text-only message should serialize as bare string; got: %s", s)
	}
	if strings.Contains(s, `"image_url"`) || strings.Contains(s, `"type":"text"`) {
		t.Errorf("text-only message accidentally used parts-array shape: %s", s)
	}
}

func TestBuildOpenAIRequest_ImageOnlyUsesPartsArray(t *testing.T) {
	contents := []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}},
		},
	}}
	req := buildOpenAIRequest("pixtral-12b", "", contents, nil, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"content":[`) {
		t.Errorf("multimodal message should use parts-array; got: %s", s)
	}
	if !strings.Contains(s, `"image_url"`) {
		t.Errorf("expected image_url part in wire body: %s", s)
	}
}

func TestBuildOpenAIRequest_PreservesOrderAcrossTextAndImage(t *testing.T) {
	contents := []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}},
			{Text: "between"},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: []byte{0xFF, 0xD8, 0xFF}}},
		},
	}}
	req := buildOpenAIRequest("pixtral-12b", "", contents, nil, false)
	msg := req.Messages[0]
	if len(msg.ContentParts) != 3 {
		t.Fatalf("want 3 content parts, got %d", len(msg.ContentParts))
	}
	if msg.ContentParts[0].Type != "image_url" ||
		msg.ContentParts[1].Type != "text" ||
		msg.ContentParts[2].Type != "image_url" {
		t.Errorf("order not preserved: %+v", msg.ContentParts)
	}
}
