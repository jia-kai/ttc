package llm

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ModelLimits contains only endpoint-announced capacities, in tokens. A zero
// MaxOutputTokens means no verified output ceiling; it is not a generation cap.
type ModelLimits struct {
	ContextLimit    int `json:"context_limit"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

// ModelInfo is remote catalog metadata without TTC context/compaction policy.
// Its slices and maps are immutable after publication. Catalog lifecycle owners
// turn this into a ModelSpec by applying application policy; protocol adapters
// never select defaults, save preferences or create cache files.
type ModelInfo struct {
	ID                  string            `json:"id"`
	BaseID              string            `json:"base_id,omitempty"`
	ServiceTier         string            `json:"service_tier,omitempty"`
	Description         string            `json:"description,omitempty"`
	Name                string            `json:"name"`
	Variants            []string          `json:"variants"`
	VariantDescriptions map[string]string `json:"variant_descriptions,omitempty"`
	DefaultVariant      string            `json:"default_variant"`
	Images              bool              `json:"images"`
	BinaryFiles         []BinaryFileType  `json:"binary_files"`
	SupportsReasoning   bool              `json:"supports_reasoning"`
	Limits              ModelLimits       `json:"limits"`
	Revision            string            `json:"revision"`
}

// ValidateCatalog checks provider-neutral metadata without I/O, mutation or
// application policy. Catalogs must contain unique, safe choices with positive
// context capacities; an unknown output ceiling is zero, otherwise it must not
// exceed context. Protocol adapters may add provider-specific constraints.
func ValidateCatalog(models []ModelInfo) error {
	if len(models) == 0 {
		return errors.New("catalog contains no selectable models")
	}
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		if err := ValidateChoiceText("model ID", m.ID, MaxModelIDBytes); err != nil {
			return err
		}
		if ids[m.ID] {
			return fmt.Errorf("duplicate catalog choice %q", m.ID)
		}
		ids[m.ID] = true
		if m.Limits.ContextLimit <= 0 || m.Limits.MaxOutputTokens < 0 || m.Limits.MaxOutputTokens > m.Limits.ContextLimit {
			return fmt.Errorf("model %q has invalid capacities", m.ID)
		}
		variants := make(map[string]bool, len(m.Variants))
		for _, v := range m.Variants {
			if err := ValidateChoiceText("variant", v, MaxVariantBytes); err != nil {
				return fmt.Errorf("model %q: %w", m.ID, err)
			}
			if variants[v] {
				return fmt.Errorf("model %q has duplicate variant", m.ID)
			}
			variants[v] = true
		}
		if !variants[m.DefaultVariant] {
			return fmt.Errorf("model %q has unknown default variant", m.ID)
		}
		for v, d := range m.VariantDescriptions {
			if !variants[v] {
				return fmt.Errorf("model %q describes unknown variant", m.ID)
			}
			if !safeCatalogText(d) {
				return fmt.Errorf("model %q has invalid description", m.ID)
			}
		}
		for _, f := range m.BinaryFiles {
			if !ValidBinaryMIME(f.MIMEType) {
				return fmt.Errorf("model %q has invalid binary MIME type", m.ID)
			}
			if !safeCatalogText(f.MIMEType) || f.MaxBytes <= 0 || (f.Kind != "image" && f.Kind != "document") || len(f.Extensions) == 0 {
				return fmt.Errorf("model %q has invalid binary format", m.ID)
			}
			for _, ext := range f.Extensions {
				if len(ext) < 2 || ext[0] != '.' || !safeCatalogText(ext) || strings.ContainsAny(ext, "/\\") || strings.TrimSpace(ext) != ext {
					return fmt.Errorf("model %q has invalid binary extension", m.ID)
				}
			}
		}
		for _, text := range []string{m.Name, m.Description, m.Revision, m.BaseID, m.ServiceTier} {
			if !safeCatalogText(text) {
				return fmt.Errorf("model %q has invalid text", m.ID)
			}
		}
	}
	return nil
}

func safeCatalogText(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// BinaryPayload is one checksum-verified original prepared by an application
// resolver. Data contains original encoded bytes, not base64 or a data URL.
// Callers transfer immutable ownership for request encoding; MIMEType is canonical.
type BinaryPayload struct {
	Data     []byte
	MIMEType string
}
