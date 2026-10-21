package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every kind of event declared here is published somewhere. A kind that is
// declared but never sent looks like a live update to whoever reads this file,
// while the clients wait for it in vain — channel.changed sat here like that
// until LIVE-1. The check reads the sources: some non-test file outside this
// package must name events.Kind….
func TestEveryKindHasAPublisher(t *testing.T) {
	declared := declaredKinds(t)
	if len(declared) == 0 {
		t.Fatal("found no Kind constants; the test reads the wrong file")
	}
	used := usedKinds(t, filepath.Join("..", ".."))
	for _, kind := range declared {
		if !used[kind] {
			t.Errorf("events.%s is declared but no module publishes it", kind)
		}
	}
}

// declaredKinds are the constants of type Kind in this package.
func declaredKinds(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	if err != nil {
		t.Fatalf("parse events.go: %v", err)
	}
	var kinds []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "Kind" {
				continue
			}
			for _, name := range value.Names {
				kinds = append(kinds, name.Name)
			}
		}
	}
	return kinds
}

// usedKinds are the Kind… names selected from this package (events.KindX) in
// the non-test, non-generated Go files of the repository, this package aside.
func usedKinds(t *testing.T, root string) map[string]bool {
	t.Helper()
	used := map[string]bool{}
	ownDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git", "tools":
				return filepath.SkipDir
			}
			if abs, _ := filepath.Abs(path); abs == ownDir {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".gen.go") {
			return nil
		}
		source, err := os.ReadFile(path) //nolint:gosec // the repository's own sources
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "events" && strings.HasPrefix(selector.Sel.Name, "Kind") {
				used[selector.Sel.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk sources: %v", err)
	}
	return used
}
