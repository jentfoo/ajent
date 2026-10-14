# Prompt design

Every string ajent sends to a model, and the rules that keep those strings
cheap, stable and honest. Each prompt surface is described in this document, so it
is the single reference for prompting.

The core idea:
**a good prompt makes the model cheap, predictable and self-aware of what it does not know.**
Cheap means cache-stable and no wasted tokens. Predictable means structured
output where a machine will consume it. Self-aware means provenance markers
everywhere injected content came from, so the model can tell real instructions
from compacted history, user rules from its own assumptions.

## Principles

These apply to every prompt surface in this document and are enforced by tests,
not taste.

**1. The system prompt is cache-stable.** Within a session, the assembled system
block must be byte-identical between requests except for explicit, deliberate
changes: the day-granular date, tool-set changes (project instructions are fixed
for the session). Anything sub-day (timestamps, token counts) busts the
provider's whole prompt cache and costs real money on every subsequent turn.
When a change does land (say `/tools` toggles a tool), it is announced with a
one-line notice so users understand why caching resets.

**2. Prompts are data plus instructions, kept separable.** The model should be
able to tell "this is the conversation you must summarise" from "here is how to
summarise". Injected content goes in explicit XML-ish tags (`<summary>`,
`<plan>`, `<git_status>`, `<project_instructions path=...>`), and instruction text
stays outside them. This is what lets one model hand work to another: the plan
workflow's kickoffs are read by a model with no other context at all.

**3. Structured output beats prose wherever a machine or another model consumes the result.**
Summaries use fixed headings with exact formats spelled out in the prompt. The
format is part of the contract, and tests assert it verbatim.

**4. Preserve what resumption needs, verbatim: file paths, line numbers, function names, error messages.**
Generic "summarise the conversation" prompts lose exactly the details that let a
later model pick up where one stopped. Every lossy prompt in this document
carries an explicit instruction to keep those artefacts exact.

**5. Only advertise what is real.** A tool appears in the request only when it
is enabled. Advertising something that is not there makes the model burn a
round-trip discovering it, or worse, try and fail. Guidance for using a tool
rides that tool's own description, which exists only when the tool does.

