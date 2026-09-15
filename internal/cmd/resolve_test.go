package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"

	"github.com/openclaw/gogcli/internal/app"
)

// clearGogEnvAndSetHome unsets every pre-existing GOG_* environment variable
// (restoring it after the test) and then points GOG_HOME at a fresh temp
// directory via t.Setenv, so resolveGogEnv and any config/credential
// resolution never observe the ambient environment or need real
// credentials.
func clearGogEnvAndSetHome(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, "GOG_") {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetenv %s: %v", name, err)
		}
		t.Cleanup(func() { _ = os.Setenv(name, value) })
	}
	t.Setenv("GOG_HOME", t.TempDir())
}

// resolveFlagByName returns the flag with the given name from a resolveDoc,
// failing the test if it is missing.
func resolveFlagByName(t *testing.T, doc resolveDoc, name string) resolveFlag {
	t.Helper()
	for _, f := range doc.Flags {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("flag %q not found in %#v", name, doc.Flags)
	return resolveFlag{}
}

// resolvePositionalByName returns the positional with the given name from a
// resolveDoc, failing the test if it is missing.
func resolvePositionalByName(t *testing.T, doc resolveDoc, name string) resolvePositional {
	t.Helper()
	for _, p := range doc.Positionals {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("positional %q not found in %#v", name, doc.Positionals)
	return resolvePositional{}
}

// TestResolveArgs_ShortFlagClusterAndPointerFlag covers a positional-heavy
// leaf command with a combined short-flag cluster (-jn == --json --dry-run)
// and a *int64 flag, checking that pointer flags are dereferenced in the
// JSON value and reported with their pointer Go type.
func TestResolveArgs_ShortFlagClusterAndPointerFlag(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"docs", "insert", "1AbCdEfGhIjKlMnOp", "hi", "--index=5", "-jn"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	if doc.Command.Dotted != "docs.insert" {
		t.Fatalf("dotted = %q", doc.Command.Dotted)
	}
	if got, want := doc.Command.Path, []string{"docs", "insert"}; !equalStrings(got, want) {
		t.Fatalf("path = %v, want %v", got, want)
	}

	docID := resolvePositionalByName(t, doc, "docId")
	if docID.Value != "1AbCdEfGhIjKlMnOp" || !docID.Set {
		t.Fatalf("docId = %#v", docID)
	}
	content := resolvePositionalByName(t, doc, "content")
	if content.Value != "hi" || !content.Set {
		t.Fatalf("content = %#v", content)
	}

	index := resolveFlagByName(t, doc, "index")
	if index.Type != "*int64" || index.Source != resolveSourceArg {
		t.Fatalf("index = %#v", index)
	}
	// resolveArgs is called in-process here, so the value is the native Go
	// type resolveJSONValue produced (int64), not the float64 a JSON
	// round-trip would give; TestExecuteResolve_Integration below exercises
	// the actual over-the-wire JSON encoding.
	if n, ok := index.Value.(int64); !ok || n != 5 {
		t.Fatalf("index value = %#v", index.Value)
	}

	jsonFlag := resolveFlagByName(t, doc, "json")
	if jsonFlag.Value != true || jsonFlag.Source != resolveSourceArg {
		t.Fatalf("json flag = %#v", jsonFlag)
	}
	dryRun := resolveFlagByName(t, doc, "dry-run")
	if dryRun.Value != true || dryRun.Source != resolveSourceArg {
		t.Fatalf("dry-run flag = %#v", dryRun)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestResolveArgs_AliasAndDefaultSubcommand covers a root alias ("doc" for
// "docs") landing on a command group whose default subcommand is explicit in
// the resolved path (default:"withargs" on DocsNamedRangesListCmd).
func TestResolveArgs_AliasAndDefaultSubcommand(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"doc", "named-range", "1AbCdEfGhIjKlMnOp"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	if doc.Command.Dotted != "docs.named-range.list" {
		t.Fatalf("dotted = %q", doc.Command.Dotted)
	}
}

// TestResolveArgs_DashDashOnNonLeaf covers "--" appearing before a
// subcommand name on a non-leaf node: the tokens after "--" are still
// dispatched as a subcommand (not treated as positionals of the group),
// whether or not the resulting parse succeeds.
func TestResolveArgs_DashDashOnNonLeaf(t *testing.T) {
	clearGogEnvAndSetHome(t)

	t.Run("valid subcommand after --", func(t *testing.T) {
		doc := resolveArgs([]string{"docs", "comments", "--", "add", "ID", "hi"})
		if doc.Error != nil {
			t.Fatalf("unexpected error: %#v", doc.Error)
		}
		if doc.Command.Dotted != "docs.comments.add" {
			t.Fatalf("dotted = %q", doc.Command.Dotted)
		}
		docID := resolvePositionalByName(t, doc, "docId")
		if docID.Value != "ID" {
			t.Fatalf("docId = %#v", docID)
		}
		content := resolvePositionalByName(t, doc, "content")
		if content.Value != "hi" {
			t.Fatalf("content = %#v", content)
		}
	})

	t.Run("incomplete subcommand after -- still resolves the command path", func(t *testing.T) {
		doc := resolveArgs([]string{"docs", "named-range", "--", "create"})
		if doc.Error == nil {
			t.Fatalf("expected parse error")
		}
		if doc.Error.Kind != "parse" || doc.Error.ExitCode != 2 {
			t.Fatalf("error = %#v", doc.Error)
		}
		if doc.Command.Dotted != "docs.named-range.create" {
			t.Fatalf("dotted = %q", doc.Command.Dotted)
		}
	})
}

// TestResolveArgs_DashDashOnLeafMakesFlagLookingTokensPositional covers "--"
// on a leaf command: everything after it is positional, including tokens
// that look like flags, and the flag they resemble keeps its default value.
func TestResolveArgs_DashDashOnLeafMakesFlagLookingTokensPositional(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"docs", "insert", "ID", "--", "--index=9"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	content := resolvePositionalByName(t, doc, "content")
	if content.Value != "--index=9" {
		t.Fatalf("content = %#v", content)
	}
	index := resolveFlagByName(t, doc, "index")
	if index.Value != nil || index.Source != resolveSourceDefault {
		t.Fatalf("index = %#v", index)
	}
}

// TestResolveArgs_SliceFlagSplitsOnComma covers a []string flag ("-e") whose
// values are split on commas by Kong's default slice mapper.
func TestResolveArgs_SliceFlagSplitsOnComma(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"docs", "sed", "-e", "s/a,b/c/", "ID"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	expressions := resolveFlagByName(t, doc, "expressions")
	got, ok := expressions.Value.([]string)
	if !ok {
		t.Fatalf("expressions value type = %T (%#v)", expressions.Value, expressions.Value)
	}
	want := []string{"s/a", "b/c/"}
	if !equalStrings(got, want) {
		t.Fatalf("expressions = %#v, want %v", got, want)
	}
}

// TestResolveArgs_ParseErrors covers plain parse failures: an unexpected
// argument on a group whose subcommand does not exist, and an unknown flag
// on a leaf command that otherwise parses.
func TestResolveArgs_ParseErrors(t *testing.T) {
	clearGogEnvAndSetHome(t)

	tests := []struct {
		name        string
		args        []string
		wantDotted  string
		wantMessage string
	}{
		{
			name:        "unknown subcommand",
			args:        []string{"docs", "create-new-thing"},
			wantDotted:  "docs",
			wantMessage: "unexpected argument",
		},
		{
			name:        "unknown flag",
			args:        []string{"docs", "insert", "ID", "--bogus"},
			wantDotted:  "docs.insert",
			wantMessage: "unknown flag",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := resolveArgs(tc.args)
			if doc.Error == nil {
				t.Fatalf("expected parse error")
			}
			if doc.Error.Kind != "parse" || doc.Error.ExitCode != 2 {
				t.Fatalf("error = %#v", doc.Error)
			}
			if !strings.Contains(doc.Error.Message, tc.wantMessage) {
				t.Fatalf("message = %q, want substring %q", doc.Error.Message, tc.wantMessage)
			}
			if doc.Command.Dotted != tc.wantDotted {
				t.Fatalf("dotted = %q, want %q", doc.Command.Dotted, tc.wantDotted)
			}
		})
	}
}

// TestResolveArgs_Help covers every way a help request can reach __resolve:
// a trailing --help, a trailing -h, the "help" desire-path prefix, and empty
// argv (which gog treats as top-level --help). In all cases Help is true,
// Error is nil, and Command names whatever the help request was for.
func TestResolveArgs_Help(t *testing.T) {
	clearGogEnvAndSetHome(t)

	tests := []struct {
		name       string
		args       []string
		wantDotted string
	}{
		{name: "trailing --help", args: []string{"docs", "insert", "--help"}, wantDotted: "docs.insert"},
		{name: "help prefix", args: []string{"help", "docs", "insert"}, wantDotted: "docs.insert"},
		{name: "trailing -h", args: []string{"docs", "insert", "-h"}, wantDotted: "docs.insert"},
		{name: "empty argv", args: []string{}, wantDotted: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := resolveArgs(tc.args)
			if !doc.Help {
				t.Fatalf("expected Help=true")
			}
			if doc.Error != nil {
				t.Fatalf("unexpected error: %#v", doc.Error)
			}
			if doc.Command.Dotted != tc.wantDotted {
				t.Fatalf("dotted = %q, want %q", doc.Command.Dotted, tc.wantDotted)
			}
		})
	}
}

// TestResolveArgs_DashDashHelpIsPositionalNotHelp covers the boundary case
// for resolveIsHelp: "--help" after "--" on a leaf command is a literal
// positional value, not a help request.
func TestResolveArgs_DashDashHelpIsPositionalNotHelp(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"docs", "insert", "ID", "--", "--help"})
	if doc.Help {
		t.Fatalf("expected Help=false, got doc=%#v", doc)
	}
	content := resolvePositionalByName(t, doc, "content")
	if content.Value != "--help" {
		t.Fatalf("content = %#v", content)
	}
}

// TestResolveArgs_Version covers the --version hook flag, which panics with
// exitPanic during Parse; resolveArgs must recover it and report Version
// rather than a parser-exited error.
func TestResolveArgs_Version(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"--version"})
	if !doc.Version {
		t.Fatalf("expected Version=true, got doc=%#v", doc)
	}
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
}

