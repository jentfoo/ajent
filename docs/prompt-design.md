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
changes: the day-granular date, project-instruction reloads, tool-set changes.
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
the result.** Summaries, sub-agent returns, classifier verdicts and review
output all use fixed headings/one-word answers with exact formats spelled out in
the prompt. The format is part of the contract; tests assert it verbatim.

**4. Preserve what resumption needs: file paths, line numbers, function names,
error messages — verbatim.** Generic "summarise the conversation" prompts lose
exactly the details that let a later model pick up where one stopped. Every
lossy prompt in this document carries an explicit instruction to keep those
artefacts exact.

**5. Only advertise what is real.** A tool appears in the prompt only when it is
enabled and has a snippet; skills are listed only when the `read` tool exists;
guidelines adapt to which tools are available. Advertising something that is not
there makes the model burn a round-trip discovering it, or worse, try and fail.

**6. Provenance everywhere.** Every block of injected content carries where it
came from: `<project_instructions path="/abs/AGENTS.md">`, a compaction summary
that says "history before this point", a sub-agent result that is the whole
return value. The model must never mistake one kind of text for another.

**7. Background prompts are short.** Compaction, classifier and plan kickoff
calls reuse the provider client, use minimal reasoning, ask for exactly what is
needed and no more. A long prompt on a call that runs every turn or per tool call
is a tax paid forever.

## Prompt surfaces

The prompt surfaces are covered in detail below, each with the contract that
governs its assembly and use.

| Surface | What it is |
|---|---|
| System prompt composition | identity + guidelines + environment facts + project instructions + tool snippets + skills |
| Tool descriptions & schemas | one-line snippet per enabled tool; JSON Schema params |
| `@`-file reference injection | synthetic read call+result ahead of the user message |
| Project instruction layering | AGENTS.md/CLAUDE.md discovery and provenance-marked assembly |
| Skills registry & injection | `<available_skills>` blocks with name/description/location |
| Prompt templates / slash commands | markdown templates expanded into prompts; `/init` survey |
| Compaction summarisation | staged free reductions + exact-format LLM summary |
| Sub-agent system prompt & contract | isolated read-only agents with an output contract and empty-summary nudge |
| Tool-barrier deny reasons & classifier | denial reason becomes the error text; one-word verdict classifier |
| Plan workflow kickoff/review prompts | role-specific kickoffs that assume zero prior context |

---

## System prompt composition

Assembled by `buildSystem` into a single cache-stable text block. Project
instructions, tool snippets and skills are layered on top of the ordered base
parts without touching the loop.

### Parts, in order

```
1. Identity sentence        — one neutral line naming the harness + repo cwd.
2. "How you help" line      — read files, run commands, edit code, write new files.
3. Available tools          — only enabled tools with their snippets.
4. Guidelines               — concise bullets; some derived from which tools exist.
5. Environment facts        — clean structured lines: cwd, platform, shell, date,
                              git branch + dirty state.
6. Project instructions     — provenance-marked AGENTS.md/CLAUDE.md blocks.
7. Skills                   — <available_skills> when read is available (optional).
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
- Use bash for file operations like ls, rg, find
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
- `buildSystem(s *State, env Environment)` stays deterministic given state+env;
  extension snippets and project instructions join through explicit inputs so
  tests can assert byte equality across calls with equal inputs.
- Changing the tool set or reloading project instructions changes this block;
  per principle 1, the change is announced with a one-line notice.

---

## Tool descriptions & schemas

A tool is described to the model by a **one-line snippet** plus its JSON Schema.
The snippet appears in `Available tools` only when the tool is enabled and has a
snippet — this is how "only advertise what is real" is enforced.

```
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement
- write: Create or overwrite files
```

Each snippet is a verb phrase plus the minimum context to invoke it. Descriptions
must state plainly anything that changes how the model should use the tool:

- The sub-agent tools carry an explicit "no session context — pass
  file paths and key facts, not content" contract.
- `bash` notes one shell process per call so the model does not assume persistent
  `cd`.
- A read-only bar on some tools is stated in their snippet.

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

Discover `AGENTS.md`/`CLAUDE.md` from the user-global dir down through cwd and up
to the workspace root, deduplicate by canonical path (a worktree's copy shadows
the repo's), then assemble with provenance markers:

```
<project_context>

