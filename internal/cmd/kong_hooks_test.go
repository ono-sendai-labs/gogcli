package cmd

import (
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// This test guards the safety claim behind `gog __resolve`: that
// (*kong.Kong).Parse has no side effects (see internal/cmd/resolve.go).
// Kong invokes several kinds of user-defined methods, discovered purely by
// reflection, while it parses:
//
//   - Hook methods named BeforeReset / BeforeResolve / BeforeApply /
//     AfterApply, called from (*kong.Kong).applyHook (kong.go), which finds
//     them via callbacks.go getMethods -> walkEmbedded + getExplicitMethod.
//     walkEmbedded recurses into any exported struct field that is either
//     Go-anonymous OR tagged `embed:""`, so a hook can live on an embedded
//     sub-struct, not just directly on a command/flag/positional target.
//   - Validate, called from (*kong.Context).Validate (context.go), found via
//     getValidators -> walkEmbedded + isValidatable. isValidatable does an
//     interface assertion rather than a name lookup, but for a type we wrote
//     ourselves the only way that assertion succeeds is by defining a method
//     literally named Validate, so name-based discovery is exact here too.
//   - Decode (and IsBool, for BoolMapperValue), used by mapper.go when the
//     registry picks a mapper for a flag/positional's Target type: types
//     implementing kong.MapperValue / kong.BoolMapperValue.
//
// AfterRun is tracked informationally only: (*kong.Context).Run calls it
// after a command's Run() returns, which is after Parse has already
// finished, so it cannot violate __resolve's contract as of this kong
// version. It is here purely so a future kong upgrade that starts calling it
// earlier gets caught by this test.
//
// Resolvers and extra mappers (kong.Resolver, kong.TypeMapper,
// kong.NamedMapper options) also run code during Parse. newParserWithWriters
// (root.go) passes none; this test does not reflect over parser options, so
// adding one must be reviewed as a change to __resolve's side-effect contract.
var kongHookMethodNames = []string{
	"BeforeReset",
	"BeforeResolve",
	"BeforeApply",
	"AfterApply",
	"Validate",
	"Decode",
	"IsBool",
}

// kongInformationalMethodNames are checked and reported the same way, but are
// not hooks Parse can trigger. Kept separate so the allowlist comparison
// below never has to treat them as equivalent to a real Parse-time hook.
var kongInformationalMethodNames = []string{
	"AfterRun",
}

// gogModulePkgPrefix identifies types this test holds gog responsible for.
// Kong's own types (github.com/alecthomas/kong/...) and anything from the
// standard library or a third-party dependency are ignored: their hook
// behavior is kong's/the vendor's concern, not something a gog code change
// can introduce or remove.
const gogModulePkgPrefix = "github.com/openclaw/gogcli"

// gogParseHookAllowlist is every gog-defined method that this test has
// confirmed Kong may call during Parse (hook, Validate, or mapper Decode/
// IsBool), together with why calling it during a bare `__resolve` parse is
// safe. __resolve's "parsing has no side effects" promise depends on every
// entry here staying pure: no filesystem, network, environment, or shared
// mutable state.
//
// Adding an entry is a review decision, not a formality.
//
// At v0.40.0 gog defines no such methods. Upstream main later adds
// (*GmailDraftsCreateCmd).AfterApply and (*GmailDraftsUpdateCmd).AfterApply;
// when rebasing onto a release that has them, this test fails and they must be
// reviewed (they only validate already-parsed flags) and added here.
var gogParseHookAllowlist = map[string]string{
	// (currently empty -- see comment above)
}

// walkKongEmbedded mirrors kong's callbacks.go walkEmbedded exactly: visit v,
// then recurse into every exported field that is Go-anonymous or tagged
// `embed:""`. This is the traversal Kong itself performs (from
// getMethods/getValidators) when looking for hook/Validate methods on a
// command, flag, or positional target, so it must be reproduced faithfully
// rather than approximated -- e.g. CLI embeds RootFlags via an `embed:""`
// tag (see root.go), and any command that embeds a shared flag struct the
// same way needs its embedded methods discovered too.
func walkKongEmbedded(v reflect.Value, visit func(reflect.Value)) {
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() {
		return
	}
	visit(v)
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		_, isEmbedTag := field.Tag.Lookup("embed")
		if !isEmbedTag && !field.Anonymous {
			continue
		}
		walkKongEmbedded(v.Field(i), visit)
	}
}

