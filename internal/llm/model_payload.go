package llm

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
	"ttc/internal/prompts"
)

// ModelPayload is one raw model-facing text value and its optional metadata.
// Text is copied byte-for-byte, including invalid UTF-8 and trailing newlines.
type ModelPayload struct {
	Metadata PayloadMetadata
	Text     string
}

// PayloadMetadata describes a payload without placing its text in YAML.
// Zero-valued fields are omitted. AttachmentIndices is a pointer so an explicit
// empty list remains distinguishable from absent attachment metadata.
type PayloadMetadata struct {
	Kind              string `yaml:"kind,omitempty"`
	Type              string `yaml:"type,omitempty"`
	Field             string `yaml:"field,omitempty"`
	ActorID           string `yaml:"actor_id,omitempty"`
	ChildID           string `yaml:"child_id,omitempty"`
	JobID             string `yaml:"job_id,omitempty"`
	Stream            string `yaml:"stream,omitempty"`
	Scope             string `yaml:"scope,omitempty"`
	Path              string `yaml:"path,omitempty"`
	Timestamp         string `yaml:"timestamp,omitempty"`
	AttachmentIndices *[]int `yaml:"attachment_indices,omitempty"`
	Index             *int   `yaml:"index,omitempty"`
	Truncated         bool   `yaml:"truncated,omitempty"`
	DeliveryTruncated bool   `yaml:"delivery_truncated,omitempty"`
	OriginalBytes     *int   `yaml:"original_bytes,omitempty"`
	Reference         string `yaml:"reference,omitempty"`
}

// FormatModelPayload frames a single raw text payload with typed YAML metadata.
// The smallest available ID is selected in one linear scan of Text. An opening
// or closing marker prefix blocks every canonical numeric prefix it contains;
// metadata needs no scan because YAML supplies its own scalar context.
func FormatModelPayload(payload ModelPayload) (string, error) {
	id := modelPayloadID(payload.Text)
	var out strings.Builder
	out.WriteString("<<<" + id + ":\n")
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(payload.Metadata); err != nil {
		return "", fmt.Errorf(prompts.LLMModelYAML, err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf(prompts.LLMModelYAML, err)
	}
	out.WriteString(">>>\n")
	out.WriteString(payload.Text)
	out.WriteString("\n<<</" + id + ">>>")
	return out.String(), nil
}

func modelPayloadID(text string) string {
	// Every occupied numeric prefix requires a digit occurrence, so fewer
	// than len(text) candidates can be blocked. A byte-indexed table gives a
	// deterministic linear bound without hashing; larger numeric prefixes
	// cannot affect the first free candidate. Accumulation stays below len(text)
	// to avoid both table overrun and integer overflow.
	var blocked []bool
	base := false
	for i := 0; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		start := i
		if strings.HasPrefix(text[start:], "<<</ttc") {
			start += 7
		} else if strings.HasPrefix(text[start:], "<<<ttc") {
			start += 6
		} else {
			continue
		}
		if !base {
			blocked = make([]bool, len(text))
			base = true
		}
		if start >= len(text) || text[start] != '-' {
			continue
		}
		start++
		if start >= len(text) || text[start] < '0' || text[start] > '9' {
			continue
		}
		if text[start] == '0' {
			blocked[0] = true
			continue
		}
		n := 0
		for j := start; j < len(text) && text[j] >= '0' && text[j] <= '9'; j++ {
			digit := int(text[j] - '0')
			limit := len(text) - 1
			if n > limit/10 || n == limit/10 && digit > limit%10 {
				break
			}
			n = n*10 + digit
			blocked[n] = true
		}
	}
	if !base {
		return "ttc"
	}
	for n := 0; ; n++ {
		if !blocked[n] {
			return fmt.Sprintf("ttc-%d", n)
		}
	}
}