// TestResolveArgs_HomeOverride covers the pre-parse --home scan: it must be
// reported when present and nil when absent, matching what
// bindRuntimeLayoutResolver would see during real execution.
func TestResolveArgs_HomeOverride(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"--home", "/tmp/x", "docs", "cat", "ID"})
	if doc.HomeOverride == nil || *doc.HomeOverride != "/tmp/x" {
		t.Fatalf("home_override = %v", doc.HomeOverride)
	}

	doc = resolveArgs([]string{"docs", "cat", "ID"})
	if doc.HomeOverride != nil {
		t.Fatalf("home_override = %v, want nil", *doc.HomeOverride)
	}
}

// TestResolveArgs_LastValueWins covers Kong's usual "last flag occurrence
// wins" behavior for a repeated bool flag with conflicting values.
func TestResolveArgs_LastValueWins(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"docs", "insert", "ID", "--no-input=false", "--no-input"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	noInput := resolveFlagByName(t, doc, "no-input")
	if noInput.Value != true || noInput.Source != resolveSourceArg {
		t.Fatalf("no-input = %#v", noInput)
	}
}

// TestResolveArgs_TagTypeExistingFile covers Kong's existingfile mapper: the
// tag_type is reported for a flag typed existingfile, and a path that does
// not exist fails at parse time (as a "parse" error, not a validation error
// resolveArgs invents itself).
func TestResolveArgs_TagTypeExistingFile(t *testing.T) {
	clearGogEnvAndSetHome(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "image.png")
	if err := os.WriteFile(path, []byte("fake-image"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	doc := resolveArgs([]string{"docs", "insert-image", "ID", "--file", path})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	file := resolveFlagByName(t, doc, "file")
	if file.TagType != "existingfile" {
		t.Fatalf("file tag_type = %q", file.TagType)
	}
	if file.Value != path {
		t.Fatalf("file value = %#v", file.Value)
	}

	missing := filepath.Join(dir, "does-not-exist.png")
	doc = resolveArgs([]string{"docs", "insert-image", "ID", "--file", missing})
	if doc.Error == nil {
		t.Fatalf("expected parse error for nonexistent existingfile path")
	}
	if doc.Error.Kind != "parse" {
		t.Fatalf("error kind = %q, want parse", doc.Error.Kind)
	}
}

// TestResolveArgs_FieldsDesirePathRewrite covers rewriteDesirePathArgs: a
// command with no local --fields flag has --fields rewritten to --select
// before parsing, while a command that declares its own --fields (an API
// field mask) keeps it untouched.
func TestResolveArgs_FieldsDesirePathRewrite(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"docs", "cat", "ID", "--fields", "x"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	if !resolveContainsString(doc.Args, "--select") {
		t.Fatalf("args = %v, expected rewritten --select", doc.Args)
	}
	sel := resolveFlagByName(t, doc, "select")
	if sel.Value != "x" {
		t.Fatalf("select = %#v", sel)
	}

	doc = resolveArgs([]string{"drive", "ls", "--fields", "id"})
	if doc.Error != nil {
		t.Fatalf("unexpected error: %#v", doc.Error)
	}
	if !resolveContainsString(doc.Args, "--fields") {
		t.Fatalf("args = %v, expected --fields preserved", doc.Args)
	}
	fields := resolveFlagByName(t, doc, "fields")
	if fields.Value != "id" {
		t.Fatalf("fields = %#v", fields)
	}
}

func resolveContainsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestResolveArgs_GogEnvListsNamesNotValues covers resolveGogEnv: it must
// report the sorted names of every GOG_* environment variable, and must
// never leak a value into the output.
func TestResolveArgs_GogEnvListsNamesNotValues(t *testing.T) {
	clearGogEnvAndSetHome(t)
	t.Setenv("GOG_ACCOUNT", "super-secret-value")

	doc := resolveArgs([]string{"version"})

	if !resolveContainsString(doc.GogEnv, "GOG_ACCOUNT") {
		t.Fatalf("gog_env = %v, want GOG_ACCOUNT", doc.GogEnv)
	}
	if !resolveContainsString(doc.GogEnv, "GOG_HOME") {
		t.Fatalf("gog_env = %v, want GOG_HOME", doc.GogEnv)
	}
	if !resolveStringsSorted(doc.GogEnv) {
		t.Fatalf("gog_env = %v, not sorted", doc.GogEnv)
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "super-secret-value") {
		t.Fatalf("encoded doc leaked an env value: %s", encoded)
	}
}

func resolveStringsSorted(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i-1] > values[i] {
			return false
		}
	}
	return true
}

