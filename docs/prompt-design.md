# Prompt design

Every string ajent sends to a model, and the rules that keep those strings
cheap, stable and honest. Each prompt surface is described in this document; it
is the single reference for prompting.

The core idea: **a good prompt makes the model cheap, predictable and
self-aware of what it does not know.** Cheap means cache-stable
and no wasted tokens. Predictable means structured output where a machine will
consume it. Self-aware means provenance markers everywhere injected content came
from — so the model can tell real instructions from compacted history, user
rules from its own assumptions.

## Principles

These apply to every prompt surface in this document and are enforced by tests,
not taste.

**1. The system prompt is cache-stable.** Within a session, the assembled system
block must be byte-identical between requests except for explicit, deliberate
changes: the day-granular date, tool-set changes (project instructions are fixed
for the session).
Anything sub-day (timestamps, token counts) busts the provider's whole prompt
cache and costs real money on every subsequent turn. When a change does land —
say `/tools` toggles a tool — it is announced with a one-line notice so users
understand why caching resets.

**2. Prompts are data plus instructions, kept separable.** The model should be
able to tell "this is the conversation you must summarise" from "here is how to
summarise". Injected content goes in explicit XML-ish tags (`<summary>`,
`<project_instructions path=...>`); instruction text stays outside them. This is
what makes compaction and plan projection work as pure transforms of an
assembled message list, never as mutations of state.

**3. Structured output beats prose wherever a machine or another model consumes
the result.** Summaries use fixed headings with exact formats spelled out in the
prompt. The format is part of the contract; tests assert it verbatim.

**4. Preserve what resumption needs: file paths, line numbers, function names,
error messages — verbatim.** Generic "summarise the conversation" prompts lose
exactly the details that let a later model pick up where one stopped. Every
lossy prompt in this document carries an explicit instruction to keep those
artefacts exact.

**5. Only advertise what is real.** A tool appears in the request only when it
is enabled; guidelines adapt to which tools are available. Advertising something
that is not there makes the model burn a round-trip discovering it, or worse,
try and fail.

**6. Provenance everywhere.** Every block of injected content carries where it
came from: `<project_instructions path="/abs/AGENTS.md">`, a compaction summary
that says "history before this point". The model must never mistake one kind of
text for another.

**7. Background prompts are short.** Compaction calls reuse the provider client,
use minimal reasoning, ask for exactly what is needed and no more. A long prompt
on a call that runs every turn or per tool call is a tax paid forever.

## Prompt surfaces

The prompt surfaces are covered in detail below, each with the contract that
governs its assembly and use.

| Surface | What it is |
|---|---|
| System prompt composition | identity + guidelines + environment facts + project instructions |
| Tool descriptions & schemas | per-tool `Description` prose plus JSON Schema params, sent via the provider tool channel |
| `@`-file reference injection | synthetic read call+result ahead of the user message |
| Project instruction layering | `~/.ajent/AGENTS.md`, then `<cwd>/AGENTS.md`, layered with provenance markers |
| Prompt templates / slash commands | markdown templates expanded into prompts; `/init` survey |
| Compaction summarisation | staged free reductions + exact-format LLM summary |
| Tool-call classifier (`auto`) | one-word verdict on an unverifiable shell command, fresh context |

---

## System prompt composition

Assembled by `buildSystem` into a single cache-stable text block. Project
instructions are layered on top of the ordered base parts without touching the
loop; the enabled tool schemas ride in the request's separate `Tools` channel,
not this block.

### Parts, in order

```
1. Identity sentence        — one neutral line naming the harness + repo cwd.
2. "How you help" line      — read files, run commands, edit code, write new files.
3. Guidelines               — concise bullets; some derived from which tools exist.
4. Environment facts        — clean structured lines: cwd, platform, shell, date,
                              git branch + dirty state.
5. Project instructions     — `~/.ajent/AGENTS.md`, then `<cwd>/AGENTS.md`, when each exists.
```

### Identity

Neutral and specific to the harness name, not a persona:

```
You are an expert coding assistant that works in the repository at /path.
You help by reading files, running commands, editing code and writing new files.
```

The first line names the working directory because everything else assumes it;
the second states the four verbs the toolset actually supports. No personality,
no capability claims beyond what is true — a model told "you are brilliant" costs
more than one simply given accurate scope.

### Guidelines

Short bullets, always including:

```
- Be concise in your responses
- Show file paths clearly when working with files
```

Plus guidelines derived from the enabled toolset. When `bash` exists but no
dedicated exploration tools do (`grep`, `find`, `ls`), add a hint so the model
uses bash for it:

```
- Use bash for file operations like ls, grep, find
```

