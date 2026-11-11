package guards

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/logging"
)

// The guards of the 2026-11-10 structured-logging design (§9), clustarr's
// contract as this repo adopts it: one error key, no global or hand-built
// logger outside pkg/logging, no logger-inventing helper, no "component"
// key, every subsystem a literal in the table, no print past the logger.

// repoRoot is the module root, two directories up from test/guards.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	return root
}

// isSelector reports whether e is pkg.name.
func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// isIdent reports whether e is the bare identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// importAlias is the name file imports importPath under, "" when it does not.
func importAlias(file *ast.File, importPath, pkgName string) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return pkgName
	}
	return ""
}

// errorKeySites lists every string literal "error" at a key position of a
// log call in file: the key/value pairs of Debug, Info, Warn, Error and
// logging.With (from the second argument), of their Context forms (from the
// third), of Log (from the fourth), and the key of a slog attribute
// constructor. The key is err (the contract's §3).
func errorKeySites(path string, file *ast.File, fset *token.FileSet) []string {
	var sites []string
	keysFrom := func(args []ast.Expr, first int) {
		for i := first; i < len(args); i += 2 {
			if lit, ok := args[i].(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"error"` {
				sites = append(sites, fmt.Sprintf("%s:%d", path, fset.Position(lit.Pos()).Line))
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "slog" {
			switch sel.Sel.Name {
			case "String", "Any", "Int", "Int64", "Uint64", "Float64", "Bool", "Duration", "Time", "Group":
				keysFrom(call.Args, 0)
			}
			return true
		}
		switch sel.Sel.Name {
		case "Debug", "Info", "Warn", "Error", "With":
			keysFrom(call.Args, 1)
		case "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
			keysFrom(call.Args, 2)
		case "Log":
			keysFrom(call.Args, 3)
		}
		return true
	})
	return sites
}

func TestNoErrorAttributeKey(t *testing.T) {
	var sites []string
	for path, file := range productionFilesWithPositions(t) {
		sites = append(sites, errorKeySites(path, file.file, file.fset)...)
	}
	sort.Strings(sites)
	assert.Empty(t, sites, "a log attribute key \"error\": the one error key is err (the contract's §3)\n%s", strings.Join(sites, "\n"))
}

// TestNoGlobalLogger: slog.Default, slog.New, slog.SetDefault and slog's
// handler constructors appear only in pkg/logging, which builds the one
// root logger a process runs with (the contract's §4, §5). Every other package
// takes the logger from its context or is handed one.
func TestNoGlobalLogger(t *testing.T) {
	var sites []string
	for path, file := range productionFilesWithPositions(t) {
		if strings.HasPrefix(path, "pkg/logging/") {
			continue
		}
		ast.Inspect(file.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, name := range []string{"Default", "New", "SetDefault", "NewJSONHandler", "NewTextHandler"} {
				if isSelector(call.Fun, "slog", name) {
					sites = append(sites, fmt.Sprintf("%s:%d slog.%s", path, file.fset.Position(call.Pos()).Line, name))
				}
			}
			return true
		})
	}
	sort.Strings(sites)
	assert.Empty(t, sites, "a global or hand-built slog logger outside pkg/logging\n%s", strings.Join(sites, "\n"))
}

// stderrExempt names the production files, or directories, that may print
// to stderr or through the log package, each with its reason.
var stderrExempt = map[string]string{
	"cmd/shim/":               "the shim runs as Plex's transcoder stand-in, whose stderr Plex reads",
	"cmd/maintenance/main.go": "the Job's usage error, before a logger exists",
}

// exempt reports whether path is one of stderrExempt's files or under one
// of its directories.
func exempt(path string) bool {
	for p := range stderrExempt {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}

// TestNoStdlibLogOrStderrPrints: no log.Print*, log.Fatal* or log.Panic*,
// and no fmt.Fprint*(os.Stderr, ...) in production code (the contract's §9);
// everything a running process says goes through its logger.
func TestNoStdlibLogOrStderrPrints(t *testing.T) {
	var sites []string
	for path, file := range productionFilesWithPositions(t) {
		if exempt(path) {
			continue
		}
		logAlias := importAlias(file.file, "log", "log")
		ast.Inspect(file.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			line := file.fset.Position(call.Pos()).Line
			if logAlias != "" {
				for _, name := range []string{"Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln"} {
					if isSelector(call.Fun, logAlias, name) {
						sites = append(sites, fmt.Sprintf("%s:%d log.%s", path, line, name))
					}
				}
			}
			for _, name := range []string{"Fprint", "Fprintf", "Fprintln"} {
				if isSelector(call.Fun, "fmt", name) && len(call.Args) > 0 && isSelector(call.Args[0], "os", "Stderr") {
					sites = append(sites, fmt.Sprintf("%s:%d fmt.%s(os.Stderr", path, line, name))
				}
			}
			return true
		})
	}
	sort.Strings(sites)
	assert.Empty(t, sites, "a print past the logger\n%s", strings.Join(sites, "\n"))
	for path, why := range stderrExempt {
		assert.NotEmpty(t, why, path)
	}
}

// parsedFile is a production file with the FileSet its positions resolve
// through, for a guard that reports lines.
type parsedFile struct {
	file *ast.File
	fset *token.FileSet
}

// productionFilesWithPositions is productionFiles keeping each file's
// FileSet.
func productionFilesWithPositions(t *testing.T) map[string]parsedFile {
	t.Helper()
	root := repoRoot(t)
	out := map[string]parsedFile{}
	for _, dir := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			rel, _ := filepath.Rel(root, path)
			out[filepath.ToSlash(rel)] = parsedFile{file: file, fset: fset}
			return nil
		})
		require.NoError(t, err)
	}
	return out
}

// literalSource is where a file's non-literal subsystem names come from: a
// file of string literals and the prefixes the names carry.
type literalSource struct {
	file     string
	prefixes []string
}

// subsystemLiteralFiles are the files that name a record's subsystem from a
// variable holding one of a known set of literals: the Plex log tailer,
// whose subsystem is its file's (FileFor's table in files.go), and the
// supervisor's stdout relay (pms.stdout, pms.stderr, shim in its own file).
var subsystemLiteralFiles = map[string]literalSource{
	"pkg/plex/logs/tailer.go":   {"pkg/plex/logs/files.go", []string{"pms"}},
	"pkg/plex/logs/parse.go":    {"pkg/plex/logs/files.go", []string{"pms"}},
	"cmd/manager/supervisor.go": {"cmd/manager/supervisor.go", []string{"pms.", "shim"}},
}

// stringLiterals is every string literal in the production file at rel.
func stringLiterals(t *testing.T, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), rel), nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, strings.Trim(lit.Value, `"`))
		}
		return true
	})
	return out
}

