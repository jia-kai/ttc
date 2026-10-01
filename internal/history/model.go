package history

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"scicode/internal/provider"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const modelChoicesBytes = 64 << 10

type modelChoice struct {
	ID      string `json:"id"`
	Variant string `json:"variant"`
}

type modelChoices struct {
	Version int                    `json:"version"`
	Choices map[string]modelChoice `json:"choices"`
}

// SwitchModel atomically records an applied selection and updates session metadata.
// It rejects a read-only session; callers must retain their old selection on error.
func (s *Store) SwitchModel(session, turn string, previous, next provider.Selection, text string) (int64, error) {
	model, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	content, err := json.Marshal(map[string]any{"type": "model_switch", "text": text, "previous": previous, "selection": next})
	if err != nil {
		return 0, err
	}
	var entry int64
	err = s.transact(func(tx *sql.Tx) error {
		result, err := tx.Exec("UPDATE sessions SET model_json=? WHERE id=? AND read_only=0", string(model), session)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("model switch requires a writable session")
		}
		entry, err = appendTx(tx, session, turn, "main", "status", "", false, content, 0)
		if err != nil {
			return err
		}
		return nil
	})
	return entry, err
}

// LastSelection reads the explicit choice saved for a provider, across workspaces.
// Nil means no saved choice. Only Provider, Model.ID and Variant are populated;
// callers must resolve current catalog metadata before using the selection.
func (s *Store) LastSelection(providerID string) (*provider.Selection, error) {
	if err := choiceText("provider", providerID, 128); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	choices, err := s.readModelChoices()
	if err != nil {
		return nil, err
	}
	choice, found := choices.Choices[providerID]
	if !found {
		return nil, nil
	}
	return &provider.Selection{Provider: providerID, Model: provider.ModelSpec{ID: choice.ID}, Variant: choice.Variant}, nil
}

// SaveSelection atomically saves an explicit model ID and variant in a private,
// versioned preference file, independently of session creation or model switches.
// Call before accepting a queued choice. Catalog metadata is never persisted.
// Invalid existing files fail rather than falling back to session history.
func (s *Store) SaveSelection(selection provider.Selection) error {
	if err := validateModelChoice(selection.Provider, modelChoice{selection.Model.ID, selection.Variant}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	choices, err := s.readModelChoices()
	if err != nil {
		return err
	}
	choices.Choices[selection.Provider] = modelChoice{selection.Model.ID, selection.Variant}
	data, err := json.Marshal(choices)
	if err != nil {
		return err
	}
	if len(data) > modelChoicesBytes {
		return errors.New("model preferences exceed 64 KiB")
	}
	if err := AtomicFile(filepath.Join(s.Root, "model-choices.json"), data, 0600); err != nil {
		return fmt.Errorf("save model preferences: %w", err)
	}
	return nil
}

// readModelChoices requires Store.mu, so read-modify-write saves cannot race.
func (s *Store) readModelChoices() (modelChoices, error) {
	path := filepath.Join(s.Root, "model-choices.json")
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return modelChoices{Version: 1, Choices: map[string]modelChoice{}}, nil
	}
	if err != nil {
		return modelChoices{}, fmt.Errorf("read model preferences: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return modelChoices{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return modelChoices{}, errors.New("model preferences must be a private 0600 regular file")
	}
	if info.Size() > modelChoicesBytes {
		return modelChoices{}, errors.New("model preferences exceed 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, modelChoicesBytes+1))
	if err != nil {
		return modelChoices{}, err
	}
	if len(data) > modelChoicesBytes {
		return modelChoices{}, errors.New("model preferences exceed 64 KiB")
	}
	if !utf8.Valid(data) {
		return modelChoices{}, errors.New("model preferences must contain UTF-8 JSON")
	}
	var choices modelChoices
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&choices); err != nil {
		return modelChoices{}, fmt.Errorf("decode model preferences: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return modelChoices{}, errors.New("model preferences contain trailing data")
	}
	if choices.Version != 1 || choices.Choices == nil {
		return modelChoices{}, errors.New("invalid model preferences version or choices")
	}
	for providerID, choice := range choices.Choices {
		if err := validateModelChoice(providerID, choice); err != nil {
			return modelChoices{}, err
		}
	}
	return choices, nil
}

func validateModelChoice(providerID string, choice modelChoice) error {
	for _, field := range []struct {
		name, value string
		limit       int
	}{{"provider", providerID, 128}, {"model ID", choice.ID, 512}, {"variant", choice.Variant, 128}} {
		if err := choiceText(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	return nil
}

func choiceText(name, value string, limit int) error {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid model preference %s (require 1–%d UTF-8 bytes without controls or surrounding whitespace)", name, limit)
	}
	return nil
}