**6. Provenance everywhere.** Every block of injected content carries where it
came from: `<project_instructions path="/abs/AGENTS.md">`, a compaction summary
that says "history before this point". The model must never mistake one kind of
text for another. Permission-barrier notes ("Allowed with note:" / "Denied with
note:") are surfaced as **user** messages immediately after the tool call they
govern, so they carry operator-level authority rather than being read as
injected or tool output.

**7. Background prompts are short.** Compaction calls reuse the provider client,
use minimal reasoning, ask for exactly what is needed and no more. A long prompt
on a call that runs every turn or per tool call is a tax paid forever.

## Prompt surfaces

The prompt surfaces are covered in detail below, each with the contract that
governs its assembly and use.

| Surface | What it is |
|---|---|
| System prompt composition | identity + guidelines + environment facts + project instructions (+ extension/sub-agent snippets) |
| Tool descriptions & schemas | per-tool `Description` prose plus JSON Schema params, sent via the provider tool channel |
| `@`-file reference injection | synthetic read call+result behind the user message |
| Project instruction layering | `~/.ajent/AGENTS.md`, then `<cwd>/AGENTS.md`, layered with provenance markers |
| `/init` project survey | the build and codebase sub-agent tasks, and the instruction that distils them into `AGENTS.md` |
| Compaction summarisation | exact-format LLM summary over a marker-reduced transcript |
| Sub-agent prompt | the child contract appended as a system snippet to every investigation, plus the empty-summary nudge |
| Plan workflow kickoffs | the planning contract, the implementation kickoff, the review kickoff and the retry prompt |
| Tool-call classifier (`auto`) | one-word verdict on an unverifiable shell command, fresh context |
| Staged shell results | the `!` user message: `User Ran:` / `Output:` ahead of your next prompt; `!!` sends nothing |

## Staged shell results

The `!cmd` result lands as a single **user-role** text message staged via
`Input.Before`, so it reads as the human's own action rather than an agent tool
call. Its exact format is:

```
User Ran: <command>

Output:
<combined stdout+stderr, or "(no output)" when empty>
```

The body reuses the real `bash` tool's result text verbatim: status prefix,
interruption marker, timeout note and truncation spill pointer all come from the
tool path itself, so what the model sees is byte-for-byte honest about how the
command ended. The user role plus the `User Ran:` voice *is* the provenance
marker the model reads. A `!!` run sends nothing to the model.

The `Input.Before` entry carrying it is an `agent.MessageInfo`, whose `Replayed`
mark is what puts it back on screen: injected context the model still carries
belongs in restored scrollback, so a resume or rewind redraws the run above the
prompt it fed instead of showing a prompt whose premise has vanished. How replay
renders it lives in `tui-design.md`.

---

## System prompt composition

Assembled by `buildSystem` into a single cache-stable text block. Project
instructions are layered on top of the ordered base parts without touching the
loop, and the enabled tool schemas ride in the request's separate `Tools` channel,
not this block.

### Parts, in order

```
1. Opening sentence         — how the agent works, domain- and tool-neutral.
2. Guidelines               — two static bullets, no tool-derived content.
3. Environment facts        — working directory, platform and day-granular date.
4. Project instructions     — `~/.ajent/AGENTS.md`, then `<cwd>/AGENTS.md`, when each exists.
5. Extension snippets       — caller-supplied, blank-line separated; sub-agents inject their contract here.
```

Parts 1 and 2 together are ajent's default prose guidance. An operator can
replace them with `agent.systemPrompt` in config (or `--system`) when they want a
different opening stance, while environment facts, project instructions and snippets
still follow unchanged, since tools depend on cwd/date.

### Opening sentence

A single neutral line stating how the agent works, deliberately naming neither a
domain nor specific tools so the same block serves coding, security work and
plain Q&A. It stays focused on the user's request and puts understanding before
action.

It claims no tool or capability beyond that: a model told "you are brilliant"
costs more than one simply given accurate scope.

### Guidelines

Two static bullets: conciseness and clear file paths. Nothing is derived from
the enabled toolset, so the block stays cache-stable across tool changes. The
rule that guidance never names an absent tool is structural: per-tool guidance
rides each tool's `Description`, which is sent only while that tool is enabled.

### Environment facts

The header describes where the agent is and what it can see, not machine trivia.
Shell, git status and a directory listing are deliberately omitted. A listing
would change whenever the workspace changes, breaking cache stability, and the
model discovers files with its tools instead. Empty values drop their line
rather than emitting `unknown`:

```
Working directory: /path
Platform: linux/amd64       # only when known
Date: 2026-08-10            # day granularity — the one thing that changes per day
```

Every input is fixed at startup, so only a date rollover varies within a
session: that is the cache-stability contract.

### Composition invariants

- One text block (`llm.TextBlock`), not many, since providers cache per-block.
- The system prompt stays deterministic given its inputs, and is byte-stable
  across calls with equal inputs. Extension snippets and project instructions join
  through explicit inputs so tests can assert that equality. An empty snippet slice
  produces a block identical to one built without the parameter. A non-empty
  `override` replaces parts 1–2 (opening sentence plus guidelines) but leaves
  environment facts, project instructions and snippets in place.
- Changing the tool set or adding/removing project instructions changes this
  block. Per principle 1, a change is announced with a one-line notice.

---

## Tool descriptions & schemas

Tools reach the model through the provider's tool-schema channel,
**not as text in the system block**. Each enabled tool contributes its name, a
prose `Description()` and JSON Schema parameters (derived from struct tags) to
the request's `Tools` list. Only tools whose state is Enabled are sent, which is
how "only advertise what is real" is enforced.

The description is full sentences, not a one-line snippet. It states plainly
anything that changes how the model should use the tool:

- `read`: returns line-numbered text and refuses binary files, with
  offset/limit paging.
- `bash`: runs in the session working directory. Output is truncated with the
  full log spilled to a file and timeout overrides the default. One shell
  process per call, no persistent `cd`. May end with a startup-probed list of
  common commands, framed as a fallback.
- `write`: creates or overwrites files, making parent directories, and an overwrite
  reports what it displaced as a diff.
- `edit`: text replacement that applies atomically or not at all. Non-exact text
  may still resolve through guarded match tiers, which the result names.

The `agent_*` sub-agent tools carry their whole contract in the description
because a model that learns them by trial burns a round trip each: no session
context (pass file paths and key facts, not content), read-only (`read`, `grep`,
`find`, `ls` plus read-only MCP tools), and the final message is the entire
return value.

There is deliberately **no "Available tools" list inside the system prompt**:
the schema channel already tells the model exactly what it may call, so a second
text copy would only cost tokens. The same rule holds for a child's block:
`childContract` carries only constraints and the output contract, never an enum
of tools (those ride the schema channel like any other request).

### Split what the model sees from what the user sees

The tool result has a model-facing form and a display form. `edit` shows the
user a colourised diff while the model gets the summary plus a diff only when
something needs checking (a non-exact match, several sites, possible
duplication). `bash` streams full output to the screen while handing the model a
truncated, ANSI-stripped version with an elision marker. The prompt contract is
**the model must know when it has been told less than the whole truth**:
truncation markers are not optional.

### Schema errors as feedback

`edit`'s "exact match" failures return actionable error text (nearest
near-match, occurrence count) because they are the model's main self-correction
loop. The same principle extends to every tool: an error result is a hint for
how to retry, not just a stop sign.

