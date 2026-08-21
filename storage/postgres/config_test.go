package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// The schema-name validator's tests.
//
// This is the only injection surface storage/postgres has. A schema name cannot
// be a bind parameter — SQL has no parameter in an identifier position — so it
// is interpolated into DDL and into every statement the backend runs, and this
// validator is the whole of what stands between the configuration and the
// database. The tests below are correspondingly paranoid, and deliberately
// broader than the story's floor: the floor names quotes, semicolons,
// whitespace, dots, comment markers, over-length, empty, a non-identifier
// leading character and non-ASCII, and everything past that is here because a
// hostile value is not obliged to stay on the list somebody thought of.

// hostileSchemaNames is the rejection corpus. Each entry is a value that must
// NEVER become a schema qualifier, with the reason it is here.
var hostileSchemaNames = []struct {
	name  string
	value string
	why   string
}{
	// ---- the story's floor ----
	{"empty", "", "there is no schema named nothing; unset is spelled by passing no Option"},
	{"single quote", "public'", "closes a string literal"},
	{"double quote", `public"`, "closes a quoted identifier — the exact escape quoting exists to prevent"},
	{"double quote leading", `"public"`, "pre-quoted input; Aperture quotes, so this would double-quote"},
	{"semicolon", "public;", "ends the statement and starts another"},
	{"statement injection", "public; DROP SCHEMA public CASCADE; --", "the canonical payload"},
	{"quote break out", `public"; DROP SCHEMA public CASCADE; --`, "breaks out of the quoting, then injects"},
	{"space", "my schema", "whitespace splits one identifier into two tokens"},
	{"leading space", " public", "an operator's stray space is a boot failure, not a silent trim"},
	{"trailing space", "public ", "likewise, and the reason this value is not trimmed"},
	{"tab", "pub\tlic", "whitespace"},
	{"newline", "public\n", "whitespace, and a log-forging character"},
	{"embedded newline injection", "public\nDROP SCHEMA public;", "a newline is a statement separator to a script"},
	{"carriage return", "public\r", "whitespace"},
	{"vertical tab", "public\v", "whitespace"},
	{"form feed", "public\f", "whitespace"},
	{"dot", "public.apt_grants", "a dot makes the qualifier address a table, not a schema"},
	{"leading dot", ".public", "same, from the other end"},
	{"line comment", "public--", "comments out the rest of the statement"},
	{"block comment open", "public/*", "opens a comment the statement never closes"},
	{"block comment close", "public*/", "closes a comment that was never opened"},
	{"hash comment", "public#", "MySQL-style comment marker; not ours, still not an identifier"},
	{"over length", strings.Repeat("a", MaxSchemaNameLength+1), "PostgreSQL truncates at 63 bytes, silently retargeting"},
	{"far over length", strings.Repeat("a", 300), "same, unmistakably"},
	{"leading digit", "1public", "not an identifier start"},
	{"all digits", "42", "not an identifier start"},
	{"non-ascii latin", "schéma", "outside the accepted grammar; normalisation is a question with no good answer"},
	{"non-ascii cyrillic", "схема", "homoglyphs: this is not the schema it looks like"},
	{"non-ascii emoji", "public🙂", "outside the accepted grammar"},
	{"non-breaking space", "public ", "whitespace that does not look like whitespace"},
	{"zero width space", "public\u200b", "invisible: two configurations that render identically"},

	// ---- past the floor ----
	{"nul byte", "public\x00", "truncates the string for anything C-shaped downstream"},
	{"nul byte then payload", "public\x00; DROP SCHEMA public;", "the same trick with a tail"},
	{"bell", "public\a", "a control character on its way into a log file"},
	{"escape", "public\x1b[2J", "an ANSI escape sequence on its way into a terminal"},
	{"backslash", `public\`, "an escape character in some string syntaxes"},
	{"backslash escape", `public\"`, "an attempt at an escaped quote"},
	{"dollar", "public$", "legal in PostgreSQL identifiers, deliberately not accepted here"},
	{"dollar quoting", "$$public$$", "dollar quoting; also not an identifier start"},
	{"backtick", "public`", "an identifier quote in other dialects"},
	{"parenthesis", "public()", "call syntax"},
	{"comma", "public,other", "an argument separator"},
	{"asterisk", "public*", "not an identifier character"},
	{"percent", "public%", "a format verb on its way into a message, and not an identifier"},
	{"hyphen", "aperture-schema", "the most plausible honest mistake in the whole list"},
	{"slash", "public/other", "path syntax"},
	{"colon", "public:other", "not an identifier character"},
	{"brace template", "{{schema}}", "a template that was never rendered"},
	{"unicode fullwidth quote", "public＂", "a quote character that is not U+0022"},
	{"whitespace only", "   ", "not empty, so it reaches the validator, and must be refused there"},
	{"just an underscore is fine but not this", "_pub-lic", "an otherwise-valid name with one bad byte"},
}

// TestValidateSchemaNameRejectsHostileValues is the rejection set, and the
// criterion it satisfies is not "a regexp said no" — it is that every one of
// these values is refused with a coded error, through the exported API, before
// anything can be built from it.
func TestValidateSchemaNameRejectsHostileValues(t *testing.T) {
	for _, tc := range hostileSchemaNames {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSchemaName(tc.value)
			if err == nil {
				t.Fatalf("ValidateSchemaName(%q) accepted it; %s", tc.value, tc.why)
			}
			if code := aerr.CodeOf(err); code != aerr.APERTURE_CONFIG_INVALID {
				t.Errorf("refusal carries code %q, want APERTURE_CONFIG_INVALID", code)
			}

			// The same value must be refused through BOTH configuration paths,
			// not just the one the validator is called from directly.
			var set settings
			if err := WithSchema(tc.value)(&set); err == nil {
				t.Errorf("WithSchema(%q) accepted it", tc.value)
			}
			if set.validatedSchema != "" {
				t.Errorf("a refused WithSchema still wrote %q into the settings", set.validatedSchema)
			}
			if strings.ContainsRune(tc.value, 0) {
				// The OS itself refuses a NUL inside an environment variable
				// (setenv(3) returns EINVAL), so this value cannot arrive by that
				// route at all. It still has to be refused through WithSchema,
				// which is asserted above, because a library caller can hand one
				// over directly.
				return
			}
			t.Setenv(EnvSchema, tc.value)
			var envSet settings
			envErr := WithSchemaFromEnv()(&envSet)
			if tc.value == "" {
				// The one documented exception: an empty variable is "unset".
				if envErr != nil {
					t.Errorf("an empty %s must mean unset, not an error: %v", EnvSchema, envErr)
				}
			} else if envErr == nil {
				t.Errorf("%s=%q was accepted", EnvSchema, tc.value)
			}
			if envSet.validatedSchema != "" {
				t.Errorf("a refused %s still wrote %q into the settings", EnvSchema, envSet.validatedSchema)
			}
		})
	}
}

