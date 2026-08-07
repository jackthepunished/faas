# ADR-083 · gregale shell completion + man pages (Tier A8)

- **Status:** accepted
- **Date:** 2026-08-08
- **Issue:** (none — internal)
- **Supersedes:** none
- **Related:** ADR-082 (per-app SLO CLI), ADR-080 (apps-auth-default-flip),
  issue #476 / ADR-076 (webhook delivery surface), Tier A7 PR-cluster
  strategy.

## Context

After Tier B/C/D closed every CLI audit-gap (PRs #722, #730, #748 —
all merged), the next highest-leverage improvement is shell completion
+ man pages. Today the CLI ships neither:

- No `contrib/completion/` checked-in scripts.
- No `docs/man/` files.
- No `man gregale-<command>(1)` page reachable from the binary or
  the docs site.
- Operators type long flag lists by hand; the dashboard is the
  primary surface for discoverability, which contradicts the
  brand promise that "everything the platform does is possible
  from this single binary" (CLAUDE.md, repo map intro).

The Tier A CLI surface is ~45 top-level commands with ~80 subcommands
and ~120 leaves. Manually typing the right verb/flag/positional is a
real friction cost on the operator side, and the only doc discoverability
today is `gregale help` (which lists commands) and the per-leaf
`PrintUsage` call (which prints one line + a docs URL).

Two related decisions are tightly coupled to this one and have to
land together:

1. **Hand-curated manifest vs. generated.** Generator-style AST
   walks are tempting — every `cmdFoo` and every `flag.NewFlagSet`
   already lives in the source. But generator bugs become
   load-bearing for completion correctness, and the `flag.NewFlagSet`
   signature has no machine-readable "this is a closed-set enum
   enum: free|hobby|pro|scale" annotation today; the closed-set
   values live in `pkg/api.AllowedAlertRule*` and similar places.
   A generator would either need to scrape the validators (no
   stable signature) or duplicate the enum (drift). The
   hand-curated slice keeps the manifest reviewable in code
   review — the same gate that catches a new `cmdFoo` shipping
   without a `case "<name>":` in main.go will catch a new command
   shipping without a `cliCommand{}` entry in `cli_meta.go`.

2. **Live API completion vs. static + cached.** Bash's `complete -F`
   can call back into `gregale apps ls --json` at TAB time, but
   this adds 50-200ms per TAB plus forces the shell to auth on
   every completion (FAAS_TOKEN plumbing). Most CLI tools (kubectl,
   gh, awscli) avoid this. The static-plus-cached model — auto-
   refresh in `c.do` middleware on every 2xx, file at
   `~/.config/gregale/completion-cache.json` with 24h TTL — gets
   the warm-cache behaviour without the per-TAB latency cost.

## Decision

### 1. Two new top-level commands: `completion` and `man`

```
gregale completion <bash|zsh|fish|powershell>
gregale man [<command>]
```

Both emit text to stdout (the roff IS the structured format for
`man`; the shell script IS the structured format for `completion`).
Neither takes `--json`. Both are reviewable in code review (the
test `TestCompletion_ManifestDrift` walks main.go's switch and
asserts every `case "<name>":` arm has a matching `cliCommand{}`
entry, and vice versa).

### 2. Hand-curated manifest in `cmd/gregale/cli_meta.go`

One `cliCommand{Name, DocSlug, Short, Subcommands []cliSub, Flags []cliFlag}`
struct per top-level command, mirroring `main.go::run`. New commands
add a 4-line entry here at the same time as the `case "<name>":` in
main.go. The drift test is the load-bearing sync mechanism — it
catches omissions during code review with the same severity as a
missing usage-block line.

The manifest includes:
- `DocSlug`: mirrors the second arg of every `PrintUsage` call
  (output.go:156); the man page renders `See Also` as
  `https://docs.gregale.dev/cli/<DocSlug>`.
- `Short`: the one-line summary shown in `gregale help` and the
  per-shell completion script's description list.
- `Subcommands`: the verb set the dispatcher recognises (mirrors
  the `case subFoo:` arms inside each `commands_*.go` file).
- `Flags`: the flag set, with `Req` markers for required flags
  and `ClosedSet` slices for closed-enum values (plan names,
  alert metric enums, window specs, etc.).

### 3. Four shell backends, one binary

```
gregale completion bash         # bash 3.2+ compatible (no mapfile, no assoc arrays)
gregale completion zsh          # zsh with #compdef gregale + _arguments
gregale completion fish         # fish complete -c gregale
gregale completion powershell   # powershell Register-ArgumentCompleter
```

All four are pure-string emitters driven by the manifest. Adding
a new command requires NO touch on the per-shell files — the new
`cliCommand{}` entry shows up in every backend at compile time.