---

## `@`-file reference injection

When a user message contains `@path`, ajent injects a synthetic `read` call +
result pair behind that message, using the real tool. The literal `@path` stays
in the text, and the injected read is what actually puts content in context. Behind,
not ahead, so that rewinding onto the message drops its reads too and a re-send
sees the file as it is now (`command-design.md`).

Prompt implications:

- Using the real tool means one code path for truncation markers, binary
  refusal, line numbering and stale-read tracking, so the model sees exactly
  what it would see if it had called `read` itself.
- Injected reads are visible to compaction's superseded-pass and countable by
  token accounting. No special "reference" block type is ever shown to the
  model. A reference is just an ordinary read.

---

## Project instruction layering

The user-global `~/.ajent/AGENTS.md` (honouring `AJENT_HOME`) and
`<cwd>/AGENTS.md`, when they exist, are read once at startup in that order and
injected into the system block ahead of the first turn. The format mirrors how
other agents present project instructions early in context: a provenance-marked
wrapper so the model can tell project rules from conversation:

```
<project_context>

Project-specific instructions and guidelines:

<project_instructions path="/abs/AGENTS.md">
...file body...
</project_instructions>

</project_context>
```

Rules:

- **Two sources, global then project.** The user-global `~/.ajent/AGENTS.md`
  (honouring `AJENT_HOME`) is read first and `<cwd>/AGENTS.md` second, so the
  more specific project file appears later in context. Absent or unresolvable
  sources are skipped. Nested discovery beyond these two: no ancestor walk.
- **Provenance marker carries the absolute path**, so the model can point at
  which instruction file told it something.
- Loaded once at startup and kept for the session, so a changed `AGENTS.md` applies
  on next launch. There is no mid-session reload or file watching, keeping the
  system block cache-stable per principle 1.

---

## `/init` project survey (`pkg/projinit/prompt.go`)

Three surfaces, shaped by one fact:
**the survey is data the final pass never saw being produced.** Stages 1 and 2
spend no model tokens: they run the real `read`, `agent_start` and `agent_poll`
tools and let their genuine call + result pairs carry the findings, so the
distilling model reads them as its own tool output rather than as a pasted
report. Structural design is in `command-design.md`.

**Sub-agent tasks.** One build survey plus one per disjoint slice of the tree.
Both end with the same tail so a child returns prose the parent can paste:

- **Report style**: prose rather than a raw dump, written to be pasted into an
  AGENTS.md
- **Grounding**: report only what was actually read, never guess or generalise
  from convention