// TestValidateSchemaNameAcceptsOrdinaryNames is the other half: a validator that
// rejects everything is not a validator, it is an outage. These are the names
// operators actually use, plus the boundary.
func TestValidateSchemaNameAcceptsOrdinaryNames(t *testing.T) {
	good := []string{
		"aperture",
		"public",
		"apt",
		"aperture_authz",
		"_private",
		"a",
		"_",
		"s3",
		"Aperture",                               // accepted, and used case-exactly; see config.go's header
		"APERTURE",                               //
		"user",                                   // a reserved word, which is fine: the name is quoted
		"order",                                  //
		strings.Repeat("a", MaxSchemaNameLength), // exactly at the limit
	}
	for _, name := range good {
		if err := ValidateSchemaName(name); err != nil {
			t.Errorf("ValidateSchemaName(%q) refused an ordinary name: %v", name, err)
		}
	}
}

// TestSchemaNameValidatorMatchesItsDocumentedPattern keeps SchemaNamePattern
// honest. The constant is what the doc comment promises and what the refusal
// message quotes back at the operator, but the implementation is a hand-written
// loop — so without this test the two are free to drift, and the one that drifts
// is always the comment.
//
// The corpus is exhaustive where it can be: every byte in isolation and every
// byte in second position, which settles the first-character rule and the
// character set together, plus the length boundary and the whole hostile list.
func TestSchemaNameValidatorMatchesItsDocumentedPattern(t *testing.T) {
	pattern := regexp.MustCompile(SchemaNamePattern)
	var corpus []string
	for b := 0; b < 256; b++ {
		corpus = append(corpus, string([]byte{byte(b)}), "a"+string([]byte{byte(b)}))
	}
	for _, tc := range hostileSchemaNames {
		corpus = append(corpus, tc.value)
	}
	corpus = append(corpus,
		"", "a", "public", "Aperture", "_x9",
		strings.Repeat("a", MaxSchemaNameLength),
		strings.Repeat("a", MaxSchemaNameLength+1),
	)
	for _, s := range corpus {
		// The documented rule is the pattern AND the byte limit.
		want := pattern.MatchString(s) && len(s) <= MaxSchemaNameLength
		got := validateSchemaName("test", s) == nil
		if got != want {
			t.Errorf("validator and %s disagree on %q: validator accepts=%v, pattern accepts=%v",
				SchemaNamePattern, s, got, want)
		}
	}
}

