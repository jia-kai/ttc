// embed-prompts validates source assets and generates runtime Go constants.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type toolPrompt struct {
	Description string            `yaml:"description"`
	Notes       map[string]string `yaml:"notes,omitempty"`
}

type compactionPrompts struct {
	Instructions string `yaml:"instructions"`
	Input        string `yaml:"input"`
	Links        string `yaml:"links"`
}

type namingSettings struct {
	Text           string `yaml:"text"`
	OutputTokens   int    `yaml:"output_tokens"`
	MaxAttempts    int    `yaml:"max_attempts"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

var assetName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func main() {
	root := flag.String("root", ".", "repository root containing prompt/ and internal/")
	output := flag.String("output", "internal/prompts/assets_generated.go", "output path relative to root")
	flag.Parse()
	if err := generate(*root, *output); err != nil {
		fmt.Fprintln(os.Stderr, "embed-prompts:", err)
		os.Exit(1)
	}
}

func decodeYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: expected exactly one YAML document", path)
	}
	return nil
}

func validText(text string) bool {
	return strings.TrimSpace(text) != "" && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func registeredTools(root string) (map[string]bool, error) {
	names := map[string]bool{}
	for _, dir := range []string{"internal/tool", "internal/session"} {
		files, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		for _, entry := range files {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, dir, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return nil, err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 3 {
					return true
				}
				registered := false
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					registered = dir == "internal/tool" && fn.Name == "Register"
				case *ast.SelectorExpr:
					pkg, ok := fn.X.(*ast.Ident)
					registered = ok && pkg.Name == "tool" && fn.Sel.Name == "Register"
				}
				if registered {
					literal, ok := call.Args[1].(*ast.BasicLit)
					if ok && literal.Kind == token.STRING {
						name, err := strconv.Unquote(literal.Value)
						if err == nil {
							names[name] = true
						}
					}
				}
				return true
			})
		}
	}
	return names, nil
}

func generate(root, output string) error {
	directory := filepath.Join(root, "prompt")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	texts := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if name == "README.md" {
			continue
		}
		if name == "tools.yaml" || name == "naming.yaml" || name == "compaction.yaml" {
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || !assetName.MatchString(stem) {
			return fmt.Errorf("unsupported prompt asset %s", name)
		}
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if !validText(string(data)) {
			return fmt.Errorf("%s must contain nonempty UTF-8 text without NUL", name)
		}
		var symbol strings.Builder
		for _, word := range strings.Split(stem, "-") {
			symbol.WriteString(strings.ToUpper(word[:1]) + word[1:])
		}
		key := symbol.String()
		if _, found := texts[key]; found || key == "Naming" || key == "NamingSettings" || key == "Compaction" || key == "CompactionInput" || key == "CompactionLinks" || key == "ToolDescription" || key == "ToolNote" || key == "ToolNames" {
			return fmt.Errorf("duplicate or reserved prompt name %s", name)
		}
		texts[key] = string(data)
	}
	for _, name := range []string{"System", "Child", "Btw"} {
		if _, ok := texts[name]; !ok {
			return fmt.Errorf("missing Markdown prompt %s", name)
		}
	}
	var compaction compactionPrompts
	if err = decodeYAML(filepath.Join(directory, "compaction.yaml"), &compaction); err != nil {
		return err
	}
	for name, text := range map[string]string{"Compaction": compaction.Instructions, "CompactionInput": compaction.Input, "CompactionLinks": compaction.Links} {
		if !validText(text) {
			return fmt.Errorf("compaction %s must contain nonempty UTF-8 text without NUL", name)
		}
		texts[name] = text
	}
	for name, slots := range map[string]int{"CompactionInput": 2, "CompactionLinks": 3} {
		text := texts[name]
		if strings.Count(text, "%s") != slots || strings.Contains(strings.ReplaceAll(text, "%s", ""), "%") {
			return fmt.Errorf("%s format must contain exactly %d %%s placeholders", name, slots)
		}
	}
	var tools map[string]toolPrompt
	if err = decodeYAML(filepath.Join(directory, "tools.yaml"), &tools); err != nil {
		return err
	}
	expected, err := registeredTools(root)
	if err != nil {
		return err
	}
	for name := range expected {
		if _, ok := tools[name]; !ok {
			return fmt.Errorf("missing prompt for registered tool %s", name)
		}
	}
	for name, tool := range tools {
		if !toolName.MatchString(name) || !expected[name] {
			return fmt.Errorf("prompt names unregistered tool %s", name)
		}
		if !validText(tool.Description) {
			return fmt.Errorf("tool %s has empty/invalid description", name)
		}
		for key, note := range tool.Notes {
			if !toolName.MatchString(key) || !validText(note) {
				return fmt.Errorf("tool %s has invalid note %s", name, key)
			}
		}
	}
	var naming namingSettings
	if err = decodeYAML(filepath.Join(directory, "naming.yaml"), &naming); err != nil {
		return err
	}
	if !validText(naming.Text) || naming.OutputTokens < 1 || naming.OutputTokens > 4096 || naming.MaxAttempts < 1 || naming.MaxAttempts > 8 || naming.TimeoutSeconds < 1 || naming.TimeoutSeconds > 120 {
		return errors.New("naming requires nonempty text, output_tokens 1–4096, max_attempts 1–8, timeout_seconds 1–120")
	}
	var generated bytes.Buffer
	generated.WriteString("// Code generated by cmd/embed-prompts; DO NOT EDIT.\n\npackage prompts\n\n")
	keys := make([]string, 0, len(texts))
	for key := range texts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&generated, "// %s is generated from the canonical prompt asset.\nconst %s = %q\n\n", key, key, texts[key])
	}
	fmt.Fprintf(&generated, "var naming = NamingSettings{Text: %q, OutputTokens: %d, MaxAttempts: %d, TimeoutSeconds: %d}\n\n", naming.Text, naming.OutputTokens, naming.MaxAttempts, naming.TimeoutSeconds)
	generated.WriteString("var toolPrompts = map[string]toolPrompt{\n")
	keys = keys[:0]
	for key := range tools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tool := tools[key]
		fmt.Fprintf(&generated, "%q: {Description: %q, Notes: map[string]string{", key, tool.Description)
		notes := make([]string, 0, len(tool.Notes))
		for note := range tool.Notes {
			notes = append(notes, note)
		}
		sort.Strings(notes)
		for _, note := range notes {
			fmt.Fprintf(&generated, "%q: %q,", note, tool.Notes[note])
		}
		generated.WriteString("}},\n")
	}
	generated.WriteString("}\n")
	source, err := format.Source(generated.Bytes())
	if err != nil {
		return fmt.Errorf("format generated prompts: %w", err)
	}
	target := filepath.Join(root, output)
	prior, err := os.ReadFile(target)
	if err == nil && bytes.Equal(prior, source) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".prompts-*.go")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(source); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), target)
}
