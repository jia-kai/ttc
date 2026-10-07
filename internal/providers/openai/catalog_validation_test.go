package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"ttc/internal/llm"
)

func TestValidateCatalogChecksAllMetadata(t *testing.T) {
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"slug":"base","visibility":"list","context_window":100000,"service_tiers":[{"id":"priority","name":"Fast"}]}]}`)
	})
	original, err := a.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func([]llm.ModelInfo) []llm.ModelInfo
	}{
		{"empty", func(m []llm.ModelInfo) []llm.ModelInfo { return nil }},
		{"duplicate ID", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].ID = m[0].ID; return m }},
		{"invalid ID", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].ID = "bad\n"; return m }},
		{"capacity", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].Limits.ContextLimit = 0; return m }},
		{"output capacity", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].Limits.MaxOutputTokens = -1; return m }},
		{"output exceeds context", func(m []llm.ModelInfo) []llm.ModelInfo {
			m[0].Limits.MaxOutputTokens = m[0].Limits.ContextLimit + 1
			return m
		}},
		{"unsafe name", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].Name = "name\n"; return m }},
		{"unsafe description", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].Description = "description\u0085"; return m }},
		{"unsafe revision", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].Revision = string([]byte{0xff}); return m }},
		{"unsafe variant description", func(m []llm.ModelInfo) []llm.ModelInfo {
			m[0].VariantDescriptions = map[string]string{"none": "description\n"}
			return m
		}},
		{"duplicate variant", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].Variants = []string{"none", "none"}; return m }},
		{"default variant", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].DefaultVariant = "missing"; return m }},
		{"description variant", func(m []llm.ModelInfo) []llm.ModelInfo {
			m[0].VariantDescriptions = map[string]string{"missing": "desc"}
			return m
		}},
		{"reasoning flag", func(m []llm.ModelInfo) []llm.ModelInfo {
			m[0].Variants = []string{"low"}
			m[0].DefaultVariant = "low"
			return m
		}},
		{"document MIME", func(m []llm.ModelInfo) []llm.ModelInfo {
			m[0].BinaryFiles[0].MIMEType = "text/plain"
			return m
		}},
		{"binary limit", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].BinaryFiles[0].MaxBytes = 0; return m }},
		{"document list", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].BinaryFiles = m[0].BinaryFiles[1:]; return m }},
		{"document positive limit", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].BinaryFiles[0].MaxBytes--; return m }},
		{"tier without base", func(m []llm.ModelInfo) []llm.ModelInfo { m[0].ServiceTier = "fast"; return m }},
		{"missing base", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].BaseID = "missing"; return m }},
		{"invalid tier", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].ServiceTier = "unknown"; return m }},
		{"fast capacity", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].Limits.ContextLimit--; return m }},
		{"fast revision", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].Revision = "other"; return m }},
		{"fast variants", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].SupportsReasoning = true; return m }},
		{"fast images", func(m []llm.ModelInfo) []llm.ModelInfo { m[1].Images = !m[0].Images; return m }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var models []llm.ModelInfo
			if err := json.Unmarshal(raw, &models); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCatalog(tc.change(models)); err == nil {
				t.Fatal("accepted invalid provider metadata")
			}
		})
	}
}

func TestModelsRejectsUnsafeMetadataAtProtocolBoundary(t *testing.T) {
	for _, metadata := range []string{
		`"display_name":"unsafe\nname"`,
		`"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low","description":"unsafe\u0085description"}]`,
		`"service_tiers":[{"id":"priority","name":"Fast","description":"unsafe\n"}]`,
	} {
		t.Run(metadata, func(t *testing.T) {
			a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"models":[{"slug":"base","visibility":"list","context_window":100000,%s}]}`, metadata)
			})
			if models, err := a.Models(context.Background()); err == nil {
				t.Fatalf("protocol boundary accepted unsafe metadata: %#v", models)
			}
		})
	}
}