// explicitMethodOwner mirrors kong's callbacks.go getExplicitMethod +
// isExplicitMethod: it reports whether name is defined directly on v's type
// (checking both the value type and, if v is addressable, the pointer type),
// excluding compiler-generated promotion wrappers for methods that actually
// live on a field embedded deeper inside v. Those deeper methods are still
// found -- walkKongEmbedded visits that field itself and this function is
// called on it directly -- so skipping the promoted wrapper here avoids
// reporting the same method under the wrong (outer) type as its owner.
func explicitMethodOwner(v reflect.Value, name string) (reflect.Type, bool) {
	if isExplicitMethodOnType(v.Type(), name) {
		return v.Type(), true
	}
	if v.CanAddr() && isExplicitMethodOnType(v.Addr().Type(), name) {
		return v.Addr().Type(), true
	}
	return nil, false
}

func isExplicitMethodOnType(t reflect.Type, name string) bool {
	method, ok := t.MethodByName(name)
	if !ok {
		return false
	}
	fn := runtime.FuncForPC(method.Func.Pointer())
	if fn == nil {
		return true
	}
	file, _ := fn.FileLine(method.Func.Pointer())
	return file != "<autogenerated>"
}

// hookOwnerName formats a discovered method as "pkgpath.TypeName.Method",
// dereferencing pointer types so *Foo and Foo report under the same owner.
func hookOwnerName(t reflect.Type, method string) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		name = t.String()
	}
	if t.PkgPath() == "" {
		return fmt.Sprintf("%s.%s", name, method)
	}
	return fmt.Sprintf("%s.%s.%s", t.PkgPath(), name, method)
}

// kongParseHookScan is the result of walking the whole Kong model looking for
// every method Parse could possibly invoke by reflection.
type kongParseHookScan struct {
	// all maps every discovered "owner.Method" name (any package) to true.
	all map[string]bool
	// gog maps discovered names owned by the gog module itself.
	gog map[string]bool
	// nodesVisited counts every kong.Node walked (commands and branching
	// arguments), including hidden ones.
	nodesVisited int
}

func scanKongParseHooks(t *testing.T, root *kong.Node) kongParseHookScan {
	t.Helper()
	scan := kongParseHookScan{all: map[string]bool{}, gog: map[string]bool{}}

	names := append(append([]string{}, kongHookMethodNames...), kongInformationalMethodNames...)

	record := func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		for _, name := range names {
			owner, ok := explicitMethodOwner(v, name)
			if !ok {
				continue
			}
			full := hookOwnerName(owner, name)
			scan.all[full] = true
			if strings.HasPrefix(owner.PkgPath(), gogModulePkgPrefix) {
				scan.gog[full] = true
			}
		}
	}

	var walkNode func(n *kong.Node)
	walkNode = func(n *kong.Node) {
		if n == nil {
			return
		}
		scan.nodesVisited++

		if n.Target.IsValid() {
			walkKongEmbedded(n.Target, record)
		}
		for _, value := range n.Values() {
			if value == nil || !value.Target.IsValid() {
				continue
			}
			walkKongEmbedded(value.Target, record)
		}

		for _, child := range n.Children {
			walkNode(child)
		}
	}
	walkNode(root)

	return scan
}