Project-specific instructions and guidelines:

<project_instructions path="/abs/AGENTS.md">
...file body...
</project_instructions>

<project_instructions path="/repo/docs/CLAUDE.md">
...file body...
</project_instructions>

</project_context>
```

Rules:

- **Order** is significant: user-global first, then ancestor→descendant so more
  specific instructions come later and can override.
- **Provenance markers carry the absolute path.** The model must be able to point
  at which instruction file told it something.
- Watch files for changes mid-session; reload at turn boundaries (never mid-turn,
  never between a tool call and its result).
- `/init` generates one by having the agent survey the repository with a prompt
  that asks for exactly what belongs in an instruction file: project layout,
  build/test commands, conventions — not prose about itself.

---

## Skills registry & injection

If ajent ships skills (specialised instruction files), they are injected as an
`<available_skills>` block, listed only when the `read` tool exists:

```
The following skills provide specialized instructions for specific tasks.
Read the full skill file when the task matches its description.
When a skill references a relative path, resolve it against the skill directory and use that absolute path in tool commands.

<available_skills>
  <skill>
    <name>...</name>
    <description>...</description>
    <location>/abs/path/to/SKILL.md</location>
  </skill>
</available_skills>
```

The block is an index, not the content: descriptions are short so the model can
match a task to a skill and then load the file. Skills with `disableModelInvocation`
are omitted entirely.

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

## Sub-agent system prompt & contract

A sub-agent is a fresh `agent.Agent` with an in-memory session, read-only tools,
and its own system prompt: the parent's neutral identity/guidelines minus most
tools, plus explicit constraints and an output contract. The two non-negotiables:

**No-session-context warning.** The sub-agent has none of the main conversation.
Its tool descriptions must say this plainly so models do not delegate work that
needs context the child cannot see — pass file paths and key facts, not content.

```
You are a research agent working in an isolated context. You have no access to
the caller's session or its history.

- The task text below is everything you know; if it lacks a path or fact,
  investigate for it rather than assuming.
- You are READ-ONLY: read, find and grep only. You cannot edit files, run the
  main agent's tools, or spawn further sub-agents. Tasks requiring edits must be
  done directly by the caller — do not attempt them here.

Task: <task>

Your final assistant message is your entire return value. Make it self-contained:
conclusions first, then file paths with line numbers and any caveats.
```

**The output contract.** The last assistant message *is* the deliverable —
self-contained, conclusions-first, with exact paths/line numbers and caveats.

**Empty-summary retry.** A reasoning model whose final message is thinking-only
returns nothing. Nudge once or twice — "output the summary now, no tool calls" —
then return a placeholder `(sub-agent produced no output)` rather than looping.

### Role prompts for delegated work

Specialised sub-agents each carry their own system prompt and structured output
format so results are usable by whatever consumes them:

- **Scout** — find relevant code fast; return Files Retrieved (with line ranges),
  Key Code, Architecture, Start Here. Its findings go to an agent that has *not*
  seen the files.
- **Planner** — read-only analysis producing Goal / Plan / Files to Modify /
  New Files / Risks, concrete enough for a worker to execute verbatim.
- **Worker** — full capabilities in isolation; returns Completed / Files Changed /
  Notes (plus handoff fields when passing to a reviewer).
- **Reviewer** — reads diffs and code read-only; returns Critical / Warnings /
  Suggestions / Summary with specific paths and line numbers.

The pattern across all four: a role identity, an explicit capability boundary,
and a fixed output format that another model can consume without re-reading the
source files. This is the single biggest context-efficiency lever in the project —
findings enter the main context as three paragraphs, not fifty tool results.

---

## Tool-barrier deny reasons & classifier

The barrier gates tools between "read-only work runs unprompted" and
"destructive calls ask first". Its prompts are part of that contract.

### Denial reason = error text

A denied call becomes an `IsError` result whose text is the model's feedback.
Denials without a reason waste the turn; with one, the model usually adapts. The
reason should say what to do instead:

- `sed -i` → "use the edit tool" (a hard reject).
- A write outside allowed scope → state the path rule.

### Allow-with-note

When the user allows a call and adds a note, that note is injected into context so
the model adapts for the rest of the session:

```
User allowed `rm` with note: only inside build/
```

This turns one approval into durable behaviour rather than a single permit.

### Model classifier (auto mode)

For commands the static analyser cannot verify, and only in auto/permission modes,
one short call asks for exactly **one word** against a fresh, minimal context
(never the session's):

```
Classify this shell command as exactly one of: readonly, write, unsure.
Only output the single word.

