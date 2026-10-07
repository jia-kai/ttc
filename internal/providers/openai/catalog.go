package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"time"
	"ttc/internal/llm"
	"unicode/utf8"
)

// CatalogVersion negotiates the verified Codex catalog contract, not TTC's release.
// The backend excludes ordinary models for older versions even with valid credentials.
const CatalogVersion = "0.159.0"
const catalogMaxAttempts = 3
const maxCatalogBytes = 4 << 20

// Models fetches endpoint metadata with at most three protocol attempts for
// transport errors, HTTP 429 or 5xx. The caller owns deadlines and persistence.
// Capacities and capabilities are validated without adding application budgets.
func (a *Adapter) Models(ctx context.Context) ([]llm.ModelInfo, error) {
	tokens, err := a.credentials(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", a.BaseURL+"/models?client_version="+CatalogVersion, nil)
	if err != nil {
		return nil, err
	}
	if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
		return nil, errors.New("subscription endpoint requires an HTTP(S) URL with a host")
	}
	headers(req, tokens)
	var resp *http.Response
	for attempt := 0; attempt < catalogMaxAttempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		resp, err = a.Client.Do(req.Clone(ctx))
		header := ""
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if invalidCertificate(err) {
				return nil, errors.New("subscription TLS certificate verification failed")
			}
			err = fmt.Errorf("model catalog transport failed (attempt %d/%d): %w", attempt+1, catalogMaxAttempts, err)
		} else if resp.StatusCode == http.StatusOK {
			break
		} else {
			status := resp.StatusCode
			header = resp.Header.Get("Retry-After")
			resp.Body.Close()
			err = fmt.Errorf("model catalog HTTP %d (attempt %d/%d)", status, attempt+1, catalogMaxAttempts)
			if status != 429 && (status < 500 || status > 599) {
				return nil, err
			}
		}
		if attempt == catalogMaxAttempts-1 {
			return nil, err
		}
		if err = wait(ctx, retryDelay(attempt, header, time.Now())); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Name       string `json:"display_name"`
			Default    string `json:"default_reasoning_level"`
			Visibility string `json:"visibility"`
			Priority   int    `json:"priority"`
			Levels     []struct {
				Effort      string `json:"effort"`
				Description string `json:"description"`
			} `json:"supported_reasoning_levels"`
			Context    int                                      `json:"context_window"`
			MaxContext int                                      `json:"max_context_window"`
			Effective  int                                      `json:"effective_context_window_percent"`
			Modalities []string                                 `json:"input_modalities"`
			Tiers      []struct{ ID, Name, Description string } `json:"service_tiers"`
		} `json:"models"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > maxCatalogBytes {
		return nil, errors.New("subscription model catalog exceeds 4 MiB")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("subscription model catalog must contain UTF-8 JSON")
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return nil, err
	}
	sort.SliceStable(catalog.Models, func(i, j int) bool { return catalog.Models[i].Priority < catalog.Models[j].Priority })
	out := []llm.ModelInfo{}
	for _, m := range catalog.Models {
		if m.Visibility == "hide" {
			continue
		}
		if m.Visibility != "list" {
			return nil, fmt.Errorf("model %q has invalid catalog visibility %q", m.Slug, m.Visibility)
		}
		capacity := m.Context
		if capacity == 0 {
			capacity = m.MaxContext
		}
		if m.Effective > 0 && m.Effective <= 100 {
			capacity = capacity/100*m.Effective + capacity%100*m.Effective/100
		}
		if capacity <= 0 {
			continue
		}
		variants := []string{}
		descriptions := map[string]string{}
		for _, v := range m.Levels {
			variants = append(variants, v.Effort)
			descriptions[v.Effort] = v.Description
		}
		if len(variants) == 0 {
			variants = []string{"none"}
			m.Default = "none"
		}
		images := false
		for _, mode := range m.Modalities {
			images = images || mode == "image"
		}
		model := llm.ModelInfo{ID: m.Slug, Name: m.Name, Variants: variants, VariantDescriptions: descriptions, DefaultVariant: m.Default, Images: images, BinaryFiles: documentFileTypes(), SupportsReasoning: len(m.Levels) > 0, Limits: llm.ModelLimits{ContextLimit: capacity}, Revision: resp.Header.Get("ETag")}
		out = append(out, model)
		for _, tier := range m.Tiers {
			if tier.Name != "Fast" {
				continue
			}
			if tier.ID != "priority" && tier.ID != "fast" {
				return nil, fmt.Errorf("model %q has unsupported Fast tier %q", m.Slug, tier.ID)
			}
			out[len(out)-1].Name = m.Name + " (Standard)"
			fast := model
			fast.ID, fast.BaseID = m.Slug+"/fast", m.Slug
			fast.Name, fast.ServiceTier, fast.Description = m.Name+" (Fast)", tier.ID, tier.Description
			out = append(out, fast)
		}
	}
	if err = ValidateCatalog(out); err != nil {
		return nil, fmt.Errorf("invalid subscription model metadata: %w", err)
	}
	return out, nil
}

// ValidateCatalog validates every selectable provider choice, including binary
// formats and Fast/base relationships. It performs no I/O or application policy.
func ValidateCatalog(models []llm.ModelInfo) error {
	if err := llm.ValidateCatalog(models); err != nil {
		return err
	}
	formats := documentFileTypes()
	byID := make(map[string]llm.ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}
	for _, m := range models {
		if !m.SupportsReasoning && (len(m.Variants) != 1 || m.Variants[0] != "none") {
			return fmt.Errorf("model %q has reasoning variants without reasoning support", m.ID)
		}
		if !reflect.DeepEqual(m.BinaryFiles, formats) {
			return fmt.Errorf("model %q has invalid document formats", m.ID)
		}
		if m.BaseID == "" {
			if m.ServiceTier != "" {
				return fmt.Errorf("model %q has a tier without a base model", m.ID)
			}
		} else {
			base, ok := byID[m.BaseID]
			if !ok || base.BaseID != "" || m.ID != m.BaseID+"/fast" || (m.ServiceTier != "fast" && m.ServiceTier != "priority") {
				return fmt.Errorf("model %q has invalid base model or service tier", m.ID)
			}
			if m.Limits != base.Limits || m.DefaultVariant != base.DefaultVariant || m.Images != base.Images || m.SupportsReasoning != base.SupportsReasoning || m.Revision != base.Revision || !reflect.DeepEqual(m.Variants, base.Variants) || !reflect.DeepEqual(m.VariantDescriptions, base.VariantDescriptions) {
				return fmt.Errorf("model %q disagrees with its base model", m.ID)
			}
		}
	}
	return nil
}