The build task names its inputs explicitly because "how do I get a footing" is
the one thing a wrong guess makes expensive. It has two parts:

- **Reads**: how the project is built, tested and linted, plus the Makefile (or
  equivalent), CI config and CONTRIBUTING.md when present
- **Reports**: exactly which commands build, test and lint it and what each
  expects (toolchains and versions, environment variables, generated files, any
  setup step that must run first). Each command names the file it came from

Each codebase task carries its own slice (`%s`) and is told another agent covers
the rest so the division is visible in what it reads:

- **Reads**: stays inside those paths
- **Reports**: what each package or module does, the dependency edges between
  them, key entry points, and any invariant or constraint worth recording for
  someone changing this code

**Distillation.** One prompt, one turn. Both variants share a header naming the
survey as data (the files read directly plus one summary per read-only
sub-agent) and a closing rule set, with only the middle differing between draft
and correct. The rules:

- Every claim must trace to something in the survey, never invent commands,
  conventions or code-style rules
- Keep wording concise because this file is read on every turn
- Write AGENTS.md with the write tool then stop

A fresh draft asks for `## Project Overview` (one paragraph), `## Commands`
(build/test/lint exactly as reported) and `## Architecture` (where code lives,
design notes, invariants), plus a section per convention the survey actually
observed. When `AGENTS.md` already exists it is read in stage 1 and the
instruction becomes a correction pass instead: treat the survey as source of
truth, correct what it contradicts, add what shows missing, keep existing
structure and wording where they are still right (a correction pass not a
rewrite). That choice gates on the stage-1 read result, never the stat alone:
an existing file whose read failed fails the survey rather than instructing
corrections over content no one saw.

Two rules are load-bearing and asserted verbatim. **Brevity**: the file is read
on every turn, so a long one is a tax paid forever (principle 7 applied to the
one prompt surface the user writes). **Nothing invented**: the structure may
echo ajent's own `AGENTS.md` (overview → commands → architecture), but its
code-style sections are project-specific and must never be copied into another
repository's file, and tests assert their absence.

The write is the model's own `write` call, so the permission barrier gates it
like any other write rather than `/init` inventing a private path to disk.

---

## Compaction summarisation

Compaction always cuts and always summarises. The newest steps are held back
verbatim, and everything before them is folded into one checkpoint. Free structural
reduction still runs, but only over the transcript the summariser reads.

### The summariser system prompt

A dedicated, single-purpose instruction to summarize context content.

### The initial summary instruction

The serialised history goes in `<conversation>...</conversation>` tags (treated
as data, not a live thread), followed by an exact-format spec. The task is to
create a structured checkpoint another model will use to continue work. It notes
that the most recent steps are deliberately withheld and must not be summarised.
It then gives six fixed headings:

- `## Goal`: current objective, or what changed if redirected
- `## Constraints & Preferences`
- `## Progress`, with Done / In Progress / Blocked checklists
- `## Key Decisions`
- `## Next Steps`, ordered list
- `## Critical Context`: everything needed to continue without re-reading
  history

The spec closes with fidelity rules:

- Be brief in wording and complete in substance. Cut preamble, hedging and
  adjectives, but never a fact
- Preserve file paths, function names, error messages and command lines exactly
- Give assistant-produced content (code, prose, plans, answers) a 2-3 sentence
  synopsis of its substance rather than just a title or name

This is the heart of resumption:
**Goal / Constraints / Progress / Decisions / Next Steps / Critical Context**,
with a hard rule to keep file paths and line numbers verbatim.

### Incremental update

When a prior summary exists it goes in `<previous-summary>` tags and the prompt
becomes an *update*, not a rewrite. It asks for ONE merged summary under
explicit rules:

- Integrate rather than append, never state the same fact twice
- Replace the goal if the user redirected the work, noting what changed
- Move items In Progress → Done as work completes
- Compress finished work to one line once nothing depends on its detail
- Never drop a constraint, decision or outstanding work
- Preserve exact file paths, function names, error messages and command lines

