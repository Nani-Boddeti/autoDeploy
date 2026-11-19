// Package srcguard finds direct process-output calls in production Go source
// so a package can prove it logs only through an injected logger.
package srcguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// denied is an explicit deny list (not prefix matching, except where noted) of
// package-level identifiers, keyed by import path, that write to process
// output or reconfigure the global logger and so bypass the injected logger.
// Constructors (log.New, slog.New*), attr helpers, levels, slog.DiscardHandler
// and slog.SetDefault are deliberately absent.
var denied = map[string]func(name string) bool{
	"os":       setOf("Stderr", "Stdout", "NewFile"),
	"syscall":  setOf("Stderr", "Stdout"),
	"fmt":      setOf("Print", "Printf", "Println"),
	"log/slog": setOf("Debug", "Info", "Warn", "Error", "DebugContext", "InfoContext", "WarnContext", "ErrorContext", "Log", "LogAttrs", "Default"),
	"log": func(name string) bool {
		return strings.HasPrefix(name, "Print") || strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Panic") ||
			setOf("Default", "Writer", "SetOutput", "SetFlags", "SetPrefix")(name)
	},
}

// dotImportDenied are the packages whose dot-import is itself a violation,
// because it hides the selector the guard looks for.
var dotImportDenied = []string{"os", "log", "log/slog", "fmt", "syscall"}

func setOf(names ...string) func(string) bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return func(n string) bool { return m[n] }
}

// Violations parses src (filename is used only in messages) and returns one
// message per direct process-output use: the denied identifiers above and the
// print/println builtins. A selector counts only when its identifier resolves
// to the denied package through the file's imports (renamed imports are
// followed; a local variable that merely shares a package's name is not
// flagged; a local that shadows a name the file also imports is not
// distinguished). Dot-imports of the guarded packages are violations outright.
// Comments and string literals never match.
func Violations(filename, src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	report := func(n ast.Node, what string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+what)
	}
	names := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := pathpkg.Base(path)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch local {
		case ".":
			if slices.Contains(dotImportDenied, path) {
				report(imp, "dot-import of "+path)
			}
		case "_":
		default:
			names[local] = path
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			if path, ok := names[id.Name]; ok {
				if deny := denied[path]; deny != nil && deny(x.Sel.Name) {
					report(x, path+"."+x.Sel.Name)
				}
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
				report(x, "builtin "+id.Name)
			}
		}
		return true
	})
	return out, nil
}

// CheckPackageDir fails t for every violation in the non-test .go files
// directly inside dir (not recursive).
func CheckPackageDir(t *testing.T, dir string) {
	t.Helper()
	for _, v := range violationsInDir(t, dir) {
		t.Errorf("direct process output; use the injected redacting logger: %s", v)
	}
}
