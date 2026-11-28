package main

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envName is the shape of a configuration variable name.
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)

// TestEnvExampleListsEveryEnvVar keeps .env.example and the code in step: every
// env var the agentgate binary reads is listed there, and every name listed
// there is read by the binary or by scripts/agentgate-local.
//
// A name counts as read by the binary when, in a non-test file of a package
// agentgate is built from, it is a string literal that
//   - is passed straight to a function whose name contains "env" (os.Getenv,
//     os.LookupEnv, envOr, envBool, envDuration, ...), or
//   - is the value of a constant or variable named Env*, or
//   - starts with "AG_".
//
// Provider keys are built at run time (config.ProviderKey); they are taken
// from configs/providers.example.json.
func TestEnvExampleListsEveryEnvVar(t *testing.T) {
	read := codeEnvNames(t)
	for _, n := range providerKeyNames(t) {
		read[n] = "configs/providers.example.json"
	}
	for _, n := range retiredEnv { // named only to refuse them; not configuration
		delete(read, n)
	}
	listed := envExampleNames(t)
	script := scriptEnvNames(t, "../../scripts/agentgate-local")

	var missing, unread []string
	for n, where := range read {
		if !listed[n] {
			missing = append(missing, n+" (read in "+where+")")
		}
	}
	for n := range listed {
		if read[n] == "" && !script[n] {
			unread = append(unread, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(unread)
	if len(missing) > 0 {
		t.Errorf(".env.example is missing names the code reads; add them with a one-line comment:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(unread) > 0 {
		t.Errorf(".env.example lists names nothing reads; remove them:\n  %s", strings.Join(unread, "\n  "))
	}
}

// codeEnvNames returns each env name read by the binary and one file reading it.
func codeEnvNames(t *testing.T) map[string]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	add := func(fset *token.FileSet, lit *ast.BasicLit) {
		if lit == nil || lit.Kind != token.STRING {
			return
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil || !envName.MatchString(s) {
			return
		}
		if _, ok := names[s]; !ok {
			p := fset.Position(lit.Pos())
			rel, _ := filepath.Rel(root, p.Filename)
			names[s] = rel + ":" + strconv.Itoa(p.Line)
		}
	}
	fset := token.NewFileSet()
	for _, dir := range strings.Fields(string(out)) {
		if !strings.HasPrefix(dir, root+string(filepath.Separator)) && dir != root {
			continue // a module dependency, not our code
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if strings.Contains(strings.ToLower(callName(n.Fun)), "env") {
						for _, a := range n.Args {
							lit, _ := a.(*ast.BasicLit)
							add(fset, lit)
						}
					}
				case *ast.ValueSpec:
					for i, id := range n.Names {
						if strings.HasPrefix(id.Name, "Env") && i < len(n.Values) {
							lit, _ := n.Values[i].(*ast.BasicLit)
							add(fset, lit)
						}
					}
				case *ast.BasicLit:
					if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && strings.HasPrefix(s, "AG_") {
						add(fset, n)
					}
				}
				return true
			})
		}
	}
	return names
}

func callName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// providerKeyNames mirrors config.ProviderKey for the providers in the example file.
func providerKeyNames(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../configs/providers.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	var out []string
	for name := range f.Providers {
		out = append(out, strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))+"_API_KEY")
	}
	return out
}

// envExampleNames parses .env.example: comments, blank lines and NAME= lines
// only. A value is an error: the file holds names, never values.
func envExampleNames(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names := map[string]bool{}
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, val, ok := strings.Cut(line, "=")
		if !ok || !envName.MatchString(name) || val != "" {
			t.Errorf(".env.example:%d: want NAME= with no value, got %q", ln, line)
			continue
		}
		if names[name] {
			t.Errorf(".env.example:%d: %s listed twice", ln, name)
		}
		names[name] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// scriptEnvNames returns the names a shell script reads as ${NAME...} or $NAME.
func scriptEnvNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(string(b), -1) {
		names[m[1]] = true
	}
	return names
}
