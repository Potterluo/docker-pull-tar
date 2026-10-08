package main

// generator — the AI-native scaffolding CLI for this template.
//
//	go run ./cmd/generator init   --module github.com/you/myapp --name MyApp
//	go run ./cmd/generator entity task --field title:string --field done:bool
//
// `init` rebrands the template (module path, display name, logo letter).
// `entity` generates a full vertical slice: store interface + SQL +
// migration, HTTP handlers, routes, typed frontend API client, CRUD page,
// and a nav entry — inserted at the `--- gen:* ---` markers in the
// skeleton. Agents should prefer the generator over hand-copying items;
// the manual recipe it automates lives in docs/agent/add-entity.md.

import (
	"bufio"
	"embed"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"unicode"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "entity":
		runEntity(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  generator init   --module <go-module-path> --name <Display Name>
  generator entity <singular-name> [--plural <plural>] [--icon <LucideIcon>]
                    [--field <name:string|text|bool|int>]...   (repeatable)

examples:
  go run ./cmd/generator init --module github.com/you/loopapp --name LoopApp
  go run ./cmd/generator entity task --field title:string --field body:text --field done:bool
`)
	os.Exit(2)
}

// --- shared helpers ---------------------------------------------------------

func repoRoot() string {
	wd, err := os.Getwd()
	must(err)
	if _, err := os.Stat(filepath.Join(wd, "go.mod")); err != nil {
		fmt.Fprintln(os.Stderr, "error: run from the repository root (go.mod not found)")
		os.Exit(1)
	}
	return wd
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func mustWrite(path string, content []byte) {
	must(os.MkdirAll(filepath.Dir(path), 0o755))
	must(os.WriteFile(path, content, 0o644))
}

// insertAfter finds the line containing marker and returns the file with
// insertion appended right after that line. Errors if the marker is
// missing (skeleton drifts → generator must fail loudly, not guess).
func insertAfter(src, marker, insertion string) (string, error) {
	// The marker must be the LAST thing on its line (documented in
	// AGENTS.md). Line-level matching keeps prose comments that merely
	// mention a marker from hijacking the insertion point.
	lines := strings.Split(src, "\n")
	found := -1
	for i, line := range lines {
		if strings.HasSuffix(strings.TrimSpace(line), marker) {
			found = i
			break
		}
	}
	if found < 0 {
		return "", fmt.Errorf("marker %q not found — the skeleton drifted from the generator; reconcile manually", marker)
	}
	out := make([]string, 0, len(lines)+8)
	out = append(out, lines[:found+1]...)
	out = append(out, strings.Split(strings.TrimRight(insertion, "\n"), "\n")...)
	out = append(out, lines[found+1:]...)
	return strings.Join(out, "\n"), nil
}

func parseTemplates() *template.Template {
	t, err := template.New("").Delims("[[", "]]").Funcs(template.FuncMap{
		"lower": strings.ToLower,
		"add":   func(a, b int) int { return a + b },
		"qs":    func(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") },
	}).ParseFS(templateFS, "templates/*.tmpl")
	must(err)
	return t
}

// --- naming -----------------------------------------------------------------

type Field struct {
	Name     string // go/js identifier, camelCase
	JSONName string // == Name (we emit camelCase json tags)
	Type     string // string | text | bool | int
	Title    string // display label
}

func (f Field) GoType() string {
	switch f.Type {
	case "bool":
		return "bool"
	case "int":
		return "int64"
	default:
		return "string"
	}
}

func (f Field) GoZero() string {
	switch f.Type {
	case "bool":
		return "false"
	case "int":
		return "0"
	default:
		return `""`
	}
}

func (f Field) SQLType() string {
	switch f.Type {
	case "bool", "int":
		return "INTEGER NOT NULL DEFAULT 0"
	default:
		return "TEXT NOT NULL DEFAULT ''"
	}
}

// TSType is the TypeScript type used in the generated API client.
func (f Field) TSType() string {
	switch f.Type {
	case "bool":
		return "boolean"
	case "int":
		return "number"
	default:
		return "string"
	}
}

// GoName is the exported Go identifier for the field: "estimate" → "Estimate".
func (f Field) GoName() string { return capitalize(f.Name) }

type Entity struct {
	Singular    string // "task"
	Plural      string // "tasks"
	Title       string // "Task"
	TitlePlural string // "Tasks"
	Icon        string // lucide icon component name
	Module      string // go module path (read from go.mod)
	Fields      []Field
}

func (e Entity) SQLColumns() string {
	// Plain column definitions — the fragment is spliced inside a Go raw
	// string literal, so no backticks (they'd terminate it).
	//
	// There is no user_id column: DockerPull is a single-user local tool,
	// so a generated entity owns its rows outright (AGENTS.md rule 2).
	cols := []string{"id TEXT PRIMARY KEY"}
	for _, f := range e.Fields {
		cols = append(cols, f.Name+" "+f.SQLType())
	}
	cols = append(cols, "created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP", "updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP")
	return strings.Join(cols, ",\n\t\t\t")
}

// camelCase identifier from user input: "blog_post" → "blogPost".
func toCamel(s string) string {
	words := splitWords(s)
	for i, w := range words {
		if i > 0 {
			words[i] = capitalize(w)
		}
	}
	return strings.Join(words, "")
}

func splitWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		out = append(out, w)
	}
	if len(out) == 0 {
		must(fmt.Errorf("invalid name %q", s))
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func titleWords(s string) string {
	words := splitWords(s)
	for i, w := range words {
		words[i] = capitalize(w)
	}
	return strings.Join(words, " ")
}

// naive plural: word + "s" unless overridden by --plural.
func pluralize(s string) string {
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	if strings.HasSuffix(s, "y") && len(s) > 1 && !isVowel(rune(s[len(s)-2])) {
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

func isVowel(r rune) bool {
	return strings.ContainsRune("aeiou", unicode.ToLower(r))
}

func readModule(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	must(err)
	re := regexp.MustCompile(`(?m)^module\s+(\S+)`)
	m := re.FindSubmatch(data)
	if m == nil {
		must(fmt.Errorf("no module directive in go.mod"))
	}
	return string(m[1])
}

// --- entity command ---------------------------------------------------------

func runEntity(args []string) {
	// Go's flag package stops parsing at the first positional argument,
	// but the ergonomic form is `entity task --field ...` (name first).
	// Pre-split: flags (with their values) to the front, positionals after.
	flagsOnly, positional := splitArgs(args, map[string]bool{"field": true, "plural": true, "icon": true})
	fsSet := flag.NewFlagSet("entity", flag.ExitOnError)
	pluralFlag := fsSet.String("plural", "", "plural form (default: naive +s)")
	iconFlag := fsSet.String("icon", "Inbox", "lucide icon component name for the nav item")
	fields := multiFlag{}
	fsSet.Var(&fields, "field", "field as name:string|text|bool|int (repeatable)")
	must(fsSet.Parse(flagsOnly))
	if len(positional) != 1 {
		usage()
	}
	singular := strings.ToLower(positional[0])
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(singular) {
		must(fmt.Errorf("entity name must be lowercase letters/digits/underscore, got %q", singular))
	}

	root := repoRoot()

	// Fail BEFORE any file is written if the skeleton markers are
	// missing — a half-applied generation is worse than none. (Markers
	// are protected by AGENTS.md, but formatters/merges can still eat
	// them; fail loudly, not silently.)
	for _, m := range [][2]string{
		{"internal/store/store.go", "--- gen:store-interfaces ---"},
		{"internal/store/db.go", "--- gen:migrations ---"},
		{"internal/server/routes.go", "--- gen:routes ---"},
		{"web/src/components/app-shell.tsx", "--- gen:nav-icons ---"},
		{"web/src/components/app-shell.tsx", "--- gen:nav ---"},
	} {
		data, err := os.ReadFile(filepath.Join(root, m[0]))
		if err != nil {
			must(fmt.Errorf("pre-check %s: %w", m[0], err))
		}
		if !strings.Contains(string(data), m[1]) {
			must(fmt.Errorf("pre-check: marker %q not found in %s — reconcile the skeleton before generating", m[1], m[0]))
		}
	}

	entity := Entity{
		Singular:    singular,
		Plural:      or(*pluralFlag, pluralize(singular)),
		Title:       titleWords(singular),
		TitlePlural: titleWords(or(*pluralFlag, pluralize(singular))),
		Icon:        *iconFlag,
		Module:      readModule(root),
	}
	for _, spec := range fields {
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 {
			must(fmt.Errorf("field %q must be name:string|text|bool|int", spec))
		}
		name, typ := toCamel(parts[0]), parts[1]
		switch typ {
		case "string", "text", "bool", "int":
		default:
			must(fmt.Errorf("field %q: unknown type %q (want string|text|bool|int)", spec, typ))
		}
		entity.Fields = append(entity.Fields, Field{Name: name, JSONName: name, Type: typ, Title: titleWords(parts[0])})
	}
	if len(entity.Fields) == 0 {
		entity.Fields = []Field{
			{Name: "title", JSONName: "title", Type: "string", Title: "Title"},
			{Name: "body", JSONName: "body", Type: "text", Title: "Body"},
		}
	}

	generated := map[string][]byte{}
	var err error
	generated[filepath.Join("internal", "store", entity.Plural+".go")], err = render(parseTemplates(), "store.go.tmpl", entity)
	must(err)
	generated[filepath.Join("internal", "server", "handlers_"+entity.Plural+".go")], err = render(parseTemplates(), "handlers.go.tmpl", entity)
	must(err)
	generated[filepath.Join("web", "src", "lib", "api", entity.Plural+".ts")], err = render(parseTemplates(), "api.ts.tmpl", entity)
	must(err)
	generated[filepath.Join("web", "src", "app", entity.Plural, "page.tsx")], err = render(parseTemplates(), "page.tsx.tmpl", entity)
	must(err)
	for path, content := range generated {
		mustWrite(filepath.Join(root, path), content)
		fmt.Printf("  wrote %s\n", path)
	}

	// Skeleton insertions at markers.
	patch(root, filepath.Join("internal", "store", "store.go"),
		"--- gen:store-interfaces ---", "\t"+entity.TitleCamel()+"Store")
	patch(root, filepath.Join("internal", "store", "db.go"),
		"--- gen:migrations ---", migrationDDL(entity))
	patch(root, filepath.Join("internal", "server", "routes.go"),
		"--- gen:routes ---", routesBlock(entity))
	patch(root, filepath.Join("web", "src", "components", "app-shell.tsx"),
		"--- gen:nav-icons ---", "\t"+entity.Icon+",")
	patch(root, filepath.Join("web", "src", "components", "app-shell.tsx"),
		"--- gen:nav ---", fmt.Sprintf(`      { href: "/%s/", label: "%s", icon: %s },`, entity.Plural, entity.TitlePlural, entity.Icon))

	fmt.Printf(`
entity %q generated (%d fields). Next steps:
  go build ./... && go vet ./...
  cd web && pnpm build        # or: make build-web
  make build                  # full single binary

Manual recipe: docs/agent/add-entity.md
`, entity.Singular, len(entity.Fields))
}

// TitleCamel is the exported Go identifier: "task" → "Task".
func (e Entity) TitleCamel() string { return capitalize(e.Singular) }

// TitleCamelLower is the lowercase Go identifier: "task" → "taskDTO".
func (e Entity) TitleCamelLower() string { return e.Singular }

func migrationDDL(e Entity) string {
	// Single-user tables: no user_id index to create.
	return fmt.Sprintf("\t\t// %s (generated by `generator entity`)\n\t\t`CREATE TABLE IF NOT EXISTS %s (\n\t\t\t%s\n\t\t)`,",
		e.Plural, e.Plural, e.SQLColumns())
}

func routesBlock(e Entity) string {
	t, n := e.TitleCamel(), e.Plural
	// Rows for the declarative route table. Every route is reachable:
	// there is no auth level any more (AGENTS.md rule 2).
	return fmt.Sprintf(`
	// %[1]s (generated).
	{http.MethodGet, "/api/%[2]s", s.handleList%[1]s},
	{http.MethodPost, "/api/%[2]s", s.handleCreate%[1]s},
	{http.MethodGet, "/api/%[2]s/{id}", s.handleGet%[1]s},
	{http.MethodPut, "/api/%[2]s/{id}", s.handleUpdate%[1]s},
	{http.MethodDelete, "/api/%[2]s/{id}", s.handleDelete%[1]s},`, t, n)
}

func render(t *template.Template, name string, data any) ([]byte, error) {
	var sb strings.Builder
	if err := t.ExecuteTemplate(&sb, name, data); err != nil {
		return nil, err
	}
	out := []byte(sb.String())
	if strings.HasSuffix(name, ".go.tmpl") {
		formatted, ferr := format.Source(out)
		if ferr == nil {
			out = formatted
		} else {
			return nil, fmt.Errorf("generated %s does not compile (template bug): %w", name, ferr)
		}
	}
	return out, nil
}

// patch inserts insertion after marker in the file at path (once).
// Idempotent: if the same insertion is already present, it is skipped —
// re-running a partially applied generation must not duplicate lines.
//
// Go files are gofmt'd after the insert: the insertion snippets are written
// with the indentation of the marker, which is not always the indentation
// the surrounding literal wants, so without this a freshly generated entity
// leaves `gofmt -l` dirty and the next `make lint` fails on a file the agent
// never hand-edited.
func patch(root, rel, marker, insertion string) {
	path := filepath.Join(root, rel)
	data, err := os.ReadFile(path)
	must(err)
	src := string(data)
	if strings.Contains(src, strings.TrimSpace(insertion)) {
		fmt.Printf("  already patched %s (skipped)\n", rel)
		return
	}
	updated, err := insertAfter(src, marker, insertion)
	must(err)

	out := []byte(updated)
	if strings.HasSuffix(rel, ".go") {
		if formatted, ferr := format.Source(out); ferr == nil {
			out = formatted
		} else {
			// A snippet that does not parse is a template bug; failing here
			// points at the entity being generated instead of at a
			// confusing compile error in the skeleton.
			must(fmt.Errorf("patched %s does not compile (template bug): %w", rel, ferr))
		}
	}
	mustWrite(path, out)
	fmt.Printf("  patched %s\n", rel)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// splitArgs separates flag tokens (and their values, for the flags in
// valueFlags) from positional arguments, regardless of order.
func splitArgs(args []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return flags, positional
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- init command -----------------------------------------------------------

var moduleRe = regexp.MustCompile(`^[a-zA-Z0-9._~/-]+$`)

// initReplacements builds the rebrand rules: module path, display name,
// logo letter, and the kebab-case slug.
func initReplacements(module, name string) []struct{ from, to string } {
	slug := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), "_", "-"))
	letter := strings.ToUpper(string([]rune(titleWords(name))[0]))
	return []struct{ from, to string }{
		{"github.com/Potterluo/docker-pull-tar", module},
		{"dockerpull", slug},
		{"DockerPull", name},
		{">D<", ">" + letter + "<"},
	}
}

func runInit(args []string) {
	fsSet := flag.NewFlagSet("init", flag.ExitOnError)
	module := fsSet.String("module", "", "new go module path, e.g. github.com/you/myapp")
	name := fsSet.String("name", "", "display name, e.g. MyApp")
	markdown := fsSet.String("markdown", "full", "markdown build: full (Shiki highlighting, +11MB) or lite")
	must(fsSet.Parse(args))

	reader := bufio.NewReader(os.Stdin)
	if *module == "" {
		fmt.Print("go module path (e.g. github.com/you/myapp): ")
		line, _ := reader.ReadString('\n')
		*module = strings.TrimSpace(line)
	}
	if *name == "" {
		fmt.Print("display name (e.g. MyApp): ")
		line, _ := reader.ReadString('\n')
		*name = strings.TrimSpace(line)
	}
	if !moduleRe.MatchString(*module) || *module == "github.com/Potterluo/docker-pull-tar" {
		must(fmt.Errorf("invalid module path %q", *module))
	}
	if *name == "" {
		must(fmt.Errorf("display name is required"))
	}
	if *markdown != "full" && *markdown != "lite" {
		must(fmt.Errorf("--markdown must be full or lite"))
	}

	root := repoRoot()
	current := readModule(root)
	if current != "github.com/Potterluo/docker-pull-tar" {
		must(fmt.Errorf("this project is already initialized (module %q)", current))
	}

	// Walk the repo, skipping build artifacts and vendored code.
	skipNames := map[string]bool{".git": true, "node_modules": true, "out": true, ".next": true, "dist": true, "bin": true, ".shots": true, "data": true}
	replacements := initReplacements(*module, *name)
	changed := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		switch ext {
		case ".go", ".mod", ".json", ".tsx", ".ts", ".md", ".yml", ".yaml", ".mjs", ".ps1", "":
		default:
			return nil
		}
		if strings.HasSuffix(path, "go.sum") || strings.HasSuffix(path, "pnpm-lock.yaml") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(data)
		out := src
		for _, r := range replacements {
			out = strings.ReplaceAll(out, r.from, r.to)
		}
		if out != src {
			mustWrite(path, []byte(out))
			rel, _ := filepath.Rel(root, path)
			changed[rel] = true
		}
		return nil
	})
	must(err)

	// Persist the markdown build choice as the Makefile default, so a
	// later plain `make build` honors it (explicit MARKDOWN= overrides).
	if *markdown == "lite" {
		mkPath := filepath.Join(root, "Makefile")
		mk, err := os.ReadFile(mkPath)
		must(err)
		if !strings.Contains(string(mk), "MARKDOWN ?= lite") {
			updated := strings.Replace(string(mk), "MARKDOWN ?= full", "MARKDOWN ?= lite", 1)
			mustWrite(mkPath, []byte(updated))
			fmt.Println("  set Makefile MARKDOWN ?= lite")
		}
	}

	// gofmt every touched Go file so formatting stays canonical.
	files := make([]string, 0, len(changed))
	for f := range changed {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if strings.HasSuffix(f, ".go") {
			src, _ := os.ReadFile(filepath.Join(root, f))
			formatted, ferr := format.Source(src)
			if ferr == nil {
				mustWrite(filepath.Join(root, f), formatted)
			}
		}
		fmt.Printf("  rewrote %s\n", f)
	}

	fmt.Printf(`
initialized: module=%s name=%s

Next steps:
  go build ./...                        # backend compiles under the new module
  cd web && pnpm install && pnpm build  # frontend (name appears in the UI)
  make build                            # full single binary
  git add -A && git commit -m "init"    # start your history

The generator markers (--- gen:* ---) are preserved, so
  go run ./cmd/generator entity <name> --field ...
keeps working in this project.
`, *module, *name)
}
