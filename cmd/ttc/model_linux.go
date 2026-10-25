package main

import (
	"fmt"
	"ttc/internal/llm"
)

// startupSelection resolves remembered IDs against current catalog metadata.
// Unavailable remembered choices fall back visibly to catalog defaults.
func startupSelection(providerID string, models []llm.ModelSpec, saved *llm.Selection) (llm.Selection, string, error) {
	if len(models) == 0 {
		return llm.Selection{}, "", fmt.Errorf("provider returned an empty model catalog")
	}
	modelID, variant, notice := models[0].ID, "", ""
	if saved != nil && saved.Provider == providerID {
		found := false
		for _, model := range models {
			if model.ID != saved.Model.ID {
				continue
			}
			found, modelID = true, model.ID
			for _, available := range model.Variants {
				if available == saved.Variant {
					variant = saved.Variant
					break
				}
			}
			if variant == "" && saved.Variant != "" {
				notice = "Saved reasoning variant unavailable; using the model default. Use /model to choose."
			}
			break
		}
		if !found {
			notice = "Saved model unavailable; using the catalog default. Use /model to choose."
		}
	}
	selection, err := llm.Resolve(providerID, models, modelID, variant)
	return selection, notice, err
}