// TestResolveArgs_EnvSource covers flag source attribution for a root flag
// with a declared env var (--access-token / GOG_ACCESS_TOKEN): the source is
// "env" when only the environment variable is set, and "arg" when the flag
// is also given on the command line (arg wins).
func TestResolveArgs_EnvSource(t *testing.T) {
	clearGogEnvAndSetHome(t)

	t.Run("from environment", func(t *testing.T) {
		t.Setenv("GOG_ACCESS_TOKEN", "abc123")
		doc := resolveArgs([]string{"docs", "cat", "ID"})
		if doc.Error != nil {
			t.Fatalf("unexpected error: %#v", doc.Error)
		}
		token := resolveFlagByName(t, doc, "access-token")
		if token.Source != resolveSourceEnv || token.Value != "abc123" {
			t.Fatalf("access-token = %#v", token)
		}
	})

	t.Run("arg wins over environment", func(t *testing.T) {
		t.Setenv("GOG_ACCESS_TOKEN", "abc123")
		doc := resolveArgs([]string{"docs", "cat", "ID", "--access-token", "xyz789"})
		if doc.Error != nil {
			t.Fatalf("unexpected error: %#v", doc.Error)
		}
		token := resolveFlagByName(t, doc, "access-token")
		if token.Source != resolveSourceArg || token.Value != "xyz789" {
			t.Fatalf("access-token = %#v", token)
		}
	})
}