// TestKongParseHooksAreAllowlisted walks the real gog Kong model exactly the
// way (*kong.Kong).Parse would reflect over it, and fails if any gog-defined
// hook/Validate/mapper method shows up that isn't on the reviewed allowlist
// above. `gog __resolve` promises callers that Parse has no side effects;
// a new, unreviewed method on this list is exactly the kind of thing that
// promise cannot see and would silently break.
func TestKongParseHooksAreAllowlisted(t *testing.T) {
	parser, _, err := newParserWithWriters(baseDescription(), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("newParserWithWriters: %v", err)
	}

	scan := scanKongParseHooks(t, parser.Model.Node)

	// Sanity check #1: the walker must not pass vacuously. kong's own
	// `Version kong.VersionFlag` field on CLI (root.go) carries a real,
	// currently-existing BeforeReset hook (kong's util.go). It is
	// kong-owned so it must NOT end up in scan.gog, but it must show up in
	// scan.all -- if it doesn't, the traversal/reflection logic below this
	// comment is broken and every other assertion in this test is
	// meaningless.
	const knownKongHook = "github.com/alecthomas/kong.VersionFlag.BeforeReset"
	if !scan.all[knownKongHook] {
		t.Fatalf("walker sanity check failed: expected to discover %s (a real kong-owned hook on CLI.Version); "+
			"the walk/reflection logic is not finding real hooks at all", knownKongHook)
	}
	if scan.gog[knownKongHook] {
		t.Fatalf("walker sanity check failed: %s was recorded as gog-owned, but it is a kong package type; "+
			"the gogModulePkgPrefix filter is broken", knownKongHook)
	}

	// Sanity check #2: exercise walkKongEmbedded/explicitMethodOwner against
	// the KongHookFixture* types below (a hook reached only through an
	// `embed:""`-tagged field, a hook reached only through multiple levels
	// of Go-anonymous embedding, and promoted methods that must NOT be
	// attributed to the outer type). This is independent of gog's current
	// hook inventory (which is empty), so it keeps the test meaningful even
	// if nobody has defined a gog hook anywhere.
	var got []string
	walkKongEmbedded(reflect.ValueOf(KongHookFixtureTop{}), func(v reflect.Value) {
		for _, name := range []string{"BeforeApply", "Validate"} {
			if owner, ok := explicitMethodOwner(v, name); ok {
				got = append(got, hookOwnerName(owner, name))
			}
		}
	})
	sort.Strings(got)
	wantPrefix := "github.com/openclaw/gogcli/internal/cmd."
	want := []string{
		wantPrefix + "KongHookFixtureAnon.BeforeApply",
		wantPrefix + "KongHookFixtureDeep.Validate",
		wantPrefix + "KongHookFixtureTagged.BeforeApply",
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("walker fixture sanity check failed: walkKongEmbedded/explicitMethodOwner over "+
			"KongHookFixtureTop found:\n  %s\nwant exactly:\n  %s\n"+
			"(this means embed-tag traversal, anonymous-embedding traversal, or promoted-method "+
			"exclusion is broken)", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// Sanity check #3: the walker actually visits a large fraction of gog's
	// real command tree (a broken traversal that only looks at the root node
	// would otherwise still pass check #1 by coincidence if VersionFlag were
	// hoisted onto the root -- it is, so this independently confirms depth).
	if scan.nodesVisited <= 500 {
		t.Fatalf("expected to visit more than 500 kong.Node values (commands/arguments), got %d; "+
			"the node walker may not be recursing into Children", scan.nodesVisited)
	}

	// The real assertion: every gog-owned discovery must be pre-approved.
	var unexpected []string
	for name := range scan.gog {
		if _, ok := gogParseHookAllowlist[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Fatalf("parse-time hook(s) not allowlisted; __resolve promises Parse has no side effects -- "+
			"review each one and add it to gogParseHookAllowlist in kong_hooks_test.go if it is pure:\n  %s",
			strings.Join(unexpected, "\n  "))
	}

	// And every allowlist entry must still correspond to something real, so
	// the allowlist can't silently go stale and hide a removed hook's
	// absence from ever being noticed (or, worse, mask a typo that let an
	// actually-different method through).
	var stale []string
	for name := range gogParseHookAllowlist {
		if !scan.gog[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("gogParseHookAllowlist entries no longer found by the walker (stale; remove them):\n  %s",
			strings.Join(stale, "\n  "))
	}

	t.Logf("visited %d kong nodes; %d total hook/Validate/mapper methods discovered (any package); %d gog-owned",
		scan.nodesVisited, len(scan.all), len(scan.gog))
}

// KongHookFixture* types exist solely so TestKongParseHooksAreAllowlisted can
// prove walkKongEmbedded/explicitMethodOwner behave correctly, independent
// of whether gog currently defines any real hooks (it doesn't). The shape
// mirrors the two ways Kong flattens a target for hook discovery:
//
//	KongHookFixtureTop
//	├── KongHookFixtureAnon (Go-anonymous field)         -- defines BeforeApply
//	│   └── KongHookFixtureDeep (Go-anonymous field)     -- defines Validate
//	└── Tagged KongHookFixtureTagged `embed:""`          -- defines BeforeApply
//
// BeforeApply and Validate are promoted from KongHookFixtureAnon and
// KongHookFixtureDeep up to KongHookFixtureTop by the Go compiler; the test
// asserts those promoted wrappers are correctly excluded (owner stays the
// defining type, not KongHookFixtureTop), while the two `embed:""` /
// anonymous traversal paths are correctly followed.
type KongHookFixtureDeep struct{}

func (KongHookFixtureDeep) Validate() error { return nil }

type KongHookFixtureAnon struct{ KongHookFixtureDeep }

func (KongHookFixtureAnon) BeforeApply() error { return nil }

type KongHookFixtureTagged struct{}

func (KongHookFixtureTagged) BeforeApply() error { return nil }

type KongHookFixtureTop struct {
	KongHookFixtureAnon
	Tagged KongHookFixtureTagged `embed:""`
}
