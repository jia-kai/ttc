package llm

import (
	"reflect"
	"strings"
	"testing"
)

func validCatalogModel() ModelInfo {
	return ModelInfo{ID: "model", Name: "Model", Variants: []string{"custom"}, DefaultVariant: "custom", VariantDescriptions: map[string]string{"custom": "Custom variant"}, Limits: ModelLimits{ContextLimit: 100}, BinaryFiles: []BinaryFileType{{MIMEType: "application/pdf", Kind: "document", MaxBytes: 1024, Extensions: []string{".pdf"}}}}
}

func TestValidateCatalog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ModelInfo)
	}{
		{"empty ID", func(m *ModelInfo) { m.ID = "" }},
		{"padded ID", func(m *ModelInfo) { m.ID = " model" }},
		{"oversized ID", func(m *ModelInfo) { m.ID = strings.Repeat("a", MaxModelIDBytes+1) }},
		{"invalid UTF-8 ID", func(m *ModelInfo) { m.ID = string([]byte{0xff}) }},
		{"control ID", func(m *ModelInfo) { m.ID = "model\u0085" }},
		{"zero context", func(m *ModelInfo) { m.Limits.ContextLimit = 0 }},
		{"negative context", func(m *ModelInfo) { m.Limits.ContextLimit = -1 }},
		{"negative output", func(m *ModelInfo) { m.Limits.MaxOutputTokens = -1 }},
		{"output exceeds context", func(m *ModelInfo) { m.Limits.MaxOutputTokens = 101 }},
		{"no variants", func(m *ModelInfo) { m.Variants = nil }},
		{"empty variant", func(m *ModelInfo) { m.Variants = []string{""} }},
		{"padded variant", func(m *ModelInfo) { m.Variants = []string{"custom "} }},
		{"oversized variant", func(m *ModelInfo) { m.Variants = []string{strings.Repeat("a", MaxVariantBytes+1)} }},
		{"invalid UTF-8 variant", func(m *ModelInfo) { m.Variants = []string{string([]byte{0xff})} }},
		{"control variant", func(m *ModelInfo) { m.Variants = []string{"custom\x00"} }},
		{"duplicate variant", func(m *ModelInfo) { m.Variants = []string{"custom", "custom"} }},
		{"unknown default", func(m *ModelInfo) { m.DefaultVariant = "missing" }},
		{"unknown description", func(m *ModelInfo) { m.VariantDescriptions["missing"] = "Description" }},
		{"unsafe description", func(m *ModelInfo) { m.VariantDescriptions["custom"] = "Description\n" }},
		{"invalid UTF-8 description", func(m *ModelInfo) { m.VariantDescriptions["custom"] = string([]byte{0xff}) }},
		{"empty MIME", func(m *ModelInfo) { m.BinaryFiles[0].MIMEType = "" }},
		{"noncanonical MIME", func(m *ModelInfo) { m.BinaryFiles[0].MIMEType = "Application/PDF" }},
		{"MIME parameters", func(m *ModelInfo) { m.BinaryFiles[0].MIMEType = "application/pdf; charset=utf-8" }},
		{"malformed MIME", func(m *ModelInfo) { m.BinaryFiles[0].MIMEType = "pdf" }},
		{"zero byte limit", func(m *ModelInfo) { m.BinaryFiles[0].MaxBytes = 0 }},
		{"negative byte limit", func(m *ModelInfo) { m.BinaryFiles[0].MaxBytes = -1 }},
		{"unknown kind", func(m *ModelInfo) { m.BinaryFiles[0].Kind = "audio" }},
		{"no extensions", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = nil }},
		{"short extension", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{"."} }},
		{"missing extension dot", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{"pdf"} }},
		{"extension path", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{".pdf/x"} }},
		{"extension backslash", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{".pdf\\x"} }},
		{"padded extension", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{".pdf "} }},
		{"unsafe extension", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{".pdf\n"} }},
		{"invalid UTF-8 extension", func(m *ModelInfo) { m.BinaryFiles[0].Extensions = []string{"." + string([]byte{0xff})} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := validCatalogModel()
			tc.change(&m)
			if err := ValidateCatalog([]ModelInfo{m}); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
	if err := ValidateCatalog(nil); err == nil {
		t.Fatal("accepted empty catalog")
	}
	m := validCatalogModel()
	if err := ValidateCatalog([]ModelInfo{m, m}); err == nil {
		t.Fatal("accepted duplicate model IDs")
	}
}

func TestValidateCatalogSafeText(t *testing.T) {
	for _, field := range []string{"name", "description", "revision", "base", "tier"} {
		for _, text := range []string{"unsafe\n", "unsafe\u0085", string([]byte{0xff})} {
			t.Run(field+"/"+text, func(t *testing.T) {
				m := validCatalogModel()
				switch field {
				case "name":
					m.Name = text
				case "description":
					m.Description = text
				case "revision":
					m.Revision = text
				case "base":
					m.BaseID = text
				case "tier":
					m.ServiceTier = text
				}
				if err := ValidateCatalog([]ModelInfo{m}); err == nil {
					t.Fatal("accepted unsafe text")
				}
			})
		}
	}
}

func TestValidateCatalogEffectFreeAndProviderNeutral(t *testing.T) {
	for _, ceiling := range []int{0, 1, 100} {
		models := []ModelInfo{validCatalogModel()}
		models[0].Limits.MaxOutputTokens = ceiling
		// Provider-specific variants, tiers and capabilities are not constrained here.
		models[0].BaseID, models[0].ServiceTier = "other", "custom"
		models[0].BinaryFiles = append(models[0].BinaryFiles, BinaryFileType{MIMEType: "image/png", Kind: "image", MaxBytes: 1024, Extensions: []string{".png"}})
		before := []ModelInfo{validCatalogModel()}
		before[0].Limits.MaxOutputTokens = ceiling
		before[0].BaseID, before[0].ServiceTier = "other", "custom"
		before[0].BinaryFiles = append(before[0].BinaryFiles, BinaryFileType{MIMEType: "image/png", Kind: "image", MaxBytes: 1024, Extensions: []string{".png"}})
		if err := ValidateCatalog(models); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(models, before) {
			t.Fatal("validation mutated metadata")
		}
	}
	m := validCatalogModel()
	m.BinaryFiles, m.VariantDescriptions = nil, nil
	if err := ValidateCatalog([]ModelInfo{m}); err != nil {
		t.Fatal("optional capabilities or descriptions required:", err)
	}
}
