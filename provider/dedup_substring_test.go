package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

func withSubstringDedup(t *testing.T, enabled bool, minBytes int32) func() {
	t.Helper()
	pe, pm := dedupSubstring, dedupSubstringMinBytes
	dedupSubstring, dedupSubstringMinBytes = enabled, minBytes
	return func() { dedupSubstring, dedupSubstringMinBytes = pe, pm }
}

func TestSubstringDedup_Disabled(t *testing.T) {
	defer withSubstringDedup(t, false, 100)()
	big := strings.Repeat("A", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "prefix "+big),
	}
	out := DedupSubstringBlocks(in)
	if &out[0] != &in[0] {
		t.Error("expected fast-path return when disabled")
	}
}

func TestSubstringDedup_EmbeddedBlock(t *testing.T) {
	defer withSubstringDedup(t, true, 100)()
	big := strings.Repeat("filecontent\n", 200) // large
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),                                  // turn 0 — needle, kept verbatim
		mkTurn(genai.RoleModel, "ack"),                               // turn 1
		mkTurn(genai.RoleUser, "Here it is again:\n"+big+"\nthanks"), // turn 2 — embeds needle
	}
	out := DedupSubstringBlocks(in)
	if out[0].Parts[0].Text != big {
		t.Error("first occurrence was modified")
	}
	got := out[2].Parts[0].Text
	if strings.Contains(got, big) {
		t.Error("embedded copy was not collapsed")
	}
	if !strings.Contains(got, "already shown in turn 1") {
		t.Errorf("placeholder missing back-pointer: %q", got)
	}
	if !strings.HasPrefix(got, "Here it is again:") || !strings.HasSuffix(got, "thanks") {
		t.Errorf("surrounding text not preserved: %q", got)
	}
}

func TestSubstringDedup_DifferentRoleNotMatched(t *testing.T) {
	defer withSubstringDedup(t, true, 100)()
	big := strings.Repeat("B", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleModel, big),        // assistant needle
		mkTurn(genai.RoleUser, "see: "+big), // user turn embeds it — must NOT collapse
	}
	out := DedupSubstringBlocks(in)
	if !strings.Contains(out[1].Parts[0].Text, big) {
		t.Error("cross-role collapse occurred (speaker mismatch)")
	}
}

func TestSubstringDedup_NoMutation(t *testing.T) {
	defer withSubstringDedup(t, true, 100)()
	big := strings.Repeat("C", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "x "+big),
	}
	orig := in[2].Parts[0].Text
	_ = DedupSubstringBlocks(in)
	if in[2].Parts[0].Text != orig {
		t.Error("input slice was mutated")
	}
}
