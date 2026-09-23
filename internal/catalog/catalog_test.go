package catalog

import "testing"

// Guards against offering model ids the current claude CLI has dropped.
// Verified live against claude 2.1.280 on 2026-09-22: `claude --model <id>
// --print "say ok"` succeeded for each of these.
func TestDefaultsOfferCurrentModelGeneration(t *testing.T) {
	want := []string{
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-fable-5-1",
		"claude-haiku-4-5-20251001",
	}

	store, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}

	got := map[string]bool{}
	for _, m := range store.Models() {
		got[m.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("default catalog missing model id %q", id)
		}
	}

	def := store.DefaultModel()
	if def.ID != "claude-opus-5" {
		t.Errorf("default model = %q, want claude-opus-5", def.ID)
	}
}

func TestResolveModelUnknownID(t *testing.T) {
	store, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if _, err := store.ResolveModel("claude-opus-4-8"); err == nil {
		t.Error("ResolveModel(retired id) = nil error, want error")
	}
}
