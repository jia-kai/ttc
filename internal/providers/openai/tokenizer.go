package openai

import (
	"regexp"

	"ttc/internal/llm"
)

var tokenizerFamily = regexp.MustCompile(`^(gpt-[0-9]+(?:\.[0-9]+)*)-(?:sol|luna|terra|astra)$`)

// TokenizerID returns the tokenizer calibration identity for the underlying
// request model. Recognized numeric GPT versions share an identity across sol,
// luna, terra and astra by TTC policy; all other IDs retain distinct identities.
// Tags keep unknown IDs disjoint from shared families. Service tier and reasoning
// compatibility are caller-owned and are not represented by this identity.
func (a *Adapter) TokenizerID(model llm.ModelSpec) string {
	id := model.RequestID()
	if match := tokenizerFamily.FindStringSubmatch(id); match != nil {
		return "version:" + match[1]
	}
	if id == "" {
		return ""
	}
	return "model:" + id
}
