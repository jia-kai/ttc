package skills

import (
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
	"ttc/internal/prompts"
)

const (
	// maxFrontmatterBytes limits YAML source bytes between the opening and closing
	// delimiters (excluding the newline before the closing delimiter), not Markdown.
	maxFrontmatterBytes = 64 << 10
	// maxFrontmatterMappingPairs bounds yaml.v3's quadratic duplicate-key checking
	// during Decode, including mappings in otherwise inert metadata.
	maxFrontmatterMappingPairs = 256
	// maxFrontmatterNodes bounds aggregate decoding work across repeated mappings.
	// Count the root and syntax children, including keys and aliases, but not the
	// document wrapper or alias targets; yaml.v3 separately limits alias expansion.
	maxFrontmatterNodes = 4096
)

// metadata reads only an opening YAML frontmatter block. Plain Markdown has
// unrestricted roles; unrelated frontmatter keys (including allowed-tools) are inert.
func metadata(content string) (string, []Role, error) {
	first, rest, opened := strings.Cut(strings.TrimPrefix(content, "\ufeff"), "\n")
	if strings.TrimSuffix(first, "\r") != "---" {
		return "", nil, nil
	}
	if !opened {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterInvalid)
	}
	frontmatter := rest
	for offset := 0; ; {
		// Never scan the Markdown body or an arbitrarily long YAML line. The
		// extra bytes allow a final newline and CRLF closing delimiter at the limit.
		scan := rest[:min(len(rest), maxFrontmatterBytes-offset+6)]
		line, _, more := strings.Cut(scan, "\n")
		if strings.TrimSuffix(line, "\r") == "---" {
			frontmatter = strings.TrimSuffix(frontmatter[:offset], "\n")
			break
		}
		if offset+len(line) > maxFrontmatterBytes || len(scan) < len(rest) && !more {
			return "", nil, fmt.Errorf(prompts.SkillFrontmatterTooLarge, maxFrontmatterBytes, maxFrontmatterMappingPairs, maxFrontmatterNodes)
		}
		if !more {
			return "", nil, fmt.Errorf(prompts.SkillFrontmatterInvalid)
		}
		offset += len(line) + 1
		rest = rest[len(line)+1:]
	}
	if len(frontmatter) > maxFrontmatterBytes {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterTooLarge, maxFrontmatterBytes, maxFrontmatterMappingPairs, maxFrontmatterNodes)
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(frontmatter))
	if err := decoder.Decode(&doc); err != nil && err != io.EOF {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterInvalid)
	}
	// YAML document-end markers must not hide another document or malformed tail.
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterInvalid)
	}
	if len(doc.Content) == 0 {
		return "", nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterInvalid)
	}
	if !frontmatterWithinLimits(root) {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterTooLarge, maxFrontmatterBytes, maxFrontmatterMappingPairs, maxFrontmatterNodes)
	}
	if duplicateKey(root) {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterDuplicateKey)
	}
	// Decode the whole mapping so duplicate keys, including in inert metadata,
	// are rejected rather than relying on a first/last-key interpretation.
	var values map[string]any
	if err := root.Decode(&values); err != nil {
		return "", nil, fmt.Errorf(prompts.SkillFrontmatterInvalid)
	}
	var description string
	var roles []Role
	if value, exists := values["description"]; exists {
		var ok bool
		description, ok = value.(string)
		if !ok {
			return "", nil, fmt.Errorf(prompts.SkillDescriptionInvalid)
		}
	}
	if value, exists := values["roles"]; exists {
		items, ok := value.([]any)
		if !ok || len(items) == 0 {
			return "", nil, fmt.Errorf(prompts.SkillRolesInvalid)
		}
		seen := map[Role]bool{}
		for _, item := range items {
			text, ok := item.(string)
			role := Role(text)
			if !ok || (role != Main && role != Child && role != Aside) || seen[role] {
				return "", nil, fmt.Errorf(prompts.SkillRolesInvalid)
			}
			seen[role] = true
			roles = append(roles, role)
		}
	}
	return description, roles, nil
}

// frontmatterWithinLimits visits syntax nodes once, before Decode's mapping
// duplicate checks. Alias targets are already syntax children elsewhere; following
// them here would double-count shared nodes and could loop on cyclic aliases.
func frontmatterWithinLimits(root *yaml.Node) bool {
	nodes := 1
	pending := []*yaml.Node{root}
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if node.Kind == yaml.MappingNode && len(node.Content)/2 > maxFrontmatterMappingPairs {
			return false
		}
		nodes += len(node.Content)
		if nodes > maxFrontmatterNodes {
			return false
		}
		pending = append(pending, node.Content...)
	}
	return true
}

// Visit syntax children, not alias targets: aliases may form cycles. The YAML
// decoder separately validates alias expansion and unsupported mapping keys.
func duplicateKey(node *yaml.Node) bool {
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind == yaml.ScalarNode {
				if seen[key.Value] {
					return true
				}
				seen[key.Value] = true
			}
		}
	}
	for _, child := range node.Content {
		if duplicateKey(child) {
			return true
		}
	}
	return false
}
