package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/openclaw/gogcli/internal/app"
)

// resolveSentinel selects parse-only mode when it is the first argument:
//
//	gog __resolve <argv...>
//
// The remaining arguments are preprocessed and parsed exactly as a normal
// invocation would be, and the result is printed as JSON instead of running a
// command. Policy wrappers use this to make decisions on gog's own parse rather
// than on a reimplementation of its grammar.
//
// The sentinel is deliberately not a Kong command: baked safety profiles and
// command allowlists cannot block it, and it never appears in help or schema.
const resolveSentinel = "__resolve"

// resolveVersion is the version of the JSON contract emitted by __resolve.
// Bump it on any incompatible change to resolveDoc.
const resolveVersion = 1

type resolveDoc struct {
	ResolveVersion int                 `json:"resolve_version"`
	Build          string              `json:"build"`
	Args           []string            `json:"args"`
	Command        resolveCommand      `json:"command"`
	Positionals    []resolvePositional `json:"positionals"`
	Flags          []resolveFlag       `json:"flags"`
	Help           bool                `json:"help"`
	Version        bool                `json:"version"`
	HomeOverride   *string             `json:"home_override"`
	GogEnv         []string            `json:"gog_env"`
	BakedProfile   resolveVerdict      `json:"baked_profile"`
	CommandRules   resolveVerdict      `json:"command_rules"`
	Error          *resolveError       `json:"error"`
}

type resolveCommand struct {
	Path   []string `json:"path"`
	Dotted string   `json:"dotted"`
}

type resolvePositional struct {
	Name    string `json:"name"`
	Value   any    `json:"value"`
	Type    string `json:"type"`
	TagType string `json:"tag_type"`
	Set     bool   `json:"set"`
}

type resolveFlag struct {
	Name    string `json:"name"`
	Value   any    `json:"value"`
	Type    string `json:"type"`
	TagType string `json:"tag_type"`
	Hidden  bool   `json:"hidden,omitempty"`
	Source  string `json:"source"`
}