The bash backend keeps a single completion function (`__gregale`)
that walks `COMP_WORDS` and dispatches to per-command branches.
Bash 3.2 is the macOS default; no `mapfile`, no associative arrays,
no `[[ -v arr ]]`. Just `compgen -W` and `case`. The fish and
powershell backends emit one `complete -c` / `Register-ArgumentCompleter`
call per (command, subcommand, position) tuple — declarative but
verbose.

### 4. Man pages rendered from the manifest

`gregale man` emits the top-level gregale(1) page (NAME / SYNOPSIS /
DESCRIPTION / COMMANDS / GLOBAL FLAGS / EXAMPLES / SEE ALSO).
`gregale man <command>` emits `gregale-<command>(1)` with the
subcommand list and flag set rendered from the manifest entry.

Output format is groff man — the same flavour `man` renders on
Linux + macOS. The user pipes to `man -l -` for immediate
rendering, or redirects to a file under
`/usr/local/share/man/man1/` for permanent install:

```
gregale man alerts | man -l -
gregale man alerts > /usr/local/share/man/man1/gregale-alerts.1
man gregale-alerts
```

### 5. Slug cache: auto-refresh in `c.do` middleware

The per-account positional completion paths (`<slug>` in
`gregale app <slug> ...`, etc.) read from a JSON file at
`~/.config/gregale/completion-cache.json`. The file is rewritten
on every 2xx response from `/v1/apps` or `/v1/orgs` by the
`pkg/api/client.go::doReq` middleware. The cache is invisible to
the request path — a broken cache never fails a request (errors
swallowed with `slog.Warn`).

Security posture:
- File mode 0600, dir mode 0700 (MkdirAll + explicit Chmod;
  MkdirAll is umask-honoring and would otherwise come out as
  0755 on a permissive umask).
- Atomic writes: tmp file in same dir + `os.Rename` (mirrors
  `LocalStorageBackend.Put`, `storage-tmp-sibling-of-final`).
- TTL: 24h via file mtime. Operators force a refresh by `rm`-ing
  the file; the next list call repopulates.
- No secrets in the cache. Only `{id, slug, name}` records.

The bash script reads the cache at TAB time via two hidden
subcommands:

```
gregale completion-cache-path       # absolute path of the cache file
gregale completion-cache-list <kind>   # one slug per line on stdout
```

These two never take `--json` and never appear in `gregale help`
— the shell functions invoke them at TAB time, keeping the install
footprint at "one binary, no extra scripts."

### 6. ADR slot: 083

Slots 080 (×3) and 081 (×2) are already triple/dual-used in this
repo. Slot 083 is the next free.

## Consequences

### Positive

- Every existing user gets shell completion for free on their next
  binary upgrade. Four shells covered.
- New commands ship with completion for free — adding a 4-line
  `cliCommand{}` entry in `cli_meta.go` is enough; the per-shell
  renderers pick it up at compile time.
- `man gregale-alerts` works locally without docs.gregale.dev
  being reachable; the man page is in the binary.
- The manifest-drift test is the single load-bearing sync
  mechanism — no AST generator to maintain.

### Negative / trade-offs

- The manifest is duplicated information (main.go's switch +
  per-file dispatchers carry the same data, again). The drift
  test catches drift in one direction (every `case "<name>":` has
  a `cliCommand{}` entry); a stale `cliCommand{}` entry with a
  removed dispatcher would only be caught at runtime. The reverse-
  direction check is in `TestCompletion_ManifestDrift` too —
  every manifest name must appear in main.go's switch.
- The slug cache is a separate state file from the SDK's
  configuration. Operators who delete `~/.config/gregale/` lose
  the cache; the next list call repopulates. No data loss risk.
- Bash 3.2 compatibility rules out a few nicer constructs
  (associative arrays would simplify the dispatch). The current
  `case` chain is ~120 lines but trivially auditable.
- `gregale man <unknown>` returns exit 1. This is conventional
  (matches `man <unknown>`) and surfaces the unknown-command
  error path explicitly.

### Out of scope (intentionally deferred)

- `contrib/completion/` checked-in scripts (the binary emits them
  on demand; no versioned copies).
- PowerShell module packaging (PSGallery).
- fpath-based zsh autoloader (the user puts the script on `$fpath`
  themselves).
- Dynamic completion for org member lists / app env vars / per-app
  secrets. Only `/v1/apps` and `/v1/orgs` populate the cache today;
  adding more is a per-endpoint change in `MaybeRefresh`.
- Bash / groff syntax-check tests in CI (both tools are absent on
  most dev boxes; CI's metal runner has them, but `make test`
  must pass on any machine per CLAUDE.md). The structural tests
  in `commands_completion_test.go` are the tripwire; a future PR
  can add `//go:build bash_complete` and `//go:build roff_complete`
  test files for the integrated validation when the toolchain is
  reliably available.