// TestSchemaNameRefusalIsUsable checks the message an operator actually reads.
// A refusal that does not say what was wrong, what the rule is, or where the
// value came from turns a typo into a debugging session.
func TestSchemaNameRefusalIsUsable(t *testing.T) {
	t.Setenv(EnvSchema, "aperture; DROP SCHEMA public CASCADE; --")
	err := WithSchemaFromEnv()(&settings{})
	if err == nil {
		t.Fatalf("the payload was accepted")
	}
	msg := err.Error()
	for _, want := range []string{EnvSchema, SchemaNamePattern, "63"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %s", want, msg)
		}
	}
	// The offending value is echoed, because that is what makes the message
	// useful — but through %q, so a payload cannot forge a log line with a
	// newline or move a terminal cursor with an escape.
	if !strings.Contains(msg, `"aperture; DROP SCHEMA public CASCADE; --"`) {
		t.Errorf("the refusal does not quote the value back: %s", msg)
	}
	forging := "aperture\nFATAL: everything is fine"
	if got := ValidateSchemaName(forging).Error(); strings.Contains(got, "\n") {
		t.Errorf("a newline in the value survived into the message unescaped: %q", got)
	}
}

// ---- the qualifier, and how it is built ----

// TestQualifierForQuotesAndCarriesTheDot pins the substitution contract: the
// TRAILING DOT belongs to the qualifier, because the placeholder replaced is
// "apt_schema." with its dot. A qualifier that dropped it would build
// `"aperture"apt_grants`, and one that kept the dot on the empty case would
// build `.apt_grants`.
func TestQualifierForQuotesAndCarriesTheDot(t *testing.T) {
	if got := qualifierFor(""); got != "" {
		t.Errorf("an unconfigured schema produced qualifier %q, want empty", got)
	}
	if got := qualifierFor("aperture"); got != `"aperture".` {
		t.Errorf("qualifierFor(\"aperture\") = %q, want %q", got, `"aperture".`)
	}
	// Case is preserved, which is the correctness argument for quoting: an
	// unquoted qualifier would fold to `aperture` while the catalog lookups
	// asked pg_namespace about `Aperture`.
	if got := qualifierFor("Aperture"); got != `"Aperture".` {
		t.Errorf("qualifierFor(\"Aperture\") = %q, want %q", got, `"Aperture".`)
	}
	// And the statements come out addressing it.
	s := &Store{schema: "Aperture", qualifier: qualifierFor("Aperture")}
	if got := s.q(`SELECT 1 FROM apt_schema.apt_grants`); got != `SELECT 1 FROM "Aperture".apt_grants` {
		t.Errorf("a pinned Store built %q", got)
	}
}

