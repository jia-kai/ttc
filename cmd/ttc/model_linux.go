package main

import (
	"fmt"
	"ttc/internal/llm"
)

// startupSelection resolves saved IDs against current catalog metadata. CLI
// choices override saved values; unavailable saved choices get a visible notice.
func startupSelection(providerID string, models []llm.ModelSpec, saved *llm.Selection, modelID, variant string) (llm.Selection, string, error) {
	if len(models) == 0 {
		return llm.Selection{}, "", fmt.Errorf("provider returned an empty model catalog")
	}
	notice := ""
	if modelID == "" {
		modelID = models[0].ID
		if saved != nil && saved.Provider == providerID {
			found := false
			for _, model := range models {
				if model.ID != saved.Model.ID {
					continue
				}
				found, modelID = true, model.ID
				if variant == "" {
					for _, available := range model.Variants {
						if available == saved.Variant {
							variant = saved.Variant
							break
						}
					}
					if variant == "" && saved.Variant != "" {
						notice = "Saved reasoning variant unavailable; using the model default. Use /model to choose."
					}
				}
				break
			}
			if !found {
				notice = "Saved model unavailable; using the catalog default. Use /model to choose."
			}
		}
	}
	selection, err := llm.Resolve(providerID, models, modelID, variant)
	return selection, notice, err
}
