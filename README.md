# ajent

[![license](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/jentfoo/ajent/blob/main/LICENSE)
[![Tests - Main Push](https://github.com/jentfoo/ajent/actions/workflows/tests-main.yml/badge.svg)](https://github.com/jentfoo/ajent/actions/workflows/tests-main.yml)
[![Vibe-Scale 4.0(V2|U1|T2): Significant AI, test gaps](https://img.shields.io/badge/Vibe--Scale%204.0(V2%7CU1%7CT2)-Significant%20AI%2C%20test%20gaps-ff7f0e)](https://github.com/vibesdk/vibe-scale/blob/main/scale/vibe-4.md#v2-u1-t2-score-40--significant-ai-test-gaps)

A very lightweight, minimal CLI coding agent interface written in Go.

<img width="640" height="380" alt="demo" src="https://github.com/user-attachments/assets/629b7935-b2d8-46c5-b0d5-2f9c62220e89" />

The project is deliberately opinionated. It trims the UI to maximize working space and makes firm choices about things like sub-agents and tool barriers. Those opinions are an ongoing exploration of CLI agent ergonomics, so expect them to shift as the project evolves.

Feedback is welcome. Please open issues if you have any suggestions or comments.

## Install

```sh
go install github.com/jentfoo/ajent@latest
```
## Design Philosophy

### TUI

One of the hardest things working with coding agents is needing to copy content out of the terminal. The output is either damaged, or padded and suffixed with spaces.

Our agent simplifies the TUI in order to provide a terminal native output. Everything above the prompt bar is permanent and will never be changed. It is only the lower section of the agent window which is dynamic, resizing for prompts and showing the activity of sub-agents or other tool calls.

<img width="640" height="309" alt="tools demo" src="https://github.com/user-attachments/assets/e17b834c-f3e9-407d-99d1-b870e9fc0d0c" />

Tab support is available for file path completions, but our TUI favors a minimal form that is more conductive to power users. No notice of files available until you hit tab twice failing to complete a path.

<img width="640" height="91" alt="files tab completion" src="https://github.com/user-attachments/assets/65ba1fb8-2577-4df1-8af4-9bbea10b5b3d" />

### Tool Barriers

Our CLI agent attempts to balance autonomy and safety. This is done through a variety of permission modes:

* **allow-read** (default) - Will automatically allow any read only tools or MCP tools which are marked as read only in their configuration. Any write or questionable bash operations will require user approval first.
* **auto** - Still attempts to provide a read only experience by default, but will delegate to the agent to make decisions on bash commands and MCP/extension tool calls which are not automatically allowed.
* **auto+write** - Auto-approves `write`, `edit` and bash operations that stay safely inside the working directory or temp directory; anything touching files elsewhere, system changes, bulk deletion or network access still requires approval.
* **allow-all** - All operations allowed without human approval.
* **block-all** - Nothing runs without explicit approval, reads included.

When a write operation does need approval, you're presented with a dialog that lets you steer, allow once, or allow for the session (or until the barrier mode is changed).

<img width="639" height="164" alt="permission check" src="https://github.com/user-attachments/assets/8b81665a-f97c-4681-a099-7c4c26c5c718" />

### Sub-agents

Sub-agents only function in a read only form. They exist only to keep the main context free concise, offloading the exploratory research to come back with a targeted summary for the main context history. They are enabled by default, and also gain access to any `readOnly` MCP services configured.

### Project instructions

`AGENTS.md` in the working directory (and `~/.ajent/AGENTS.md` for habits you want everywhere) is read at startup and injected into every turn's system prompt. Editing it applies on the next start, not mid-session.

`/init` writes that file for you. It reads the project's `README*` itself, then fans the build and the codebase out to read-only sub-agents in parallel — one surveying the Makefile, CI config and `CONTRIBUTING.md`, and one to four splitting the tree between them, scaled to its size and balanced by where the code actually is. Their summaries come back as ordinary tool results, and a single turn distills them into `AGENTS.md`. The write goes through the normal permission barrier, so you see the diff and approve it like any other. Run `/init` again later and it corrects the existing file against a fresh survey rather than replacing it. Esc cancels a survey in progress.

### Tools

MCP and other tools are loaded on the first message (using `/tools`). Once a tool is loaded it can't be unloaded, however we do allow adding tools later at the cost of a cache miss.

## Configuration

### Providers

Providers, and the models they serve, are configured in `~/.ajent/models.json`, a format that is generally a superset of pi model configurations. A provider is one endpoint speaking one wire dialect. The two dialects are `anthropic-messages` and the OpenAI family (`openai-responses` or `openai-completions`). A gateway serving two dialects at once is simply two providers.

```jsonc
{
  "providers": {
    "lmstudio": {
      "baseUrl": "http://127.0.0.1:1234/v1",
      "api": "openai-completions",
      "timeouts": { "idle": "0s" }
    },
    "anthropic": {
      "apiKeyEnv": "ANTHROPIC_API_KEY"
    }
  }
}
```

Per provider you can set:

* `baseUrl` - the endpoint
* `flavor` - selects discovery and quirk defaults (`anthropic`, `openai`, `openrouter`, `lmstudio`, `llamacpp`, `generic`). Defaults to the provider key, so an OpenAI-compatible proxy in front of a known server can say `{"flavor": "lmstudio"}` and still get the right behavior.
* `apiKeyEnv` / `apiKey` - where to read the secret. Resolution order is the configured env var, then a literal key, then the dialect's conventional variable (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, etc.).
* `headers` - extra request headers
* `timeouts` - connect / TLS / header / idle / total bounds as Go durations; an explicit `"0s"` disables a bound. Split these because one client timeout would clamp the body read, which is exactly what must be allowed to take minutes.
* `retry` - attempts and backoff (`base`, `max`, `jitter`); honors `Retry-After` but caps it at 60s.
* `discover` - whether this provider's models are fetched from the server. On by default for openrouter, lm-studio and llama.cpp; any other OpenAI-compatible endpoint can opt in with `"discover": true`, which asks its `/v1/models`.
* `models` - your declared list for this provider. When present, it *is* the whole list; discovery only fills gaps.

### Models

Models come from exactly two places, both under the provider they belong to:

* **hand-written declarations** - your per-provider `models` list.
* **provider discovery** - asking openrouter, lm-studio or llama.cpp for their model list. It fills any gaps in what you declared. A llama.cpp multi-model router (which reports nothing useful on `/props`) falls back to its OpenAI-compatible `/v1/models`, and any other server speaking chat-completions can be discovered the same way with `"discover": true`.

The minimal entry (if not using discovery) is just an id. Everything else has a sane default, so you typically only add one to pin something discovery got wrong (a name or context window).

```jsonc
{
  "providers": {
    "lmstudio": {
      "models": [
        {
          "id": "deepseek-v4-flash-0731",
          "name": "DeepSeek V4 Flash",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 400000,
          "maxTokens": 20000
        }
      ]
    }
  }
}
```

Per model you can set:

* `id` (required) - the identifier; also how overrides address it.
* `name` / `aliases` - display name and extra names the registry resolves.
* `reasoning` - a boolean: `true` enables reasoning with the model's resolved thinking format. There is no style-name form; that matches pi.
* `input` - accepted modalities (`text`, `image`); defaults to text plus image when capabilities allow it.
* `contextWindow` / `maxTokens` - input window and output cap in tokens.
* `compat` - the capability overrides for this model: thinking format, tokenizer, cache-control encoding, parallel tool support, temperature, images, and so on. Every field is a pointer internally, so an override turns one quirk on without restating the others.
* `thinkingLevelMap` / `thinkingBudgets` - how our seven reasoning levels (`off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`) map to this provider's effort values and token budgets; a `null` entry omits the parameter for that level.
* `samplingParams` / `headers` / `api` / `baseUrl` - opaque additions folded into the request body, per-request headers, or a dialect/endpoint override so one gateway can serve two dialects.

Reasoning retention is configured globally rather than per model (`reasoning.retain`, below) and applied only to what is sent; the transcript always keeps everything.

The JSON loader is deliberately lenient: `//` comments and trailing commas are accepted, duplicate keys warn instead of silently keeping the last one, and an unrecognised key or reasoning level warns rather than locking you out. An unrecognised `api` disables its provider (a wrong dialect is a wrong protocol); an unknown flavor just degrades to generic.

#### Minimal local setup

The fastest start is no declaration at all: llama.cpp discovers whatever model you have loaded, so pointing it at a server serving `qwen3-27b` needs only the endpoint:

```jsonc
{
  "providers": {
    "llamacpp": { "baseUrl": "http://127.0.0.1:8080" }
  }
}
```

The host defaults to `localhost:8080`; discovery fills in the model's name and context window from what the server reports. Declare an entry only when you want to pin a window or override something discovery got wrong:

```jsonc
{
  "providers": {
    "llamacpp": {
      "baseUrl": "http://127.0.0.1:8080",
      "models": [
        { "id": "qwen3.8-27b", "contextWindow": 128000 }
      ]
    }
  }
}
```
### ~/.ajent/config.json

This file holds everything else: which model starts a session, how the agent behaves, what the barrier allows, and how the UI looks. It is one of several layers resolved lowest-to-highest (default → `~/.ajent/config.json` (user) → `<workspace>/.ajent/config.json` (project) → `<workspace>/.ajent/config.local.json` (local) → `AJENT_*` env vars → command-line flags → live session changes). Objects fold deeply; arrays and scalars replace wholesale.

A literal API key should only be used in the **user** layer, where a group- or world-readable file triggers a warning. Prefer an env var (`apiKeyEnv`) for anything shared.

Any scalar key at dotted path `p.q.r` binds to the environment variable `AJENT_P_Q_R`, so `permissions.mode` is `AJENT_PERMISSIONS_MODE`. `AJENT_HOME` overrides the whole config directory.

```jsonc
{
  "model": "anthropic/claude-opus-4",
  "reasoning": { "level": "high", "retain": "wholeTurn" },
  "agent": { "maxSteps": 40 },
  "permissions": {
    "mode": "auto",
    "safeCommands": ["git status", "npm test"],
    "deniedCommands": ["rm -rf"]
  },
  "compaction": { "auto": true, "threshold": 0.8, "minSteps": 2, "verbatimFraction": 0.1 },
  "subagent": { "maxConcurrent": 4 },
  "ui": { "render": "auto", "theme": "dark-warm" },
  "disableUpdateCheck": true
}
```

The top-level blocks:

* `model` - the model a fresh session starts with. A `/model` change writes your most recent choice here, so it is remembered across restarts.
* `reasoning` - the default reasoning level (`level`, one of the seven), how much thinking to retain when sending history (`retain`: `none`, `lastTurn`, `wholeTurn`, `all`), an optional token `budget`, and whether reasoning is shown (`show`).
* `agent.maxSteps` - an optional cap on one turn's tool-calling iterations; absent or zero means unlimited.
* `providers` / `models` - the same overrides you can put in `~/.ajent/models.json`, folded over it. These let a project pin its own endpoint or widen a context window without duplicating the whole file.
* `tools.enabled` and `tools.limits` - which built-ins start enabled (defaults are just `read`, `write`, `edit`, `bash`) and per-tool output bounds (`lines`/`bytes` for bash, read, find, grep, ls, refInject, refTotal).
* `permissions.mode` - the barrier mode: `allow-read` (default), `auto`, `auto+write`, `allow-all`, or `block-all`. See Tool Barriers above; a Shift+Tab cycle changes it for the session only.
* `permissions.safeCommands` / `deniedCommands` - extra auto-allow and hard-deny rules. Each entry is an exact tool name, a whole MCP server namespace, or a bash command line matched at token boundaries (so `git status` covers its subcommands, and wrapping in `cd … &&` never defeats either list).
* `compaction.auto` / `threshold` - whether automatic context reduction is on, and the fraction of the window (or an absolute token count) at which it fires; default 0.8. `auto: false` stops the automatic trigger only: `/compact` still works, and an overflow still recovers. A model that sets its own `compactThreshold` in `models.json` keeps it; `threshold` is the default for the ones that do not.
* `compaction.minSteps` / `verbatimFraction` - how much recent work a compaction keeps byte-exact. A *step* is one assistant message plus the tool results it produced. At least `minSteps` of them survive whatever they weigh (default 2), extended with older steps while the kept region stays within `verbatimFraction` of the compaction point (default 0.1). Nothing in that region is ever stubbed, elided or thinning-stripped; everything older is replaced by a single structured summary. Every trigger keeps the same band.
* `subagent.model` / `maxConcurrent` - a dedicated model for research sub-agents (empty inherits your session model) and how many may run at once (default 8).
* `ui.render`, `ui.theme`, `showCost`, `showThinking` - paint mode (`auto`, `inline`, `alt`, `plain`), palette, and whether cost or thinking are shown.
* `ui.color` - colour depth: `auto` (default), `none`, `basic`, `256` or `true`. `auto` reads `TERM` and `COLORTERM`; any other value names the depth outright, which is the way out if your terminal is classified badly — `256` turns on syntax highlighting where detection was too conservative, `none` turns colour off entirely. `AJENT_UI_COLOR=none` sets it for a single run.
* `disableUpdateCheck` - turn off the startup update-available notice. Off by default; a fork install or an offline machine that does not want to nag can set this once in the user layer.

### Command-line options

The CLI is deliberately small. Run `ajent --help` for the full list; the important ones:

```
-v, --version        print the build version and exit
-m, --model <key>      initial model to use
    --render <mode>    paint mode: auto, inline (terminal scrollback),
                       alt (own scrollback), plain
    --continue         resume the most recent session automatically
    --resume [id]      list saved sessions and pick one; with an id,
                       resume that session directly
    --update           reinstall ajent from @latest in the foreground, then exit
-p, --prompt <text>    run one turn non-interactively, print the result and exit
-o, --output <shape>   one-shot output: text (final answer) or json (one event per line)
    --allow-all        one-shot: offer every tool, bash included
    --read-only        one-shot: offer only read-only tools
    --allow-tools      one-shot: extra tool names to offer
    --deny-tools       one-shot: tool names to withhold
```

The `-p/--prompt` flags turn the interactive agent into a scriptable one shot. There is no dialog in headless mode, so the barrier runs at allow-all and the **offered tool set** carries the policy instead (the model is only ever handed tools it may call, which keeps it from wasting steps discovering a refusal). Scope flags (`--allow-all`, `--read-only`) are mutually exclusive; `--allow-tools` / `--deny-tools` refine either.

Self-updates run `go install github.com/jentfoo/ajent@latest`. The `/update` command does it in the background inside a session, or the `--update` flag runs it in the foreground.

Exit codes are stable enough to branch on in scripts: `0` the turn answered, `1` bad usage or setup failure before the turn, `2` the turn itself failed or produced nothing. They are identical for text and json output.

