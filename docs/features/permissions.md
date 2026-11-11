# Permissions

Every tool call passes the permission barrier before it runs. The barrier
classifies each call, consults configuration and session grants, and in the auto
modes asks a model classifier, opening an approval dialog only when nothing else
can decide. This file covers the five modes, what each auto-runs, the approval
dialog, and safe/denied matching. Which tools are offered at all is
`commands.md` (`/tools`) and `cli.md` (scope flags), configuration keys and
env binding in `config.md`.

## Modes

The mode picks what runs without asking. Everything not auto-run opens the
approval dialog, or fails closed when nobody can answer.

| Mode | Status label | Auto-runs |
|---|---|---|
| `allow-read` (default) | `read`, hidden until changed | verifiably read-only calls: built-in readers, MCP tools declared read-only, bash lines the analyser proves side-effect free, plus anything listed in `permissions.safeCommands` |
| `auto` | `auto` | as allow-read, and a model classifier judges every call static analysis could not verify (bash commands and MCP/extension tools), approving the side-effect-free ones |
| `auto+write` | `auto+w` | as auto, plus writes confined to the workspace: `write`/`edit` inside the session directory or temp dir, shell output redirected there, in-scope `mkdir`/`rmdir`. Bash classifies under workspace rules instead of read-only ones |
| `allow-all` | `all` | everything. Only `permissions.deniedCommands` still refuses |
| `block-all` | `block` | nothing without approval, reads included |

The classification layers stack: every mode inherits the modes above it.
`deny-list matching → allow-all → write scope → read-only verification →
safe list → session grants → classifier → dialog` is the decision order a call
walks.

### Cycling

- `Shift+Tab` and `Shift+→` step forward: allow-read, auto, auto+write,
  allow-all, block-all, wrapping back to allow-read
- `Shift+←` steps back the same chain
- each change prints one notice (`permissions mode: auto`) and the status line
  shows a `permissions` segment whenever the live mode is not the default

The mode is session-only state: nothing is written to config or transcript, so a
resume always starts from the configured mode. Changing it clears approvals
earned this session (see Session memory), reapplies CLI pre-grants, and
re-evaluates any open dialog. A pending call the new mode permits resolves as
allowed without a keystroke, and a verdict the old mode's classifier was still
producing is discarded, since it no longer speaks for the gate.

## How a call is decided

Checks run in order, first match wins:

1. **You typed it yourself** (`!cmd`, `!!cmd`). User-initiated shell lines are
   exempt from the barrier entirely and run in every mode regardless of any rule
   below, denials included.
2. **`permissions.deniedCommands`** refuses outright, in every mode including
   allow-all.
3. **`allow-all`** permits everything left.
4. **auto+write's write scope** permits writes confined to the workspace roots.
5. **Static verification**: a call proven read-only runs. A hard refusal (`sed
   -i`) denies with guidance no list can override. Anything else becomes an ask,
   which `permissions.safeCommands` converts back into an auto-run.
6. **`block-all`** turns every remaining call into an approval ask, reads
   included.
7. **The model classifier** (auto modes only) judges what static analysis could
   not verify.
8. **The approval dialog**, or a fail-closed refusal when no dialog can open.

### Verifiably read-only