The derivation rule matters: guidelines should never tell the model to use a tool
that is not present.

### Environment facts

Structured lines with empty values omitted entirely (a missing platform or shell,
or a non-git directory, drops that line rather than emitting `unknown`):

```
Working directory: /path
Platform: linux/amd64       # only when known
Shell: bash                 # only when known
Date: 2026-08-10            # day granularity — the one thing that changes per day
Git branch: main (dirty)    # whole line drops outside a repo; "(dirty)" appended on uncommitted changes
```

Facts are probed with bounded, silent-failure git calls so an unreachable repo
cannot stall prompt assembly. Only the date varies within a session — that is the
cache-stability contract.

### Composition invariants

- One text block (`llm.TextBlock`), not many; providers cache per-block.
- `buildSystem(s *State, env Environment, proj []ProjectInstruction)` stays
  deterministic given state+env; extension snippets and project instructions join
  through explicit inputs so tests can assert byte equality across calls with
  equal inputs.
- Changing the tool set or adding/removing project instructions changes this block;
  per principle 1, a change is announced with a one-line notice.

---

## Tool descriptions & schemas

Tools reach the model through the provider's tool-schema channel, **not as text
in the system block**. Each enabled tool contributes its name, a prose
`Description()` and JSON Schema parameters (derived from struct tags) to the
request's `Tools` list; only tools whose state is Enabled are sent — this is how
"only advertise what is real" is enforced.

The description is full sentences, not a one-line snippet. It states plainly
anything that changes how the model should use the tool:

- `read`: returns line-numbered text and refuses binary files; supports
  offset/limit paging.
- `bash`: runs in the session working directory; output is truncated with the
  full log spilled to a file, and timeout overrides the default. One shell process
  per call — there is no persistent `cd`.
- `write`: creates or overwrites files, making parent directories.
- `edit`: exact text replacement that applies atomically or not at all.

There is deliberately **no "Available tools" list inside the system prompt**:
the schema channel already tells the model exactly what it may call, and a second
text copy would only cost tokens.

### Split what the model sees from what the user sees

The tool result has a model-facing form and a display form. `edit` shows the user
a colourised diff but tells the model "applied"; `bash` streams full output to
the screen while handing the model a truncated, ANSI-stripped version with an
elision marker. The prompt contract is: **the model must know when it has been
told less than the whole truth** — truncation markers are not optional.

### Schema errors as feedback

`edit`'s "exact match" failures return actionable error text (nearest near-match,
occurrence count) because they are the model's main self-correction loop. The
same principle extends to every tool: an error result is a hint for how to retry,
not just a stop sign.

---

## `@`-file reference injection

When a user message contains `@path`, ajent injects a synthetic `read` call +
result pair ahead of that message, using the real tool. The literal `@path`
stays in the text; the injected read is what actually puts content in context.

Prompt implications:

- Using the real tool means one code path for truncation markers, binary
  refusal, line numbering and stale-read tracking — so the model sees exactly
  what it would see if it had called `read` itself.
- Injected reads are visible to compaction's superseded-pass and countable by
  token accounting. No special "reference" block type is ever
  shown to the model; a reference is just an ordinary read.

---

## Project instruction layering

The user-global `~/.ajent/AGENTS.md` (honouring `AJENT_HOME`) and `<cwd>/AGENTS.md`,
when they exist, are read once at startup in that order and injected into the
system block ahead of the first turn. The format mirrors how other agents present
project instructions early in context — a provenance-marked wrapper so the model
can tell project rules from conversation:

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
  sources are skipped. Nested discovery beyond these two — no ancestor walk.
- **Provenance marker carries the absolute path**, so the model can point at which
  instruction file told it something.
- Loaded once at startup and kept for the session; a changed `AGENTS.md` applies on
  next launch. There is no mid-session reload or file watching, keeping the system
  block cache-stable per principle 1.

---

## Prompt templates & slash commands

Reusable prompts live as **markdown files** discovered from config dirs; the
filename becomes the command name (`review.md` → `/review`). Frontmatter carries a
description and an optional argument hint:

```markdown
---
description: Review PRs from URLs with structured issue and code analysis
argument-hint: "<PR-URL>"
---
You are given one or more GitHub PR URLs: $@

For each URL, do the following in order:
1. Read the page in full — description, comments, commits, changed files.
2. ...
```

Argument substitution supports `$1`, `$@`/`$ARGUMENTS`, and `${N:-default}` /
slicing for optional or repeated arguments.

Design rules:

- **A template is a self-contained prompt.** It assumes nothing about prior
  context; it states the process, the output format (often with headings to fill
  in), and what not to do. The `/wr`-style finish prompt is a good model: numbered
  steps, explicit constraints ("never `git add .`"), and an exact closing line.

