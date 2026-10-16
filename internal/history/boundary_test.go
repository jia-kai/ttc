package history

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestStorageBoundary prevents application code from acquiring SQL handles or
// depending on the SQL fault-injection helper used by persistence tests.
func TestStorageBoundary(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, tree := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			storage := strings.HasPrefix(rel, "internal/history/")
			testHelper := strings.HasPrefix(rel, "internal/historytest/")
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				name, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				switch {
				case name == "ttc/internal/historytest":
					t.Errorf("%s: production code must not import the history test helper", rel)
				case !storage && !testHelper && (name == "database/sql" || strings.HasPrefix(name, "modernc.org/sqlite")):
					t.Errorf("%s: SQL access belongs in internal/history", rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryValuesAndProjectionsHaveNoIOImports(t *testing.T) {
	for _, path := range []string{"types.go", "projection.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "database/sql" || name == "os" || name == "net" || strings.HasPrefix(name, "net/") || strings.HasPrefix(name, "modernc.org/") {
				t.Errorf("%s imports %s: acquire external data before projecting it", path, name)
			}
		}
	}
}