// TestQuoteSchemaIdentDoublesQuotes exercises the branch no accepted value can
// reach. That is the point of testing it: quoting is written to be correct on
// its own so that it is still a real control if the validator is ever loosened,
// and a control nobody tests is a control nobody notices the loss of.
func TestQuoteSchemaIdentDoublesQuotes(t *testing.T) {
	if got := quoteSchemaIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteSchemaIdent(`a\"b`) = %q, want %q", got, `"a""b"`)
	}
	if got := quoteSchemaIdent(`"; DROP SCHEMA public; --`); strings.Count(got, `"`)%2 != 0 {
		t.Errorf("quoting left an unbalanced quote: %q", got)
	}
}

// TestCreateSchemaStatementIsQuoted asserts the DDL Setup would actually send.
func TestCreateSchemaStatementIsQuoted(t *testing.T) {
	if got := createSchemaStatement("aperture"); got != `CREATE SCHEMA IF NOT EXISTS "aperture"` {
		t.Errorf("createSchemaStatement = %q", got)
	}
}

// ---- Open ----

// TestOpenRefusesABadSchemaBeforeOpeningAnything is the "fails at boot, not at
// query time" criterion, stated as behaviour: Open returns the coded error, and
// it returns NO Store — so there is nothing holding a pool and nothing for a
// caller to accidentally use.
func TestOpenRefusesABadSchemaBeforeOpeningAnything(t *testing.T) {
	s, err := Open("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable",
		WithSchema("public; DROP SCHEMA public CASCADE; --"))
	if err == nil {
		t.Fatalf("Open accepted an injectable schema name")
	}
	if s != nil {
		t.Errorf("Open returned a Store alongside the refusal")
	}
	if code := aerr.CodeOf(err); code != aerr.APERTURE_CONFIG_INVALID {
		t.Errorf("Open's refusal carries %q, want APERTURE_CONFIG_INVALID", code)
	}
}

// TestOpenWithNoSchemaOptionIsAmbient is the zero-config path: unchanged from
// before this knob existed, which is what keeps the apt_ prefix doing its job.
func TestOpenWithNoSchemaOptionIsAmbient(t *testing.T) {
	t.Setenv(EnvSchema, "should_be_ignored_without_the_option")
	s, err := Open("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Schema() != "" {
		t.Errorf("Schema() = %q with no Option passed, want empty — Open must not read the environment on its own", s.Schema())
	}
	if s.qualifier != "" {
		t.Errorf("qualifier = %q, want empty", s.qualifier)
	}
	if got := s.q(`SELECT 1 FROM apt_schema.apt_grants`); got != `SELECT 1 FROM apt_grants` {
		t.Errorf("the ambient store built %q, want an unqualified statement", got)
	}
}

func TestOpenWithSchemaFromEnv(t *testing.T) {
	t.Setenv(EnvSchema, "Aperture_Authz")
	s, err := Open("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable", WithSchemaFromEnv())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Schema() != "Aperture_Authz" {
		t.Errorf("Schema() = %q, want %q", s.Schema(), "Aperture_Authz")
	}
	if s.qualifier != `"Aperture_Authz".` {
		t.Errorf("qualifier = %q", s.qualifier)
	}
}

func TestOpenWithSchemaFromEnvUnsetIsAmbient(t *testing.T) {
	t.Setenv(EnvSchema, "")
	s, err := Open("postgres://nobody@127.0.0.1:1/nothing?sslmode=disable", WithSchemaFromEnv())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Schema() != "" {
		t.Errorf("an empty %s produced schema %q, want the ambient search_path", EnvSchema, s.Schema())
	}
}

