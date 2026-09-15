# `gog __resolve`

`gog __resolve <argv...>` is a hidden, parse-only mode for policy wrappers —
sandbox proxies, approval gates, or anything else that must decide whether an
AI agent's `gog` invocation should run before it runs. It parses `<argv...>`
exactly as a normal `gog` invocation would, then prints a JSON document
describing the result instead of executing anything: no Google API call, no
file write, no OAuth flow, nothing observable happens.

`__resolve` exists so that wrappers make decisions from gog's own parse
instead of re-implementing gog's argument grammar (subcommand aliases,
default subcommands, flag negation, positional matching, `--` handling, and
so on) and inevitably drifting from it.

`__resolve` is a sentinel, not a Kong command: it is recognized before Kong
sees the argv at all. It never appears in `--help` or `gog schema`, and baked
safety profiles and `--enable-commands`/`--disable-commands` rules cannot
block it from running — those checks apply to the *resolved* command and are
reported inside the JSON document (see `baked_profile` and `command_rules`
below), not to the `__resolve` invocation itself.

## Invocation

```bash
gog __resolve <argv...>
```

`<argv...>` is whatever you would otherwise pass to `gog` — global flags,
command path, positionals, and command flags. `__resolve` always prints a
JSON document to stdout and always exits `0`, whether or not the parse
succeeded. Parse failures, help/version requests, and policy verdicts are all
reported as data inside the document; the wrapper decides what to do with
them.

## Example

Generated from a real build of the binary in this repository:

```bash
GOG_HOME=$(mktemp -d) gog __resolve docs insert 1AbCdEfGhIjKlMnOp "hello" --index=5
```

```json
{
  "resolve_version": 1,
  "build": "v0.40.0+dirty",
  "args": [
    "docs",
    "insert",
    "1AbCdEfGhIjKlMnOp",
    "hello",
    "--index=5"
  ],
  "command": {
    "path": ["docs", "insert"],
    "dotted": "docs.insert"
  },
  "positionals": [
    {
      "name": "docId",
      "value": "1AbCdEfGhIjKlMnOp",
      "type": "string",
      "tag_type": "",
      "set": true
    },
    {
      "name": "content",
      "value": "hello",
      "type": "string",
      "tag_type": "",
      "set": true
    }
  ],
  "flags": [
    {
      "name": "index",
      "value": 5,
      "type": "*int64",
      "tag_type": "",
      "source": "arg"
    },
    {
      "name": "file",
      "value": "",
      "type": "string",
      "tag_type": "",
      "source": "default"
    },
    {
      "name": "json",
      "value": false,
      "type": "bool",
      "tag_type": "",
      "source": "default"
    }
  ],
  "help": false,
  "version": false,
  "home_override": null,
  "gog_env": ["GOG_HOME"],
  "baked_profile": {
    "enabled": false,
    "blocked": false
  },
  "command_rules": {
    "enabled": false,
    "blocked": false
  },
  "error": null
}
```

The `flags` list above is trimmed to a few representative entries — a real
invocation reports every root and leaf flag (around thirty for most
commands), including ones the caller never mentioned, at their effective
(post-default, post-locked-flag) value.

## Field reference

### Top-level fields