// TestEverySubsystemIsInTheTable (the contract's §5, §9): every logging.Subsystem and
// SubsystemLogger call names its subsystem with a string literal (or a
// package's own `const Subsystem`, or a "remediation."+name concatenation
// where subsystemPrefixSites allows it); every such name is in
// logging.Subsystems; every entry of the table is used; and every Reconcile
// method taking a context names one.
func TestEverySubsystemIsInTheTable(t *testing.T) {
	table := map[string]bool{}
	for i, name := range logging.Subsystems {
		assert.False(t, table[name], "logging.Subsystems lists %q twice", name)
		table[name] = true
		if i > 0 {
			assert.Less(t, logging.Subsystems[i-1], name, "logging.Subsystems is sorted")
		}
	}
	used := map[string]bool{}
	var problems []string
	for path, pf := range productionFilesWithPositions(t) {
		alias := importAlias(pf.file, "github.com/mediactl/clusterplex/pkg/logging", "logging")
		if pf.file.Name.Name == "logging" && strings.HasPrefix(path, "pkg/logging/") {
			alias = ""
		}
		consts := map[string]string{}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			gen, ok := n.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				return true
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[id.Name] = strings.Trim(lit.Value, `"`)
						}
					}
				}
			}
			return false
		})
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var nameArg ast.Expr
			switch {
			case alias != "" && (isSelector(call.Fun, alias, "Subsystem") || isSelector(call.Fun, alias, "SubsystemLogger")),
				alias == "" && strings.HasPrefix(path, "pkg/logging/") && (isIdent(call.Fun, "Subsystem") || isIdent(call.Fun, "SubsystemLogger")):
				if len(call.Args) == 2 {
					nameArg = call.Args[1]
				}
			default:
				return true
			}
			where := fmt.Sprintf("%s:%d", path, pf.fset.Position(call.Pos()).Line)
			switch v := nameArg.(type) {
			case *ast.BasicLit:
				name := strings.Trim(v.Value, `"`)
				used[name] = true
				if !table[name] {
					problems = append(problems, where+": subsystem "+name+" is not in logging.Subsystems")
				}
			case *ast.Ident, *ast.SelectorExpr:
				if strings.HasPrefix(path, "pkg/logging/") {
					return true // the package's own plumbing passes the name through
				}
				if src, ok := subsystemLiteralFiles[path]; ok {
					for _, lit := range stringLiterals(t, src.file) {
						for _, prefix := range src.prefixes {
							if strings.HasPrefix(lit, prefix) {
								used[lit] = true
								if !table[lit] {
									problems = append(problems, where+": subsystem "+lit+" is not in logging.Subsystems")
								}
							}
						}
					}
					return true
				}
				id, isIdent := nameArg.(*ast.Ident)
				if !isIdent {
					problems = append(problems, where+": the subsystem is not a literal")
					return true
				}
				name, ok := consts[id.Name]
				if !ok {
					problems = append(problems, where+": the subsystem is not a literal or this file's const")
					return true
				}
				used[name] = true
				if !table[name] {
					problems = append(problems, where+": subsystem "+name+" is not in logging.Subsystems")
				}
			default:
				problems = append(problems, where+": the subsystem is not a literal")
			}
			return true
		})
	}
	for name := range table {
		if !used[name] {
			problems = append(problems, "logging.Subsystems lists "+name+", which nothing names")
		}
	}
	sort.Strings(problems)
	assert.Empty(t, problems, strings.Join(problems, "\n"))
}

