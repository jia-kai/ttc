// Package catalog owns provider-neutral model storage and discovery lifecycle.
package catalog

import (
	"errors"

	"ttc/internal/llm"
)

// Policy contains TTC token reserves, in tokens, rather than remote capacities.
// A zero Policy selects DefaultPolicy. Invalid policies are rejected by Open.
type Policy struct {
	OutputAllowance        int
	EstimationMargin       int
	RecentTokensMin        int
	RecentTokensMax        int
	NextTurnInputReserve   int
	SummaryOutputAllowance int
}

// DefaultPolicy returns TTC's context and compaction reserves.
func DefaultPolicy() Policy { return Policy{4096, 4096, 4096, 16000, 4096, 2048} }

func (p Policy) validate() error {
	if p.OutputAllowance <= 0 || p.EstimationMargin < 0 || p.RecentTokensMin < 0 || p.RecentTokensMax <= 0 || p.RecentTokensMin > p.RecentTokensMax || p.NextTurnInputReserve < 0 || p.SummaryOutputAllowance <= 0 {
		return errors.New("invalid catalog token policy")
	}
	return nil
}

func validate(models []llm.ModelInfo, extra func([]llm.ModelInfo) error) error {
	if err := llm.ValidateCatalog(models); err != nil {
		return err
	}
	if extra != nil {
		return extra(models)
	}
	return nil
}

// prepare omits models whose capacities cannot accommodate the application
// reserves, and fails if none remain. Published output ceilings clamp both
// ordinary and summary reserves; no ceiling is invented when it is unknown.
func prepare(models []llm.ModelInfo, p Policy) ([]llm.ModelSpec, error) {
	out := make([]llm.ModelSpec, 0, len(models))
	for _, m := range models {
		b := llm.Budget{ContextLimit: m.Limits.ContextLimit, MaxOutputTokens: m.Limits.MaxOutputTokens, OutputAllowance: p.OutputAllowance, EstimationMargin: p.EstimationMargin, RecentTokensMin: p.RecentTokensMin, RecentTokensMax: p.RecentTokensMax, NextTurnInputReserve: p.NextTurnInputReserve, SummaryOutputAllowance: p.SummaryOutputAllowance}
		if b.MaxOutputTokens > 0 {
			b.OutputAllowance = min(b.OutputAllowance, b.MaxOutputTokens)
			b.SummaryOutputAllowance = min(b.SummaryOutputAllowance, b.MaxOutputTokens)
		}
		if b.Validate() != nil {
			continue
		}
		out = append(out, llm.ModelSpec{ID: m.ID, BaseID: m.BaseID, ServiceTier: m.ServiceTier, Description: m.Description, Name: m.Name, Variants: append([]string(nil), m.Variants...), VariantDescriptions: cloneMap(m.VariantDescriptions), DefaultVariant: m.DefaultVariant, Images: m.Images, BinaryFiles: cloneFormats(m.BinaryFiles), SupportsReasoning: m.SupportsReasoning, Budget: b, Revision: m.Revision})
	}
	if len(out) == 0 {
		return nil, errors.New("catalog has no models with capacity for application reserves")
	}
	return out, nil
}
func cloneFormats(in []llm.BinaryFileType) []llm.BinaryFileType {
	if in == nil {
		return nil
	}
	out := append([]llm.BinaryFileType(nil), in...)
	for i := range out {
		out[i].Extensions = append([]string(nil), in[i].Extensions...)
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