// TestResolveArgs_BakedProfileBlocksCommand covers a baked safety profile
// that denies a specific command: the block is reported in BakedProfile
// without setting doc.Error, and positionals/flags are still populated as
// they would be for the command a wrapper needs to reason about.
func TestResolveArgs_BakedProfileBlocksCommand(t *testing.T) {
	clearGogEnvAndSetHome(t)
	withBakedSafetyProfile(t, `
name: blocklist
deny:
  - docs.insert
`)

	doc := resolveArgs([]string{"docs", "insert", "ID", "hi"})
	if !doc.BakedProfile.Enabled {
		t.Fatalf("expected baked_profile.enabled=true")
	}
	if !doc.BakedProfile.Blocked {
		t.Fatalf("expected baked_profile.blocked=true")
	}
	if doc.BakedProfile.Message == "" {
		t.Fatalf("expected non-empty baked_profile.message")
	}
	if doc.Error != nil {
		t.Fatalf("expected error=nil for a baked-profile block, got %#v", doc.Error)
	}
	docID := resolvePositionalByName(t, doc, "docId")
	if docID.Value != "ID" || !docID.Set {
		t.Fatalf("docId = %#v, want positionals populated despite the block", docID)
	}
}

// TestResolveArgs_LockedFlagSourceAndConflict covers a baked safety profile
// that locks a boolean flag: with no conflicting value the flag's source is
// "locked" and its value is the locked one, and a command-line value that
// disagrees with the lock is reported as a "locked_flags" error.
func TestResolveArgs_LockedFlagSourceAndConflict(t *testing.T) {
	clearGogEnvAndSetHome(t)
	const profile = `
name: locked
locked-flags:
  wrap-untrusted: true
docs:
  cat: true
`

	t.Run("locked value applies with no conflict", func(t *testing.T) {
		withBakedSafetyProfile(t, profile)
		doc := resolveArgs([]string{"docs", "cat", "ID"})
		if doc.Error != nil {
			t.Fatalf("unexpected error: %#v", doc.Error)
		}
		wrap := resolveFlagByName(t, doc, "wrap-untrusted")
		if wrap.Source != resolveSourceLocked || wrap.Value != true {
			t.Fatalf("wrap-untrusted = %#v", wrap)
		}
	})

	t.Run("conflicting command-line value is rejected", func(t *testing.T) {
		withBakedSafetyProfile(t, profile)
		doc := resolveArgs([]string{"docs", "cat", "ID", "--wrap-untrusted=false"})
		if doc.Error == nil {
			t.Fatalf("expected locked_flags error")
		}
		if doc.Error.Kind != "locked_flags" || doc.Error.ExitCode != 2 {
			t.Fatalf("error = %#v", doc.Error)
		}
	})
}

