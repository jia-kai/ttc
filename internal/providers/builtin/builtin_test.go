package builtin

import "testing"

func TestModulesContainsOnlyOpenAIAndReturnsFreshSlice(t *testing.T) {
	modules := Modules()
	if len(modules) != 1 || modules[0].ID != "openai" || modules[0].Configure == nil {
		t.Fatal(modules)
	}
	modules[0].ID = "mutated"
	if Modules()[0].ID != "openai" {
		t.Fatal("shared builtin slice")
	}
}