It reuses the same six-section format. This is how summaries merge rather than
nest: each compaction refines one checkpoint instead of piling summaries inside
summaries.

### Framing integrity

The transcript is untrusted text inside a trusted frame, so tag literals in data
are escaped at render time and stored raw everywhere else. Any literal
`</conversation>`, `</previous-summary>` or `</summary>` rendered inside a block
becomes `<\/...>` (`session.EscapeTagLiterals`) and cannot close it early. Only
the framing's own tags stay raw. The scheme composes across merge rounds because
it is idempotent: record-time canonicalisation undoes any escapes a model copied
out of `<previous-summary>`, so every render escapes exactly once, and yesterday's
unescaped summary needs no migration. A reply missing any of the six section
headers gets one bounded retry. Still missing them, the fold records anyway and
warns through `Result.Warnings`: a structurally defective checkpoint must not
fail the run it paid for, nor persist silently.

### What the summariser reads

The verbatim band is excluded by construction (the span handed to the summariser
stops where the band begins), so the checkpoint never re-describes work the
reader can already see.

- **Thinking is omitted.** Cheap to drop, and it removes any confusion about
  whose reasoning is being read.
- **Tool results carry self-describing markers** where the reduction pass
  replaced them: `[identical to an earlier tool result]`,
  `[superseded by a later read of the same file]`, and the failed-tool
  one-liner. The wording is the instruction, and the prompt says nothing about them.
- **Output is clipped only when it must be.** The transcript is built whole
  first, and a clip is applied only if it would not fit alongside the reply,
  stepping down toward smaller sizes and finally dropping the oldest entries.
  Dropped entries are folded into the checkpoint by their own chained call, so
  nothing the cut removes goes unsummarised. If even a chunk cannot fit, the
  run fails with an error rather than sending a request the provider will
  reject or summarising an empty transcript. Compaction fires near the top of
  the window, so an unclipped span plus its summary can overflow, and an
  oversized request would fail the session exactly when it most needs to
  shrink. An unknown window applies no bound.

### User guidance

- `/compact <instructions>` appends the user's focus as a short "Additional
  focus: ..." line so the summary leans toward what they care about.

### Re-injection

The compacted history is placed back in context with explicit provenance so the
model knows what it lost: an intro line naming that the history before this
point was compacted, wrapped around the summary as a `<summary>` block. Tag
literals inside are escaped the same way at replay, so a stored summary cannot
end its own provenance frame and let its tail read as live user text.