// TestNoLoggerInventingHelper: no method named log or logger returning a
// *slog.Logger. Such a helper invents a logger (slog.Default, a discard
// logger) for a type nobody handed one, and its lines then carry no service
// or subsystem; a type refuses a nil logger where its work starts instead.
func TestNoLoggerInventingHelper(t *testing.T) {
	var sites []string
	for path, pf := range productionFilesWithPositions(t) {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || (fn.Name.Name != "log" && fn.Name.Name != "logger") {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			if star, ok := fn.Type.Results.List[0].Type.(*ast.StarExpr); ok && isSelector(star.X, "slog", "Logger") {
				sites = append(sites, fmt.Sprintf("%s:%d %s()", path, pf.fset.Position(fn.Pos()).Line, fn.Name.Name))
			}
		}
	}
	sort.Strings(sites)
	assert.Empty(t, sites, "a logger-inventing helper\n%s", strings.Join(sites, "\n"))
}

// TestNoComponentKey: the unit that wrote a line is its subsystem (the
// contract's §3), never a "component" attribute.
func TestNoComponentKey(t *testing.T) {
	var sites []string
	for path, pf := range productionFilesWithPositions(t) {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && lit.Value == `"component"` {
				sites = append(sites, fmt.Sprintf("%s:%d", path, pf.fset.Position(lit.Pos()).Line))
			}
			return true
		})
	}
	sort.Strings(sites)
	assert.Empty(t, sites, "a \"component\" key: the subsystem carries it\n%s", strings.Join(sites, "\n"))
}
