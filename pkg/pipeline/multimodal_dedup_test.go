package pipeline

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/genai"
)

var fakePNG = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0xDE, 0xAD, 0xBE, 0xEF}

// --- Image dedup ----------------------------------------------------------

func TestDedup_RepeatedImage(t *testing.T) {
	defer withDedup(t, true, 100)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{Text: "screenshot:"},
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}},
		}},
		mkTurn(genai.RoleModel, "ack"),
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{Text: "and now:"},
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}}, // same bytes
		}},
	}
	out := DedupReplayedBlocks(in)
	// First user's image stays verbatim.
	if out[0].Parts[1].InlineData == nil ||
		!bytes.Equal(out[0].Parts[1].InlineData.Data, fakePNG) {
		t.Errorf("first occurrence of image was modified")
	}
	// Third user's image should be replaced by a text-part placeholder.
	got := out[2].Parts[1]
	if got.InlineData != nil {
		t.Errorf("duplicate image not collapsed; still has InlineData")
	}
	if !strings.Contains(got.Text, "image elided") {
		t.Errorf("placeholder text mismatch: %q", got.Text)
	}
	if !strings.Contains(got.Text, "image/png") {
		t.Errorf("placeholder missing mime: %q", got.Text)
	}
	if !strings.Contains(got.Text, "turn 1") {
		t.Errorf("placeholder missing turn ref: %q", got.Text)
	}
}

func TestDedup_RoleScopedImage(t *testing.T) {
	defer withDedup(t, true, 100)()
	in := []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}},
		}},
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}},
		}},
	}
	out := DedupReplayedBlocks(in)
	// User turn's identical image must NOT dedup against the model turn.
	if out[1].Parts[0].InlineData == nil {
		t.Errorf("user image was incorrectly deduped against assistant image")
	}
}

func TestDedup_DifferentMIMEDoesNotCollide(t *testing.T) {
	defer withDedup(t, true, 100)()
	in := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: fakePNG}},
		}},
		mkTurn(genai.RoleModel, "ack"),
		{Role: genai.RoleUser, Parts: []*genai.Part{
			// Same bytes but different MIME — should NOT collapse.
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: fakePNG}},
		}},
	}
	out := DedupReplayedBlocks(in)
	if out[2].Parts[0].InlineData == nil {
		t.Errorf("differing-MIME image was incorrectly deduped")
	}
}

// --- Budget accounts for image bytes --------------------------------------

func TestContentBytes_CountsImageBytes(t *testing.T) {
	c := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{Text: "hello"}, // 5
			{InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, 1000)}},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: make([]byte, 500)}},
		},
	}
	if got := contentBytes(c); got != 5+1000+500 {
		t.Errorf("contentBytes = %d; want %d", got, 1505)
	}
}

func TestContentBytes_NilAndEmpty(t *testing.T) {
	if got := contentBytes(nil); got != 0 {
		t.Errorf("nil contentBytes = %d; want 0", got)
	}
	if got := contentBytes(&genai.Content{}); got != 0 {
		t.Errorf("empty contentBytes = %d; want 0", got)
	}
}