A branch return uses similar framing ("a summary of a branch this conversation
came back from"). Nothing is ever deleted, and recovery is a rewind onto the
compaction row (see `compaction-design.md`).

### Honesty

Compaction reports real numbers as a replayable notice so users never think the
agent "forgot" for no reason. The exact shape is specified in
`compaction-design.md`.

---

## Plan workflow kickoffs (`pkg/plan/prompt.go`)

Four surfaces, all shaped by one fact:
**the receiving model has no prior context**. Structural design is in
`plan-design.md`, and the wording contract is here.

**Planning contract.** Appended to the user's first goal as its own content
block, so `Input.Text` (and therefore the echoed line and the recall entry)
stays the user's own words. The input emits its text before its blocks, so the
model reads the goal and then the contract. That placement is deliberate,
putting the no-prior-context rule closest to where the plan gets written. It
sets the planning role, tells the model to ground the plan in the codebase it
can read, and to put a genuine design fork to the user with `ask_user` rather
than guessing. The load-bearing paragraph is that the plan goes to a separate
model that will not see this conversation: every file path, interface,
constraint and acceptance criterion has to be carried in the plan text. It asks
for interfaces and signatures where they pin the design down and no
implementation beyond that, then: call `dev_implement` when the plan is
complete, and edit nothing yourself.

**Implementation kickoff.** The first and only message of a fresh root. It opens
by saying so ("You have no prior context. Everything you need is below."), then
`<plan>`, and `<revision_instructions>` on later rounds, described as work still
outstanding. It asks the model to verify its work as it goes, and to call
`dev_review` with a summary, warning that review starts anyway if it stops. The
summary's description says plainly that the reviewer sees none of this
conversation and only learns what it reports, which is why the field is
required, and a round that never calls the tool falls back to its closing message.

**Review kickoff.** Carries `<plan>` **as the user approved it**, labelled as
such since it may differ from the draft the reviewer watched itself write, plus
`<git_status>`, `<git_diff_stat>` and `<implementation_summary>`. It states that
the implementation conversation is deliberately absent and files must be re-read
rather than assumed. Two exits, `dev_complete` or `dev_revise`, with the same
no-prior-context warning attached to the instructions the latter produces.

**Retry prompt.** Self-contained by necessity, since it may follow a turn that
died mid-edit: continue from where you stopped, do not repeat completed work,
call `dev_review` when done.

The compaction focus strings live here too: implementation keeps files changed,
approaches tried, decisions made and unfinished plan items (reproduced
verbatim), and review keeps files inspected, issues found and conclusions reached.

---

## Tool-call classifier (`auto` / `auto+write` modes)

All three prompts live in **`pkg/permit`** (`ClassifierSystem`,
`MCPClassifierSystem`, `WorkspaceClassifierSystem`), the package that owns the
`Classifier` interface, not `pkg/app`. The shell prompt keeps its strict
unconditional bar (running arbitrary software is always a write), while the MCP
variant states the no-observable-change and network-exfiltration rules. A call
is classified with one fresh-context request to the session's current model
(never the session history), and the verdict never enters the session. All three
ask the same question (may this run unattended?) for the same one-word answer:
`allow`, `deny` or `unsure`. One vocabulary means one normaliser and no
per-prompt parsing anywhere downstream. Reasoning is off, so a verdict costs no
thinking tokens. In auto modes classification runs concurrently with the typing
hold, so an allow verdict approves mid-typing (see `agent-loop-design.md`).
Verdicts normalise by scanning for
the verdict words anywhere in the reply: an `allow`/`allowed` token yields
approval, `deny`/`denied`/`disallowed` denial, and
**both present (or neither) is unsure**. The asymmetry is deliberate: `deny` and
`unsure` are indistinguishable downstream (both leave the dialog open), so only
an unambiguous single-direction reply can fail open, and any conflict keeps the
dialog open. A verdict word shadowed by a negator (`not`, `never`, `cannot`,
`can't`, …) anywhere in the reply counts for nothing, so "I can't allow this"
never fails open either.

Caching and single-flighting live below the prompt in `pkg/permit`, and unsure
verdicts are never cached. One rule reaches up here: elided arguments carry a
digest of the full text in the cache key, so two calls sharing one rendering
never share a verdict.

The modes differ in what they classify and by which rule set. **`auto`** judges
unverifiable bash commands plus MCP/extension tool calls, sending the model the
call's description and JSON-Schema parameters so it can judge functionality it
has never seen before, and `auto+write` adds write confinement to that same
classifier.

A non-bash call's arguments are bounded before the request (the shell branch
stays unbounded): head and tail survive around an explicit `…[N chars elided]…`
marker, so a verdict sees both ends of a large payload and is never left to
wonder whether text was cut.