// TestResolveArgs_CommandRulesDisableCommands covers --disable-commands: the
// rule is reported as enabled and blocked in doc.CommandRules, matching
// enforceDisabledCommands's own decision.
func TestResolveArgs_CommandRulesDisableCommands(t *testing.T) {
	clearGogEnvAndSetHome(t)

	doc := resolveArgs([]string{"--disable-commands", "docs.insert", "docs", "insert", "ID"})
	if !doc.CommandRules.Enabled {
		t.Fatalf("expected command_rules.enabled=true")
	}
	if !doc.CommandRules.Blocked {
		t.Fatalf("expected command_rules.blocked=true")
	}
	if doc.CommandRules.Message == "" {
		t.Fatalf("expected non-empty command_rules.message")
	}
}

// TestResolveArgs_DispatchAgreesWithRealParse covers a corpus of valid argv
// (aliases, default subcommands, root desire-path shortcuts) and asserts
// that resolveArgs's reported command path is exactly what a real parse
// through the same preprocessing pipeline dispatches to. Policy wrappers
// depend on this agreement holding for every command, not just the ones
// exercised elsewhere in this file.
func TestResolveArgs_DispatchAgreesWithRealParse(t *testing.T) {
	clearGogEnvAndSetHome(t)

	corpus := [][]string{
		{"docs", "cat", "ID"},
		{"doc", "named-range", "ID"},
		{"ls"},
		{"send", "--to", "a@b.c", "--subject", "s", "--body", "b"},
		{"docs", "insert", "ID", "hi"},
		{"docs", "named-range", "create", "ID", "--name", "n"},
		{"drive", "ls"},
		{"gmail", "send", "--to", "a@b.c", "--subject", "s", "--body", "b"},
		{"calendar", "alias", "set", "work", "cal@x"},
		{"auth", "status"},
		{"version"},
		{"docs", "comments", "list", "ID"},
		{"docs", "cat", "ID", "--tab", "Sheet1"},
		{"me"},
		{"whoami"},
		{"status"},
	}

	for _, argv := range corpus {
		argv := argv
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			want := realDispatchDotted(t, argv)
			doc := resolveArgs(argv)
			if doc.Error != nil {
				t.Fatalf("resolveArgs error: %#v", doc.Error)
			}
			if doc.Command.Dotted != want {
				t.Fatalf("resolveArgs dotted = %q, real dispatch = %q", doc.Command.Dotted, want)
			}
		})
	}
}