// TestAtomicChildInheritsTheSchema covers the field pair travelling together. A
// child that took the qualifier and not the name would build correct statements
// and resolve the wrong schema — the kind of split that only shows up in the one
// call site that uses the other half.
func TestAtomicChildInheritsTheSchema(t *testing.T) {
	root := &Store{schema: "aperture", qualifier: qualifierFor("aperture")}
	child := &Store{pool: nil, exec: root.exec, schema: root.schema, qualifier: root.qualifier}
	if child.schema != root.schema || child.qualifier != root.qualifier {
		t.Fatalf("the child did not inherit the pair: %q/%q vs %q/%q",
			child.schema, child.qualifier, root.schema, root.qualifier)
	}
	if child.qualifier != qualifierFor(child.schema) {
		t.Errorf("the child's qualifier %q does not derive from its schema %q", child.qualifier, child.schema)
	}
}

// ---- the structural gates ----

// TestQuotingIsTotal is the claim that makes quoting an INDEPENDENT control
// rather than a cosmetic second opinion: for ANY input whatsoever — every byte,
// every payload in the hostile corpus, values the validator would never let
// through — quoteSchemaIdent produces exactly one syntactically closed quoted
// identifier. The quotes balance, and no '"' inside the name is left single.
//
// This is why the package can honestly claim two controls. If the validator were
// deleted tomorrow, a payload would still arrive at the server as a schema NAME
// (one that does not exist, refused with 3F000) rather than as SQL.
func TestQuotingIsTotal(t *testing.T) {
	var corpus []string
	for b := 0; b < 256; b++ {
		corpus = append(corpus, string([]byte{byte(b)}), "a"+string([]byte{byte(b)})+"b")
	}
	for _, tc := range hostileSchemaNames {
		corpus = append(corpus, tc.value)
	}
	for _, v := range corpus {
		got := quoteSchemaIdent(v)
		if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) || len(got) < 2 {
			t.Fatalf("quoteSchemaIdent(%q) = %q is not a quoted identifier", v, got)
		}
		// Strip the outer quotes; every remaining '"' must be part of a doubled
		// pair, which is the only way a quoted identifier can contain one.
		inner := got[1 : len(got)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] != '"' {
				continue
			}
			if i+1 >= len(inner) || inner[i+1] != '"' {
				t.Fatalf("quoteSchemaIdent(%q) = %q leaves a lone quote at %d, which closes the identifier early", v, got, i+1)
			}
			i++ // consume the pair
		}
	}
}