A **core writer is never classified** in any mode. `write`/`edit` are decided
statically (by `auto+write`'s path scope, otherwise by the dialog), so a stray
`allow` verdict can never auto-allow one. Every other built-in is resolved by
`Classify` before the asker runs, so only bash and non-built-in tools ever reach
the model.

**`auto+write`** keeps the strict MCP prompt for non-shell calls, but sends
shell commands to `WorkspaceClassifierSystem`: there the question is not
*does this write?* but *is this write permissible?*. The prompt names its two
writable roots verbatim, cwd and the temp dir, the same two the barrier
path-scopes `write`/`edit` against, so gate and model judge by one rule.
Reads anywhere are always `allow`, credentials and secrets the sole
exception, and only writes need confinement. Inside the two roots it allows
file creation, scripted rewrites, redirects, moves and copies, removing
individual files, build and test commands and in-repo git. It denies regardless
any path outside the roots (a write there), bulk destruction, system or package
changes, repository metadata writes, unaccountable execution and credential
access. Ordinary network retrieval is left to model discretion here, unlike the
`auto` shell prompt below: anything that could exfiltrate data or reach a
sensitive target denies. An
**in-scope** directory create/remove never reaches it: the barrier resolves
those path arguments itself, and an out-of-scope one still goes to the model
like any other command. Ambiguity resolves to `unsure`, never `allow`, and a
`cd` into the workspace never launders a later absolute path.

`Subject.AllowWrite` selects the rule set and is part of the **cache key**: the
LRU survives a mode change, so without it one mode's verdict would answer for
the other. The words are the same across prompts, but the rule they encode is
not.

The shell prompt keeps the reference's framing (compound constructs classify by
the commands they actually run, examples are illustrative not exhaustive) with
one deliberate change: **reading from the network is *not* read-only**. The
exfiltration channel means "does not write locally" never equals safe, so the
classifier must say so explicitly rather than inheriting the reference's
opposite claim. Network tools are absent from both the static allowlist and
any notion of classifier read-only.

The MCP prompt applies the same no-change bar to a single tool invocation: a
`allow` verdict requires **no observable change anywhere** (files, repo,
process, network, remote service, permissions, configs, caches or credentials),
and reading from the network alone is never enough. The tool's name, description
and parameters are embedded verbatim so an unfamiliar MCP server can be judged
by what it declares rather than guessed at.

---

## Sub-agent prompt

Every investigation child gets a fresh system block built by the same
`buildSystem` with one extra snippet appended after project instructions:
`childContract`. It carries three parts, pinned by golden tests. The intro
names the agent as an isolated research sub-agent of a coding agent. The
constraints section limits it to read-only tool calls (`read`, `grep`, `find`,
`ls` and any MCP tool marked read-only) issued as typed tool calls rather than
shell, with no editing or destructive operations. That constraint is
structural, not advisory, because the tool set is filtered before the model
ever sees it. Inside a git work tree the same sentence also names the four
`git_*` history readers (`git_status`, `git_log`, `git_show`, `git_diff`),
matching the repo-gated tool set. Outside one they are omitted so the
advertised list is never wider than what a call can answer. The output section
makes the final assistant message the entire return value, a single
self-contained summary of everything discovered, with conclusions, key file
paths and line numbers, caveats, and no tool calls. Investigate thoroughly,
then stop.

A reasoning model whose final assistant message is thinking-only returns no
text, so an empty summary triggers a bounded nudge to emit plain-text output
with no tool calls. A message cut short by the output or step cap gets its own
truncation variant asking for everything discovered so far. Either way one
attempt at most, then the job fails rather than looping.

The task prompt is `Task:\n<task>` with an optional `Extra instructions:` block
prepended when the caller supplied them.

**Completion steer.** When an investigation finishes unpolled, one batched
notice is injected at the next step or turn boundary as an injected user
message, prefixed `[tool]` so the model reads it as generated context rather
than operator speech:

```
[tool] Sub-agent sub-1 completed. Call agent_poll with id sub-1 to retrieve the summary.
```

The keyed UI notice naming the same completions is front-end text only and
carries no prefix.

---

## Testing prompting

Prompts are code. The bar is golden/verbatim tests over exact strings:

- **System prompt**: assert byte-equality across calls with equal inputs, assert
  cache stability across days (only the date differs), and assert empty facts drop
  their lines and git failures fall silent.
- **Tool schemas & descriptions**: assert a tool appears in the request's
  `Tools` list only when enabled.
- **Compaction**: assert the exact six-section format is present, that user
  focus instructions and the previous summary are included, and (with a scripted
  fake provider) that the model received history in `<conversation>` tags
  treated as data. Assert tag literals inside those blocks are escaped so no
  payload can close its own frame.

Where a prompt is user-configurable (project instructions) the tests cover
presence/absence of `<cwd>/AGENTS.md`, the provenance marker carrying its
absolute path, byte-stability across calls with equal inputs, and that project
instructions are appended after environment facts.