Command:
<command>
```

Rules that keep it cheap and correct:

- Skip entirely when static analysis already found a confident write (it would
  prompt anyway).
- Cache verdicts per exact command string in a session LRU (~500); never cache
  `unsure` — it is usually transient.
- Reuse the provider client; minimal reasoning.

---

## Plan workflow kickoff/review prompts

The plan → implement → review loop runs two models against projected contexts, so
every stage's prompt must be **self-contained**: each receiving model has *no*
prior context beyond what the projection gives it.

**Kickoff prompts state this explicitly.** The single biggest quality lever in the
whole workflow is that every kickoff says the plan/revision must carry every file
path and fact because nothing else does:

```
You are working on one stage of the plan below. You have NO prior context from
the stages before it.

The plan below carries every fact you need — read it fully before acting.
Preserve all file paths exactly; do not invent or assume paths the plan does not
give you, and investigate rather than guess when one is missing.

<plan / revision instructions>
```

- **Planning** kickoff: explore read-only (read/find/grep/bash gated by barrier,
  plus sub-agents), ask clarifying questions, stay in planning until it calls the
  implement transition.
- **Implementing**: runs against a projected context of only its own plan kickoff
  (+ reviewer instructions on later rounds) and its work. Tools include edit/write.
- **Reviewing**: sees `[planning through dev_implement] + [all prior review rounds]
  + [git status --porcelain]` — never the implementation chatter in between, so it
  reviews the plan's intent against actual state.

**Revision instructions must be self-contained.** A `dev_revise(instructions)`
payload is fed to a fresh implementing segment that has no memory of the previous
round; therefore the revision text names files and exact changes. The reviewer
stalls without calling a control tool → ask the user rather than guessing.

Robustness wording worth keeping: review starts whether or not `dev_review` was
called (a stopped implementor is finished); an implementor turn ending in provider
error retries up to 3 times so review never runs against untouched work; every
kickoff that could be interrupted (`Esc`, `/plan-stop`) restores model, tools and
reasoning level on the way out.

---

## Testing prompting

Prompts are code. The bar is golden/verbatim tests over exact strings:

- **System prompt**: assert byte-equality across calls with equal inputs; assert
  cache stability across days (only the date differs); assert empty facts drop
  their lines and git failures fall silent.
- **Tool snippets & schemas**: assert a tool appears only when enabled-with-snippet;
  guidelines derive correctly from the enabled set.
- **Compaction**: assert the exact six-section format is present, that user focus
  instructions and the previous summary are included, and (with a scripted fake
  provider) that the model received history in `<conversation>` tags treated as data.
- **Sub-agent**: assert tool-set enforcement (`agent_*` absent; no write reachable),
  the empty-summary nudge sequence bounded to two, and that the output contract is
  in the system prompt verbatim.
- **Barrier classifier**: assert one-word verdicts, cache hit/miss behaviour, and
  that `unsure` is never cached.
- **Plan kickoff**: assert the projected context for each stage contains only its
  allowed segments, and that revision prompts are self-contained.

Where a prompt is user-configurable (project instructions, templates) the tests
cover discovery order, dedupe by canonical path, provenance markers, and reload at
turn boundaries — never mid-turn.