- Built-in readers run by name: `read`, `grep`, `find`, `ls` and the four
  `git_*` history readers, plus any MCP tool its server declares read-only (the
  `readOnlyHint` annotation or mcp.json's `readOnly`, see `agents.md`). Nothing
  is trusted by default: an undeclared MCP tool prompts in every non-auto mode.
- Bash goes through a quote-aware analyser. A line passes only when *every*
  segment it runs is provably read-only:
  - always-safe heads: `ls cat head tail grep diff which ps jq wc cd pwd du od
    echo find`
  - per-invocation checkers for commands with both forms: `git` (pure readers
    like status/log/diff/show pass, dual-form subcommands such as branch/tag/
    config/remote are verified argument by argument), `sed` (no `-i`), `awk`
    (inline script only, no output redirection in the script), `rg`, `sort`,
    `date` (nothing that sets), `file`. Flags that delete or execute (`find
    -delete`, `-exec`) disqualify.
  - an environment assignment anywhere in the line defeats the verdict, as do
    command substitution, subshells and any shape analysis cannot pin down
  - output redirects always write: one redirect keeps a whole line out of
    read-only verification. A `/dev/tcp` or `/dev/udp` target (an outbound
    connection) flags the line unsafe outright.

One hard refusal exists above every list and mode: `sed -i` denies with "use
the edit tool instead".

### Writes under auto+write

The writable roots are the session directory and the system temp dir.

- `write`/`edit` run without approval when their target path resolves inside a
  root. Paths that expand at runtime (`~`, globs, `$var`, `{...}`) never resolve,
  so they prompt.
- Shell lines may read anywhere but write only inside: every output redirect
  must land in scope from every `cd` baseline the line can reach (chains of up
  to four `cd`s are tracked). Unknown constructs fail closed to a prompt.
- `mkdir`/`rmdir` run free when all their path arguments are in scope, `-p`
  checking the highest ancestor too. An unrecognised flag fails closed.
- `.git`, `.hg` and `.svn` are refused even inside a root, since a hook or
  config alias there runs on the next command.

### The model classifier

The auto modes delegate unverifiable calls to the session model with one-word
verdicts: allow (no side effects), deny (any mutation), unsure. One prompt per
call shape:

- bash under `auto`: judged read-only, network reads and file writes deny
- bash under `auto+write`: judged against workspace rules naming the two roots.
  Reading anywhere is fine, mutations must stay inside the roots, and bulk
  destruction, history rewrites, system changes, credential access and piping
  downloaded code into a shell always deny
- MCP/extension tools: judged from their declared name, description and
  parameter schema. An allow requires no observable change anywhere

The classifier never judges `write` or `edit`: outside auto+write's scope those
prompt regardless of what the model thinks. Verdicts cache for the session keyed
by exact call (tool, payload, working directory, rule set), so identical calls
share one request and a verdict is usually in hand before a dialog would open.
An allow auto-approves with a `Tool auto allowed` notice. A deny or an unsure
(a garbled reply, a provider error) keeps the dialog open for a person. A reply
that reads as refusal counts as one: "can't allow" never approves anything.

In unattended runs (one-shots, `--block-autodenied`) plain auto only lets
nameable, redirect-free lines reach the classifier at all: anything analysis
cannot parse refuses instead of trusting a model verdict that promises read-only.
auto+write has less to promise, so its workspace rules judge every parsable line.

## The approval dialog

Opens whenever nothing above decided a call and someone can be asked:

```
Allow `bash` tool call?
git add -A && git push origin main
 1 Allow
 2 Allow with note
 3 Allow `git add` and `git push` for session
 4 Deny
```

Under block-all the question reads "block-all permits nothing without approval.
Run this?" The subject shows the shell command or the tool's raw arguments. For
`write`/`edit` a change summary renders in the dialog while the full diff is
printed above it, so you read what you approve at full length. Long subjects
elide to 16 rows / 480 characters with a `…+N lines` marker.

| Choice | Keys | Effect |
|---|---|---|
| Allow | number or Enter | runs once. Notice: `Tool call allowed this time` |
| Allow with note | | runs and injects the note you type into the model's context as steering, so it learns why without stopping the turn |
| Allow `x` for session | | remembers the grant (see Session memory). Notice: `Tool allowed for session`. Offered only when the line offers reliable memory |
| Deny | Esc | refuses. An optional typed reason reaches the model as a user message (`denied by user: <reason>`), an empty one denies plainly |

Number keys 1-9 select directly, ↑/↓ move and Enter confirms, Esc (or Ctrl+C)
denies.

Dialog behavior worth knowing:

- dialogs hold while you are typing so one never steals focus from your draft.
  An auto-mode classification racing that hold can resolve the call silently
  mid-keystroke: an allow runs without the dialog ever appearing
- with a dialog already open, a classifier allow resolves it and a keystroke
  still beats the model to it. First resolver wins
- with no dialog possible (plain render mode, or a UI that went away) every ask
  settles fail-closed exactly as an unattended run's would
- `--block-autodenied` makes interactive runs behave that way always: nothing
  short of a classifier allow is ever asked, anything else fails closed with a
  `Tool auto denied` notice

A controller holding the control seat can answer dialogs remotely, and
observers see them announced (`sessions.md`).

### Session memory

"Allow for session" remembers what later calls can match:

- a non-bash tool remembers under its own name
- bash remembers one grant per command head that drives the line's effect,
  narrowed by one subcommand word for commands that take verbs (`git`, `go`,
  `make`, `docker`, `kubectl`, `npm`, `cargo`): allowing `git add x` remembers
  `git add`, never covering `git push`. Operand commands stay head-granular,
  their second word naming a file rather than a capability. Read-only segments
  never govern, and a repeated grant key collapses: `rm build && ls` remembers
  just `rm`
- the remembered key matches with any arguments (`go test` covers `go test
  ./...`) and launcher prefixes resolve to it (`time go test ./...` keys as
  `go test`). A bare-head grant, such as from `--allow-tools go`, covers every
  verb line under it
- the bare name `bash` (from `--allow-tools bash`) stands in for any nameable
  shell line

Some lines offer no session memory at all, so their dialog drops the option:
command substitution or subshells, an unnameable head such as an environment
assignment prefix. Grants never bless an outbound-connection redirect
(`/dev/tcp`, `/dev/udp`), and a file redirect matches only under auto+write with
the target resolving in scope.

Grants live in memory: cleared when the mode changes, gone at exit. CLI
pre-grants (`--allow-tools`) are given up front and survive every clear.

## Safe and denied lists

Both configure as arrays. Each entry is one of:

- an exact tool name (`git_diff`, `agent_start`)
- an MCP server namespace (`deepwiki`), covering every `deepwiki__*` tool it
  exposes
- a bash command line matched at token boundaries: `go test` covers `go test
  ./... -run x`, while `cat` never matches `catalog`

### permissions.safeCommands

Converts would-be prompts into auto-runs. Matching is deliberately strict,
because an entry is a promise the analyser could not prove:

- heads compare literally after quoting resolves, and a path-qualified head is
  never blessed (`/usr/bin/curl` is not `curl`), so nothing planted under an
  allowlisted name rides in
- a compound line matches only when every component is either listed or
  verifiably read-only: `make build && rm -rf /` fails as a whole even with
  `make build` listed
- output redirects must resolve inside the write scope from every `cd`
  baseline, which under modes without one means no match at all
- substitution (`$(...)`, backticks) and unparseable shapes never match
- `write` and `edit` can never be listed, so no config entry overrides a known
  writer, and a hard refusal like `sed -i` stays refused above any list

### permissions.deniedCommands

Refuses in every mode before allow-all and everything else except a line you
typed yourself. The model sees "denied by configuration, ask user to run if
necessary", so it knows to hand the command to you instead of retrying.

Matching resolves deeper than the safe list because a refusal may over-match on
purpose:

- quoting forms (`$'...'`), environment prefixes (`FOO=bar sudo ...`), leading
  keywords and launcher wrappers (`env`, `nohup`, `nice`, `stdbuf`, `xargs`,
  `time`, `timeout`) are seen through: entry `apt` refuses `sudo apt install`
- subshell bodies, command substitution, case tails and heredoc text fed to an
  interpreter all get scanned
- nested interpreters resolve their payloads: `bash -c "..."` and `eval ...`
  name what they run

A denied tool name refuses at the gate with "refused: <name>". A bash head in
the deny list is also excluded from the commands the `bash` tool description
advertises to the model.

## Configuration

The four keys (`permissions.mode`, `permissions.headlessMode`,
`permissions.safeCommands`, `permissions.deniedCommands`) layer like any other
config key; defaults and `AJENT_*` env bindings are in `config.md`. Both mode
values accept the five names above plus legacy `auto+mcp`, which reads as
`auto`: an unrecognized `mode` falls back to allow-read, an unrecognized
`headlessMode` warns and uses auto+write. The two lists configure in files
only.

Precedence for a run's starting mode:

- interactive: permission scope flag > `permissions.mode` > default allow-read
- one-shot: scope flag > `permissions.headlessMode` > auto+write

A scope flag holds for that invocation only. `/settings permissions.mode`
changes the live barrier mid-session and is likewise never restored on resume.

Shaping the lists to your own risk tolerance, an example:

```jsonc
{
  "permissions": {
    // where interactive sessions start. Shift+Tab moves per session
    "mode": "auto",
    // one-shot runs may write the workspace but still classify everything else
    "headlessMode": "auto+write",
    // auto-run entries the read-only verifier would otherwise prompt on
    "safeCommands": [
      "go build", "go doc", "go vet", "go test", "gofmt", "golangci-lint",
      "make", "make lint", "jq", "wc"
    ],
    // refused in every mode, dialog and classifier included
    "deniedCommands": [
      "sudo", "su", "ssh",
      "systemctl", "service", "mount",
      "apt", "apt-get", "brew", "pip install", "go install",
      "git push", "git rebase", "git stash"
    ]
  }
}
```

The two lists are where you encode a threat model:

- `permissions.safeCommands` accepts risks the analyser cannot prove away. The
  read-only verifier deliberately distrusts network access, so `curl`, `wget`
  and `dig` always prompt until listed. Listing them trusts the tool but also
  accepts exfiltration: the same curl that fetches docs can POST source or
  secrets anywhere. That is a comfortable trade on a disposable container or an
  offline codebase, and one to think through elsewhere.
- `permissions.deniedCommands` draws lines nothing may cross: privilege
  escalation and remote login (`sudo`, `su`, `ssh`), system services and mounts,
  package managers that install executables, and destructive git history work.
  Because deny matching sees through wrappers and nested interpreters, these
  hold even when the model wraps them, and the classifier's own verdicts cannot
  overrule them.

## Headless one-shots

A one-shot (`-p` or piped) has no dialog, so every ask settles fail-closed: the
auto modes take the classifier verdict as final (bounded by the unattended-run
rules above), `permissions.headlessMode` or a scope flag sets the gate, and
`ask_user` is never offered because there is nobody to answer. Every refusal
(denied review, unapproved call, unparseable line) reads exactly
`permission not given`, so scripts match one string and never hang on a prompt.
Exit codes and the JSON shape of a run are in `cli.md`.

## See also

- `cli.md` - permission scope flags and the `--allow-tools`/`--deny-tools`
  grants
- `commands.md` - `/tools`, `!shell` lines and the input grammar
- `agents.md` - MCP tool declarations, sub-agent read-only guarantees
- `config.md` - config layers and `AJENT_*` env binding rules
- `sessions.md` - dialogs over the control socket, resume behavior