// realDispatchDotted runs the same preprocessing + parse pipeline
// executeWithRuntime uses (minus everything after Parse) and returns the
// dotted command path the way enforceEnabledCommands/enforceDisabledCommands
// see it via commandPath(kctx.Command()), which lowercases and stops before
// any "<positional>" placeholder in Kong's rendered command string.
func realDispatchDotted(t *testing.T, argv []string) string {
	t.Helper()

	parser, _, err := newParserWithWriters(baseDescription(), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("newParserWithWriters: %v", err)
	}
	args := rewriteHelpArgs(append([]string{}, argv...))
	args = preprocessArgs(parser.Model, args)

	var kctx *kong.Context
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parser exited unexpectedly for %v: %v", argv, r)
			}
		}()
		kctx, err = parser.Parse(args)
	}()
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return strings.Join(commandPath(kctx.Command()), ".")
}

// TestExecuteResolve_Integration covers the __resolve dispatch wired through
// executeWithRuntime end to end (argv[0]=="__resolve" routes to
// executeResolve, which JSON-encodes resolveArgs's result to runtime.IO.Out)
// and confirms array-typed fields are always emitted as arrays, never as a
// JSON null, even when the parse itself fails.
func TestExecuteResolve_Integration(t *testing.T) {
	clearGogEnvAndSetHome(t)

	result := executeWithTestRuntime(t, []string{"__resolve", "docs", "cat", "ID"}, nil)
	if result.err != nil {
		t.Fatalf("executeWithTestRuntime: %v (stderr=%q)", result.err, result.stderr)
	}
	var doc resolveDoc
	if err := json.Unmarshal([]byte(result.stdout), &doc); err != nil {
		t.Fatalf("unmarshal: %v (stdout=%q)", err, result.stdout)
	}
	if doc.ResolveVersion != 1 {
		t.Fatalf("resolve_version = %d", doc.ResolveVersion)
	}
	if doc.Command.Dotted != "docs.cat" {
		t.Fatalf("dotted = %q", doc.Command.Dotted)
	}

	assertArrayFieldsNeverNull(t, result.stdout)

	// A parse failure must still emit arrays, not nulls, for every
	// array-typed field: wrappers decode this JSON unconditionally and must
	// not need special-case handling for the error path.
	errResult := executeWithTestRuntime(t, []string{"__resolve", "docs", "create-new-thing"}, nil)
	if errResult.err != nil {
		t.Fatalf("executeWithTestRuntime: %v (stderr=%q)", errResult.err, errResult.stderr)
	}
	var errDoc resolveDoc
	if err := json.Unmarshal([]byte(errResult.stdout), &errDoc); err != nil {
		t.Fatalf("unmarshal: %v (stdout=%q)", err, errResult.stdout)
	}
	if errDoc.Error == nil || errDoc.Error.Kind != "parse" {
		t.Fatalf("expected a parse error, got %#v", errDoc.Error)
	}
	assertArrayFieldsNeverNull(t, errResult.stdout)
}