// TestEverySchemaQualifierIsDerivedFromQualifierFor is the claim the unit tests
// above cannot make on their own.
//
// They prove the validator refuses hostile values and the quoting contains
// anything it might miss. They do not prove that a string has no OTHER route to
// Store.qualifier — and Store.qualifier is the one value this package splices
// into SQL text, in q() for every statement and in applySchema for the whole
// embedded script.
//
// So this parses the package's non-test source with go/ast and checks every
// place Store.qualifier is written, requiring each to be either qualifierFor(x)
// — the sole quoting constructor — or a copy of another Store's qualifier. A
// literal, a concatenation, or a "temporary" test hook is build-red here rather
// than an injection hole that reads plausibly.
//
// Where a literal Store sets qualifier from qualifierFor(x), it must set schema
// from the SAME x. A Store whose two fields named different schemas would build
// statements against one and resolve the catalog against the other.
//
// House shape: parse with a real parser, never grep — the same discipline
// sqlprovider/values_test.go and storage/sqlite/schema_naming_test.go follow.
func TestEverySchemaQualifierIsDerivedFromQualifierFor(t *testing.T) {
	fset, files := parsePackageSource(t)

	writes := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				id, ok := node.Type.(*ast.Ident)
				if !ok || id.Name != "Store" {
					return true
				}
				fields := map[string]ast.Expr{}
				for _, elt := range node.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							fields[key.Name] = kv.Value
						}
					}
				}
				q, has := fields["qualifier"]
				if !has {
					return true
				}
				writes++
				if !derivesFromQualifierFor(q) {
					t.Errorf("%s: a Store literal sets qualifier from %s; it must be qualifierFor(...) or another Store's qualifier",
						fset.Position(node.Pos()), exprString(q))
					return true
				}
				// The pair must name one schema.
				if call, ok := q.(*ast.CallExpr); ok {
					if len(call.Args) != 1 {
						t.Errorf("%s: qualifierFor called with %d arguments", fset.Position(call.Pos()), len(call.Args))
						return true
					}
					arg := exprString(call.Args[0])
					got, set := fields["schema"]
					if !set || exprString(got) != arg {
						t.Errorf("%s: a Store literal builds its qualifier from %s but sets schema to %s; the pair must name one schema",
							fset.Position(node.Pos()), arg, exprString(got))
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "qualifier" || i >= len(node.Rhs) {
						continue
					}
					writes++
					if !derivesFromQualifierFor(node.Rhs[i]) {
						t.Errorf("%s: qualifier is assigned from %s; it must be qualifierFor(...) or another Store's qualifier",
							fset.Position(node.Pos()), exprString(node.Rhs[i]))
					}
				}
			}
			return true
		})
	}
	// Anti-vacuity: Open builds one and Atomic's child copies one. A scan that
	// suddenly finds none has stopped parsing, not stopped needing to.
	if writes < 2 {
		t.Fatalf("the scan found only %d writes to Store.qualifier; it is broken, not the code", writes)
	}
}

func derivesFromQualifierFor(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		return ok && id.Name == "qualifierFor"
	case *ast.SelectorExpr:
		return v.Sel.Name == "qualifier"
	case *ast.BasicLit:
		// The ambient default is the one literal allowed: it is the ABSENCE of a
		// qualifier, so nothing is spliced into anything.
		return v.Kind == token.STRING && (v.Value == `""` || v.Value == "``")
	}
	return false
}

// TestEveryWriteToTheSettingsSchemaValidatesFirst closes the door one level up.
//
// settings.validatedSchema is named for its invariant, and Open trusts that name
// completely: whatever is in that field becomes the qualifier. So every function
// that writes to it must call validateSchemaName. A third Option added later
// that reads a name from a config file and assigns it directly is exactly the
// plausible-looking change this catches.
func TestEveryWriteToTheSettingsSchemaValidatesFirst(t *testing.T) {
	fset, files := parsePackageSource(t)

	writers := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			assigns, validates := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "validatedSchema" {
							assigns = true
						}
					}
				case *ast.CallExpr:
					if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "validateSchemaName" {
						validates = true
					}
				}
				return true
			})
			if !assigns {
				continue
			}
			writers++
			if !validates {
				t.Errorf("%s: %s writes settings.validatedSchema without calling validateSchemaName",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
	// WithSchema and WithSchemaFromEnv. Both, or the scan is broken.
	if writers < 2 {
		t.Fatalf("the scan found only %d writers of settings.validatedSchema; it is broken, not the code", writers)
	}
}

// parsePackageSource parses this package's non-test files, from DISK. It FAILS
// rather than skips when it finds nothing, so neither gate above can pass
// vacuously if a file is renamed or the scan is broken.
func parsePackageSource(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		if file.Name.Name != "postgres" {
			t.Fatalf("%s declares package %q, not postgres", name, file.Name.Name)
		}
		files = append(files, file)
	}
	// postgres.go, store.go, config.go, integrity.go, doc.go at least.
	if len(files) < 4 {
		t.Fatalf("the scan parsed only %d source files; it is broken, not the package", len(files))
	}
	return fset, files
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprString(v.Fun) + "(" + exprString1(v.Args) + ")"
	}
	return "an expression"
}

func exprString1(args []ast.Expr) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, exprString(a))
	}
	return strings.Join(parts, ", ")
}