type resolveVerdict struct {
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`
	Blocked bool   `json:"blocked"`
	Message string `json:"message,omitempty"`
}

type resolveError struct {
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
}

const (
	resolveSourceArg     = "arg"
	resolveSourceEnv     = "env"
	resolveSourceDefault = "default"
	resolveSourceLocked  = "locked"
)

// preprocessArgs applies gog's argv rewrites that depend on the command model.
// Both normal execution and __resolve must call it so they parse identical argv.
func preprocessArgs(model *kong.Application, args []string) []string {
	args = rewriteDocsCellUpdateContentArgs(model, args)
	return rewriteDesirePathArgs(model, args)
}

// executeResolve implements `gog __resolve <argv...>`. It always writes a JSON
// document to stdout; parse and policy failures are reported inside it.
func executeResolve(args []string, runtime *app.Runtime) error {
	runtime = normalizedRuntime(runtime)
	doc := resolveArgs(args)

	enc := json.NewEncoder(runtime.IO.Out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func resolveArgs(args []string) (doc resolveDoc) {
	doc = resolveDoc{
		ResolveVersion: resolveVersion,
		Build:          VersionString(),
		Command:        resolveCommand{Path: []string{}},
		Positionals:    []resolvePositional{},
		Flags:          []resolveFlag{},
		GogEnv:         resolveGogEnv(),
	}

	// Mirror executeWithRuntime's pre-parser handling.
	if len(args) == 0 {
		args = []string{"--help"}
	}
	args = rewriteHelpArgs(args)
	if home, provided := preScanHomeArg(args); provided {
		doc.HomeOverride = &home
	}

	// baseDescription, not helpDescription: the latter reads runtime config, and
	// help text is irrelevant because Kong's writers are discarded.
	parser, _, err := newParserWithWriters(baseDescription(), io.Discard, io.Discard)
	if err != nil {
		doc.Error = &resolveError{Kind: "internal", Message: err.Error(), ExitCode: 1}
		return doc
	}
	// Execution refuses to start when a baked profile locks a flag the binary
	// does not have; report that the same way.
	if err = verifyLockedFlagsExist(parser.Model.Node); err != nil {
		doc.Error = &resolveError{Kind: "locked_flags", Message: resolveErrorMessage(err), ExitCode: 2}
		return doc
	}
	args = preprocessArgs(parser.Model, args)
	doc.Args = append([]string{}, args...)

	if resolveIsHelp(args) {
		// Kong would print help and exit during Parse. Report the command the
		// help request is for instead.
		doc.Help = true
		doc.Command = resolveCommandFor(commandNodeBefore(parser.Model.Node, args[:len(args)-1]))
		return doc
	}

	var kctx *kong.Context
	func() {
		defer func() {
			if r := recover(); r != nil {
				ep, ok := r.(exitPanic)
				if !ok {
					panic(r)
				}
				// Only hook flags such as --version exit during Parse.
				doc.Version = resolveHasFlagToken(args, "--version")
				if !doc.Version {
					doc.Error = &resolveError{Kind: "exit", Message: "parser exited", ExitCode: ep.code}
				}
			}
		}()
		kctx, err = parser.Parse(args)
	}()
	if doc.Version || doc.Error != nil {
		return doc
	}

	if err != nil {
		var parseErr *kong.ParseError
		if errors.As(err, &parseErr) {
			kctx = parseErr.Context
		}
		doc.Error = &resolveError{Kind: "parse", Message: resolveErrorMessage(err), ExitCode: 2}
		if kctx != nil {
			doc.Command = resolveCommandFor(kctx.Selected())
		}
		return doc
	}

	doc.Command = resolveCommandFor(kctx.Selected())

	profile, profileErr := loadBakedSafetyProfile()
	if profileErr == nil {
		doc.BakedProfile.Enabled = profile.enabled
		doc.BakedProfile.Name = profile.name
	}
	if blockErr := enforceBakedSafetyProfile(kctx); blockErr != nil {
		doc.BakedProfile.Blocked = true
		doc.BakedProfile.Message = resolveErrorMessage(blockErr)
	}

	// enforceLockedFlags applies locked values to the parsed targets, so the flag
	// values reported below are the ones a command handler would see.
	lockErr := enforceLockedFlags(kctx)

	var rules RootFlags
	for _, flag := range kctx.Flags() {
		switch flag.Name {
		case "enable-commands":
			rules.EnableCommands = resolveString(flag.Target)
		case "enable-commands-exact":
			rules.EnableCommandsExact = resolveString(flag.Target)
		case "disable-commands":
			rules.DisableCommands = resolveString(flag.Target)
		}
	}
	doc.CommandRules.Enabled = rules.EnableCommands != "" || rules.EnableCommandsExact != "" || rules.DisableCommands != ""
	if ruleErr := enforceEnabledCommands(kctx, rules.EnableCommands, rules.EnableCommandsExact); ruleErr != nil {
		doc.CommandRules.Blocked = true
		doc.CommandRules.Message = resolveErrorMessage(ruleErr)
	} else if ruleErr := enforceDisabledCommands(kctx, rules.DisableCommands); ruleErr != nil {
		doc.CommandRules.Blocked = true
		doc.CommandRules.Message = resolveErrorMessage(ruleErr)
	}

	if selected := kctx.Selected(); selected != nil {
		for _, positional := range selected.Positional {
			doc.Positionals = append(doc.Positionals, resolvePositional{
				Name:    positional.Name,
				Value:   resolveJSONValue(positional.Target),
				Type:    positional.Target.Type().String(),
				TagType: resolveTagType(positional.Tag),
				Set:     positional.Set,
			})
		}
	}

	seen := make(map[string]bool)
	for _, flag := range kctx.Flags() {
		if seen[flag.Name] {
			continue
		}
		seen[flag.Name] = true
		doc.Flags = append(doc.Flags, resolveFlag{
			Name:    flag.Name,
			Value:   resolveJSONValue(flag.Target),
			Type:    flag.Target.Type().String(),
			TagType: resolveTagType(flag.Tag),
			Hidden:  flag.Hidden,
			Source:  resolveFlagSource(kctx, flag),
		})
	}

	if lockErr != nil {
		doc.Error = &resolveError{Kind: "locked_flags", Message: resolveErrorMessage(lockErr), ExitCode: 2}
	}
	return doc
}

// resolveIsHelp reports whether rewriteHelpArgs turned args into a help request.
// It leaves the help flag as the final argument, before any "--".
func resolveIsHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	last := args[len(args)-1]
	if last != "--help" && last != "-h" {
		return false
	}
	for _, arg := range args[:len(args)-1] {
		if arg == "--" {
			return false
		}
	}
	return true
}

// resolveGogEnv lists the names (never values) of GOG_* environment variables.
// Some of them change behavior without being bound to a flag (for example
// GOG_ACCOUNT), so wrappers need to see that they are present.
func resolveGogEnv() []string {
	names := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GOG_") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func resolveHasFlagToken(args []string, name string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == name {
			return true
		}
	}
	return false
}

func resolveCommandFor(node *kong.Node) resolveCommand {
	path := []string{}
	for current := node; current != nil; current = current.Parent {
		if current.Type == kong.CommandNode {
			path = append(path, current.Name)
		}
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return resolveCommand{Path: path, Dotted: strings.Join(path, ".")}
}

func resolveFlagSource(kctx *kong.Context, flag *kong.Flag) string {
	if bakedSafetyEnabled() {
		if _, locked := bakedSafetyLockedFlag(flag.Name); locked {
			return resolveSourceLocked
		}
	}
	for _, trace := range kctx.Path {
		if trace.Flag == flag && !trace.Resolved {
			return resolveSourceArg
		}
	}
	for _, env := range flag.Envs {
		if _, ok := os.LookupEnv(env); ok {
			return resolveSourceEnv
		}
	}
	return resolveSourceDefault
}

func resolveTagType(tag *kong.Tag) string {
	if tag == nil {
		return ""
	}
	return tag.Type
}

func resolveString(target reflect.Value) string {
	if target.IsValid() && target.Kind() == reflect.String {
		return target.String()
	}
	return ""
}

// resolveJSONValue converts a parsed Kong target into a JSON-friendly value.
// Pointers are dereferenced (nil becomes null); values that do not marshal
// cleanly fall back to their fmt representation.
func resolveJSONValue(target reflect.Value) any {
	if !target.IsValid() {
		return nil
	}
	for target.Kind() == reflect.Pointer {
		if target.IsNil() {
			return nil
		}
		target = target.Elem()
	}
	if !target.CanInterface() {
		return nil
	}
	value := target.Interface()
	if _, err := json.Marshal(value); err != nil {
		return fmt.Sprint(value)
	}
	return value
}

func resolveErrorMessage(err error) string {
	var parseErr *kong.ParseError
	if errors.As(err, &parseErr) {
		return strings.TrimSpace(parseErr.Error())
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) && exitErr.Err != nil {
		return strings.TrimSpace(exitErr.Err.Error())
	}
	return strings.TrimSpace(err.Error())
}