// assertArrayFieldsNeverNull decodes raw into a map of json.RawMessage and
// asserts that "positionals", "flags", and "gog_env" are never the literal
// JSON null. Unmarshaling null into a Go slice silently yields a nil slice
// indistinguishable (by length) from an empty one, so the check is done on
// the raw bytes instead.
func assertArrayFieldsNeverNull(t *testing.T, raw string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("unmarshal raw fields: %v", err)
	}
	for _, name := range []string{"positionals", "flags", "gog_env"} {
		value, ok := fields[name]
		if !ok {
			t.Fatalf("field %q missing from %s", name, raw)
		}
		if strings.TrimSpace(string(value)) == "null" {
			t.Fatalf("field %q is JSON null, want an array: %s", name, raw)
		}
	}
}

// Sanity check that resolveArgs never panics into the test binary and that
// executeResolve encodes without escaping HTML characters (e.g. content
// containing "&" or "<"), matching the enc.SetEscapeHTML(false) call.
func TestExecuteResolve_DoesNotEscapeHTML(t *testing.T) {
	clearGogEnvAndSetHome(t)

	var buf bytes.Buffer
	runtime := &app.Runtime{IO: app.IO{In: strings.NewReader(""), Out: &buf, Err: io.Discard}}
	if err := executeResolve([]string{"docs", "insert", "ID", "a<b>&c"}, runtime); err != nil {
		t.Fatalf("executeResolve: %v", err)
	}
	if !strings.Contains(buf.String(), "a<b>&c") {
		t.Fatalf("expected literal HTML characters preserved, got: %s", buf.String())
	}
}

func TestResolveArgs_LockedFlagMissingFromBinary(t *testing.T) {
	clearGogEnvAndSetHome(t)
	withBakedSafetyProfile(t, "name: locks\ndocs: true\n")
	// Set directly: the profile parser may reject names it cannot map, but a
	// binary built from a mismatched profile can still carry such a lock.
	bakedSafetyTestProfile.lockedFlags = map[string]string{"no-such-flag": "true"}

	doc := resolveArgs([]string{"docs", "cat", "ID"})
	if doc.Error == nil || doc.Error.Kind != "locked_flags" || doc.Error.ExitCode != 2 {
		t.Fatalf("error = %#v, want locked_flags with exit code 2", doc.Error)
	}
	if len(doc.Positionals) != 0 || len(doc.Flags) != 0 {
		t.Fatalf("positionals/flags should be empty when the lock check fails before parsing")
	}
}
