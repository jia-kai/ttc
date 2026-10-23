// Package messagestream defines durable presentation rows independently of model context.
package messagestream

import (
	"crypto/sha256"
	"encoding/hex"
)

// Action is an explicit frontend operation on an immutable history target.
type Action struct {
	Kind   string `json:"kind"`
	Target int64  `json:"target"`
}

const (
	// Inspect opens the human-readable inspector.
	Inspect = "inspect"
	// Raw opens the exact captured provider JSON.
	Raw = "raw"
)

// Item is a bounded, replayable presentation row. ID is its history entry ID.
type Item struct {
	ID       int64    `json:"id"`
	Revision int64    `json:"revision"` // Latest private source event; updates this row without adding another row.
	Actor    string   `json:"actor"`
	Title    string   `json:"title"`
	Actions  []Action `json:"actions,omitempty"`
}

// Hash computes the content address of exact bytes.
func Hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