- The command registry (`/help`, `/model`, `/tools`, ...) is where templates are
  surfaced; unknown commands error rather than costing tokens.

---

## Compaction summarisation

Compaction runs free structural reductions first (stages 1–3: drop failed/superseded/
aborted tool calls, elide old output to shape summaries, strip retained thinking),
and only when they are not enough does it spend an LLM call on a summary (stage 4).

### The summariser system prompt

A dedicated, single-purpose instruction:

```
You are a context summarization assistant. Your task is to read a conversation
between a user and an AI assistant, then produce a structured summary following
the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in it.
ONLY output the structured summary.
```

### The initial summary instruction

The serialised history goes in `<conversation>...</conversation>` tags (treated as
data, not a live thread), followed by an exact-format spec:

```
The messages above are a conversation to summarize. Create a structured context
checkpoint that another model will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements]
- [(none) if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Data, examples, or references needed to continue]
- [(none) if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.
For content the assistant produced (code, prose, plans, answers), include a 2-3
sentence synopsis of its substance — never just a title or name.
```

This is the heart of resumption: **Goal / Constraints / Progress / Decisions /
Next Steps / Critical Context**, with a hard rule to keep file paths and line
numbers verbatim.

### Incremental update

When a prior summary exists it goes in `<previous-summary>` tags and the prompt
becomes an *update*, not a rewrite — preserving old information, moving progress,
adding new context:

```
The messages above are NEW conversation messages to incorporate into the existing
summary provided in <previous-summary>.

Update the structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context
- UPDATE Progress: move items In Progress → Done when completed
- UPDATE Next Steps based on what was accomplished
- PRESERVE exact file paths, function names, error messages

Use this EXACT format:
[the same six-section format]
```

This is how summaries merge rather than nest — each compaction refines one
checkpoint instead of piling summaries inside summaries.

### User guidance and turn-prefix cases

- `/compact <instructions>` appends the user's focus as a short "Additional
  focus: ..." line so the summary leans toward what they care about.
- When only a prefix of an oversized turn must be dropped, use a lighter format:
  Original Request / Early Progress / Context for Suffix — enough to understand
  the kept recent work without re-summarising everything.

### Re-injection

The compacted history is placed back in context with explicit provenance so the
model knows what it lost:

```
The conversation history before this point was compacted into the following summary:

<summary>
...
</summary>
```

A branch return uses similar framing ("a summary of a branch this conversation
came back from"). Nothing is ever deleted; `/compact undo` drops the newest entry.

### Honesty

Every compaction reports real numbers: `compacted 142k → 61k (dropped 18 failed
tool results, truncated 6 outputs, summarised 47 messages)`. Silent compaction
leaves users convinced the agent "forgot" for no reason.

---

## Tool-call classifier (`auto` mode)

The permission barrier classifies an unverifiable shell command in `auto` with a
one-shot call to the session's current model — **fresh context**, never the
session history, and its verdict never enters the session. It asks for exactly one
word: `readonly`, `write` or `unsure`. Reasoning is clamped minimal; the output
token budget leaves room for a thinking block. Verdicts normalise by lowercasing,
dropping non-letters and prefix-matching, so `` `readonly` ``, `read-only` and
`readonly.` all collapse to one word; anything else is `unsure`. The response is
never cached when unsure (usually transient: an abort, missing auth, an API
error); confident verdicts are LRU-cached per exact command string.

The prompt keeps the reference's framing — compound constructs classify by the
commands they actually run, examples are illustrative not exhaustive — with one
deliberate change: **reading from the network is *not* read-only**. The exfiltration
channel means "does not write locally" never equals safe; the classifier must say
so explicitly rather than inheriting the reference's opposite claim. Network tools
(`curl`, `wget`, `nc`) are absent from both the static allowlist and any notion of
classifier read-only.

---

## Testing prompting

Prompts are code. The bar is golden/verbatim tests over exact strings:

- **System prompt**: assert byte-equality across calls with equal inputs; assert
  cache stability across days (only the date differs); assert empty facts drop
  their lines and git failures fall silent.
- **Tool schemas & descriptions**: assert a tool appears in the request's `Tools`
  list only when enabled; guidelines derive correctly from the enabled set.
- **Compaction**: assert the exact six-section format is present, that user focus
  instructions and the previous summary are included, and (with a scripted fake
  provider) that the model received history in `<conversation>` tags treated as data.

Where a prompt is user-configurable (project instructions, templates) the tests
cover presence/absence of `<cwd>/AGENTS.md`, the provenance marker carrying its
absolute path, byte-stability across calls with equal inputs, and that project
instructions are appended after environment facts.