| Field             | Type            | Meaning |
|-------------------|-----------------|---------|
| `resolve_version` | integer         | Version of this JSON contract. See [Compatibility](#compatibility). |
| `build`           | string          | The binary's version string (`gog version`'s output). |
| `args`            | array of string | `<argv...>` **after** gog's own pre-parse rewrites (see [Notes on `args`](#notes-on-args)). This is what was actually handed to Kong, not the raw argv you passed to `__resolve`. |
| `command`         | object          | The resolved command path. See [`command`](#command). |
| `positionals`     | array of object | The selected command's positional arguments. Empty for help/version requests and for `error.kind` values `"internal"`, `"exit"`, and `"parse"` (even when the latter still identifies a `command`). Populated when `error.kind` is `"locked_flags"` because locked-flag enforcement failed after an otherwise-successful parse, but empty when the baked profile locks a flag this binary does not have (detected before parsing). See [Positional entries](#positional-entries). |
| `flags`           | array of object | Every flag visible to the selected command (root flags plus the command's own), each exactly once. Same emptiness rule as `positionals` above: absent for `"internal"`/`"exit"`/`"parse"` errors and help/version requests, present (with locked values already applied) for a `"locked_flags"` error raised after parsing. See [Flag entries](#flag-entries). |
| `help`            | bool            | `true` if this invocation is a help request (`--help`, `-h`, or bare invocation with no args). When `true`, `positionals` and `flags` are empty; `command` still names the command the help request targets. |
| `version`         | bool            | `true` if this invocation is `--version` (or equivalent). When `true`, `positionals` and `flags` are empty. |
| `home_override`   | string or null  | The `--home` flag's value if the argv passed one explicitly, else `null`. This reflects only the `--home` **flag**, not the `GOG_HOME` environment variable — check `gog_env` for that. |
| `gog_env`         | array of string | Names (never values) of every `GOG_*` environment variable present in the process environment, sorted. Some of these (e.g. `GOG_ACCOUNT`) affect behavior without being bound to any flag, so wrappers need to see that they are set even though nothing in `flags` reflects them directly. |
| `baked_profile`   | object          | Whether a compiled-in safety profile would allow the resolved command. See [Verdict objects](#verdict-objects-baked_profile-and-command_rules). |
| `command_rules`   | object          | Whether runtime `--enable-commands`/`--enable-commands-exact`/`--disable-commands` rules would allow the resolved command. See [Verdict objects](#verdict-objects-baked_profile-and-command_rules). |
| `error`           | object or null  | Set if parsing failed, the parser called an early exit (e.g. a hook flag other than `--version` panicked out), or locked-flag enforcement failed. `null` on success. See [`error`](#error). |

### `command`

| Field    | Type            | Meaning |
|----------|-----------------|---------|
| `path`   | array of string | Canonical command names from the root down, e.g. `["docs", "insert"]`. Default subcommands are made explicit: `gog adsense accounts` resolves to `path: ["adsense", "accounts", "list"]` because `list` is that group's default. Empty for a bare root invocation or an unresolvable path. |
| `dotted` | string          | `path` joined with `.`, e.g. `"docs.insert"`. Empty when `path` is empty. |

For a parse error, `path`/`dotted` reflect the deepest node Kong managed to
select before failing (e.g. `docs` if `docs bogus-command` failed to match a
subcommand), which may be a non-leaf command.

### Positional entries

| Field     | Type   | Meaning |
|-----------|--------|---------|
| `name`    | string | The positional's name as declared in its `arg:""` Kong tag. |
| `value`   | any    | The parsed value (string, number, bool, or `null`), JSON-shaped from Go's reflected value. Unset optional positionals report their zero value here — check `set`, not `value`, to know whether the caller actually supplied it. |
| `type`    | string | The Go type of the target field (e.g. `"string"`, `"*int64"`). |
| `tag_type`| string | The Kong `type:"..."` struct tag on the field (e.g. `"existingfile"`), or `""` if the field has none. Present even when empty; unlike `gog schema`'s `tag_type`, this key is not omitted. |
| `set`     | bool   | `true` if the caller's argv actually supplied a value for this positional; `false` if it was left at its default/zero value (only possible for `optional:""` positionals). |

### Flag entries

| Field     | Type   | Meaning |
|-----------|--------|---------|
| `name`    | string | The flag's long name, without `--`. |
| `value`   | any    | The effective value after parsing, defaults, and (if applicable) locked-flag enforcement — the same value a command handler would see. |
| `type`    | string | The Go type of the target field. |
| `tag_type`| string | The Kong `type:"..."` struct tag on the field, or `""` if none. |
| `hidden`  | bool   | Present (`true`) only for flags marked hidden in their Kong definition; omitted otherwise. |
| `source`  | string | Where the effective value came from. See below. |

`source` is one of:

- **`arg`** — the caller's argv explicitly set this flag (via `--flag=value`,
  `--flag value`, a short flag, or a negated bool flag).
- **`env`** — not set on the command line, but one of the flag's bound
  `Envs` names (declared via Kong's `env:"..."` tag) is present in the
  process environment.
- **`locked`** — a baked safety profile locks this flag to a fixed value,
  overriding whatever the argv or environment supplied. Only possible when
  the binary has a baked profile with `locked-flags` covering this flag.
- **`default`** — none of the above; the value is the flag's ordinary
  Kong default.

**Caveat:** some flags derive their effective value from an environment
variable through a Kong `vars:"..."` binding rather than through the flag's
own `env:"..."` tag (for example `GOG_JSON` feeding the `--json` default).
`source` cannot distinguish that from an ordinary default and reports
`"default"` for these even though an environment variable changed the value.
If a flag's behavior matters to a policy decision, check its **effective
`value`**, not `source`.

### Verdict objects (`baked_profile` and `command_rules`)

| Field     | Type   | Meaning |
|-----------|--------|---------|
| `name`    | string | Profile name, if any. Omitted/empty when there is no baked profile or no name. |
| `enabled` | bool   | `baked_profile.enabled`: `true` if this binary has a compiled-in safety profile at all. `command_rules.enabled`: `true` if any of `--enable-commands`, `--enable-commands-exact`, or `--disable-commands` was supplied on the command line. |
| `blocked` | bool   | `true` if this specific resolved command would be rejected by that mechanism. This is a prediction of what running the command for real would do — `__resolve` itself is never blocked. |
| `message` | string | The rejection message, present only when `blocked` is `true`. |

### `error`

| Field       | Type    | Meaning |
|-------------|---------|---------|
| `kind`      | string  | One of `"internal"` (failed to construct the parser), `"exit"` (some other Kong hook flag triggered an early exit during `Parse` the way `--version` does, but wasn't recognized as `--version`), `"parse"` (ordinary Kong parse error — unknown command, missing required flag, bad value, etc.), or `"locked_flags"` (the baked profile locks a flag this binary does not have, or locked-flag enforcement failed after an otherwise successful parse). |
| `message`   | string  | Human-readable error text. |
| `exit_code` | integer | The exit code a normal `gog` invocation would have produced for this failure (e.g. `2` for parse errors). This is **not** `__resolve`'s own process exit code — see below. |

## Help and version behavior

If the argv resolves to a help request (`--help`, `-h`, or no arguments at
all), `help` is `true`, `command` names the command the help would describe,
and `positionals`/`flags` are empty — `__resolve` reports what would be shown
help for, not the help text itself.

If the argv is `--version` (or another hook flag that Kong triggers as an
early exit specifically for version), `version` is `true` and the rest of the
document is otherwise minimal.

## Process exit code

`gog __resolve` **always exits `0`**, regardless of whether the parse
succeeded. A non-zero `error.exit_code` inside the JSON is what a *normal*
`gog` invocation with the same argv would have exited with — it does not
propagate to `__resolve`'s own exit status. Wrappers must check the JSON
body, not the process exit code, to detect a parse failure.

## Notes on `args`

`args` is the argv gog actually parsed, after gog's own pre-parser rewrites
(for example the rewrites that let `docs cell update-content` and certain
"desire path" shorthands accept alternate forms). It also reflects
`--help`/`-h` normalization. If you need to know exactly what Kong saw, read
`args`, not the argv you passed to `__resolve`.

Kong keeps matching subcommand names as it walks non-leaf commands even after
a `--` token — for example `gog docs -- insert x y` still resolves to
`docs.insert`, because `--` only stops flag/positional parsing within the
node that is active when it's encountered, not subcommand lookup on the way
there. This is one of the reasons wrappers should resolve through gog rather
than parsing argv themselves: the effect of `--`, aliases, and default
subcommands on the final command path is easy to get wrong by hand.

## Guidance for wrappers

- **Execute exactly the argv you resolved**, with the same environment
  variables and the same binary revision (same `build`) that produced the
  `__resolve` document. Resolving one argv/environment/binary combination and
  then running a different one defeats the purpose.
- **Decide on resolved values, not raw argv.** Read `command.path`,
  `positionals[].value`, and `flags[].value` — not the strings in `args` —
  since aliases, default subcommands, negated flags, and environment-derived
  defaults all change the effective meaning of an invocation without
  changing how it looks on the command line.
- **Treat unrecognized shapes as deny, not allow.** Specifically:
  - Any non-`null` `error` should deny by default; do not try to guess
    intent from a partially-parsed `command`.
  - An unknown or newer `resolve_version` than the wrapper was written
    against should deny — see [Compatibility](#compatibility).
  - A non-`null` `home_override` should be treated with suspicion (or
    denied outright) unless the wrapper specifically intends to allow the
    caller to redirect gog's config/data/state/cache location.
  - Any `gog_env` entry the wrapper doesn't explicitly recognize and allow
    should deny, since a `GOG_*` variable can change behavior without
    appearing anywhere else in the document.
- **Don't rely on `baked_profile`/`command_rules` alone.** They describe
  what gog itself would enforce for this argv; a wrapper adding its own
  policy (e.g. "which Doc IDs may be edited") still has to evaluate that
  policy against the resolved `positionals`/`flags` itself.

## Compatibility

`resolve_version` is bumped only for changes that are incompatible with
existing consumers (removing or renaming a field, changing a field's type or
meaning). New fields may be added without a version bump — wrappers should
ignore unrecognized keys rather than treating them as errors, but should
still deny on a `resolve_version` newer or otherwise unknown than the one
they were written against, per [Guidance for wrappers](#guidance-for-wrappers).
