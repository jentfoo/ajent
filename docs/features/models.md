# Models

Where models come from and how they are configured: the `~/.ajent/models.json`
file that defines providers, the discovery that fills in what you do not
declare, and the reasoning controls (levels, budgets, retention, visibility)
that shape how a model thinks. The first-run provider wizard is covered in
`cli.md`, every config.json key with its layer and `AJENT_*` env binding in
`config.md`, and the commands themselves (`/model`, `/reasoning`, `/reload`)
in `commands.md`.

## Where models come from

ajent ships no model catalogue. Everything the picker offers comes from two
places, both under the provider they belong to:

- **declarations** - your per-provider `models` list in models.json
- **discovery** - asking the provider for its model list (see below)

The merge rule: when a provider declares models, that declaration is the
whole list for that provider. Discovery fills fields an entry left unset (a
real context window, modalities) and never removes entries. It adds models to
a declared list only through `discoverAdd` (below). A provider that declares
nothing gets whatever discovery reports. A model that left the catalogue keeps
serving until you switch away.

Nothing is compiled in except per-flavor endpoint and quirk defaults, so a
bare `"id"` is already a complete model entry: it gets a name derived from the
id, text input, and the reasoning baseline its flavor sets.

## The models.json file

`~/.ajent/models.json` (or `$AJENT_HOME/models.json`) is the only models file.
There is no project-level layer because a provider entry may hold a literal API
key. It is written with secret permissions, and a save over a file that is
group or world readable warns.

The loader is deliberately lenient:

- `//` line comments and trailing commas are accepted, so an existing
  configuration pastes in unchanged
- a missing, empty or comment-only file loads as no providers
- unrecognized keys warn instead of failing, so a typo never locks you out
- duplicate keys warn (a repeated key would otherwise silently keep the last)
- any other top-level value that is not an object rejects the file with the
  line and column it is on

```jsonc
{
  "defaultModel": "lmstudio/deepseek-v4-flash",   // what a fresh session picks
  "providers": {
    "lmstudio": {
      "baseUrl": "http://192.168.1.100:1111/v1",
      "api": "openai-completions",
      "timeouts": { "idle": "0s" },
      "models": [ /* ... */ ]
    },
    "anthropic": { "apiKeyEnv": "ANTHROPIC_API_KEY" }
  }
}
```

`defaultModel` is optional and resolves like any model name. config.json's
`model` key overrides it when set (`config.md`), and the `-m` flag overrides
both. A configured default that resolves to nothing warns and falls back.

## Providers

One provider entry is one endpoint, and its `api` value names which request
style ajent speaks to it: `anthropic-messages`, `openai-responses` or
`openai-completions`. A gateway serving two of those at once is simply two
providers. An unrecognized `api` disables just that provider with a warning
naming the valid choices.

Per provider you can set:

| Field | Description |
|---|---|
| `baseUrl` | the endpoint. Omitted means the flavor's own default, so a bare `"anthropic"` or `"zai"` entry needs nothing else. The OpenAI-compatible chat path is appended verbatim, and a trailing `/v1` is accepted either way (local server flavors normalize both spellings to one request) |
| `flavor` | selects discovery endpoints and quirk defaults. Defaults to the provider key when that names a known flavor. An unknown flavor degrades to `generic` with a warning |
| `apiKeyEnv` / `apiKey` | where the secret comes from, in this order: the configured env var, then the literal key, then the request style's conventional variable (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, ...), then an error naming what to set |
| `headers` | extra request headers |
| `timeouts` | connect / TLS / header / idle / total bounds as durations. An explicit `"0s"` disables a bound, which is how you wait out a just-in-time model load (lmstudio's defaults already release header and idle, llamacpp its idle) |
| `retry` | transport retry policy: `attempts`, `base`, `max`, `jitter`. Honors a server's `Retry-After` but caps the wait, so an agent never sleeps silently instead of failing |
| `discover` | whether this provider's models are fetched from the server. On by default for every flavor with a known model list (below), and any other OpenAI-compatible endpoint opts in with `"discover": true`, which asks its `/v1/models` |
| `discoverAdd` | what discovery may add to a declared list: `"loaded"` adds only entries the endpoint reports resident right now, and `"all"` adds everything it listed. Unset keeps your declaration as the whole list. Aimed at lazy-loading proxies like llama-swap, where an unloaded entry still serves a turn |
| `routing` | openrouter only: upstream provider preference (`order`, `allowFallbacks`, `dataCollection`, `only`, `ignore`, `sort`, `max_price`, ...). Both ajent's camelCase and OpenRouter's own snake_case spellings load unchanged |
| `compat` | quirk defaults applied to every model under the provider. Same fields as a model's own compat block (below) |
| `models` | your declared list for this provider, see Model declarations |
| `modelOverrides` | partial edits onto discovered models by id, see below |
| `disabled` | `true` configures without connecting |

A few keys are accepted but unused (`name`, `oauth`, `authHeader`), so files
written for other tools load cleanly.

### Flavors

The built-in flavors: `anthropic`, `openai`, `zai`, `zai-coding-cn`,
`openrouter`, `deepseek`, `together`, `xai`, `groq`, `mistral`, `moonshotai`,
`moonshotai-cn`, `kimi`, `google`, `cerebras`, `nvidia`, `huggingface`,
`minimax`, `minimax-cn`, `baseten`, `ant-ling`, `fireworks`, `qwen-token-plan`,
`qwen-token-plan-cn`, `xiaomi`, `xiaomi-token-plan-cn`,
`xiaomi-token-plan-sgp`, `xiaomi-token-plan-ams`, `lmstudio`, `llamacpp` and
`generic`.

A flavor only picks defaults, never the protocol. That is what lets an
OpenAI-compatible proxy in front of a known server borrow it: say
`{"flavor": "lmstudio"}` on any provider key and you get lmstudio's timeouts,
thinking format and discovery endpoints pointed at your `baseUrl`. The hosted
catalogue flavors each carry their real endpoint and conventional env var, so
regional variants (`-cn`, token-plan regions) are reachable in models.json even
though the first-run wizard does not list them (`cli.md`).

Discovery is on by default for every flavor with a known model list:
`anthropic`, `openai`, the z.ai pair, `openrouter`, `deepseek`, `together`,
`xai`, `groq`, `mistral`, both moonshot flavors, `google`, `cerebras`,
`nvidia`, `huggingface`, `baseten`, `ant-ling`, the token-plan flavors and both
local servers. The anthropic-protocol flavors (`kimi`, `minimax`, `fireworks`)
offer no model list, so their wizard row asks for one id instead.

## Model declarations

```jsonc
{
  "providers": {
    "lmstudio": {
      "models": [
        {
          "id": "deepseek-v4-flash-0731",
          "name": "DeepSeek V4 Flash",
          "aliases": ["flash"],
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 400000,
          "maxTokens": 40000
        }
      ]
    }
  }
}
```

Per model you can set:

| Field | Description |
|---|---|
| `id` (required) | the provider's model identifier, also how overrides address it |
| `name` / `aliases` | display name and extra names the registry resolves. The picker shows the name when set, falling back to the id |
| `reasoning` | a boolean: `true` enables reasoning with the model's resolved thinking format. There is no style-name form (see Reasoning levels) |
| `input` | accepted modalities (`text`, `image`). Defaults to text plus image when the model's capabilities allow it. Images sent to a text-only model degrade to a placeholder note instead of failing the request |
| `contextWindow` / `maxTokens` | input window and output cap in tokens. Discovery fills these when unset, so you typically declare them only to pin something discovery got wrong. A zero window leaves the context bar without a denominator and auto-compaction never fires |
| `contextReserve` | tokens held back from the window for the response: a fraction below 1 (default 0.2) or an absolute count at 1 or more, clamped to 90% of the window |
| `compactThreshold` | where automatic compaction fires for this model: a fraction of the window (or an absolute token count). Overrides `compaction.threshold`, which is otherwise the default (`config.md`) |
| `api` / `baseUrl` | per-model request-style and endpoint override, so one provider entry can serve two styles |
| `compat` | capability overrides for this model: thinking format, tokenizer, cache-control encoding, parallel tools, temperature, images, and so on. Every field is optional, so an override turns one quirk on without restating the others. A set field the resolved request style does not use warns |
| `thinkingLevelMap` / `thinkingBudgets` | how ajent's reasoning levels map onto this provider's effort values and token budgets (below) |
| `headers` | per-request headers, merged over the provider's |
| `samplingParams` | opaque additions folded into the request body |

### Resolving a name

`/model claude`, `-m flash` and config `model` all resolve through one match
order: alias, then `provider/id`, bare id, unique id suffix, unique key
substring, unique name substring. An ambiguous match is an error listing the
candidates rather than a coin flip. Two entries sharing one provider's id both
stay listed with a warning, and only `provider/id` picks between them.

### modelOverrides

`modelOverrides` adjusts a discovered model by id without restating it as a
full declaration:

```jsonc
"lmstudio": {
  "discover": true,
  "modelOverrides": {
    "qwen3.8-27b": { "contextWindow": 128000, "reasoning": false }
  }
}
```

It accepts the display and capability fields (`name`, `reasoning`, `input`,
`contextWindow`, `maxTokens`, `compat`, `thinkingLevelMap`, `headers`,
`samplingParams`) but cannot introduce a model, rename an id or move an
endpoint. An override on an id you declared is inert and warns: edit the
declaration instead (with or without `discoverAdd`, a declared entry always
beats its discovered twin). Overrides naming ids discovery has not reported yet
are kept, not warned about.

## Discovery

Discovery runs in the background and never blocks startup or fails the run:
an offline start with a populated cache works, and an offline start with no
cache still resolves declared models. Results cache under
`~/.ajent/cache/models.json` and refetch is conditional (`ETag` /
`Last-Modified`, so a 304 keeps the models). Cached results stay fresh for 24
hours on hosted catalogues and one minute on local servers, whose loaded model
changes far more often.

Endpoint behavior worth knowing:

- lmstudio asks `/api/v1/models` (0.4 and later), falls back to `/api/v0` for
  older builds, then the OpenAI list. Its native report includes the context
  length the model was *loaded* with, which is often smaller than its maximum
- llamacpp reports only id and window on `/props`, so a multi-model router
  that serves no single loaded model falls through to `/v1/models`
- hosted catalogues report the most: identifiers, windows, completion sizes,
  modalities, reasoning and tool support. OpenRouter's list is also where a
  hosted model's reasoning flag comes from
- the `generic` flavor discovers only through the OpenAI list, and only when it
  opted in

At startup ajent refreshes every provider quietly: models discovery adds show
up as one notice (`discovered N more models`). The active pointer survives a
refresh in place, so new window or capability data take effect without a
restart. `/reload` re-reads the file against the live session too (see
`commands.md`).

## Reasoning levels

Seven levels name how hard a model thinks: `off`, `minimal`, `low`, `medium`,
`high`, `xhigh`, `max`. The default is `medium` (`reasoning.level`),
changeable per run with `/reasoning <level>` and for the session in
`/settings`.

A model offers what it can express. Most models take every level, translating
them through their own effort vocabulary:

- a model's `thinkingLevelMap` translates the seven names onto its provider's
  values, including the ones that are spelled differently (`"xhigh": "high"`)
  or sent as numbers. A `null` entry omits the reasoning parameter entirely for
  that level, and `"off": null` marks a model that cannot stop thinking (its
  requests then never claim thinking is off)
- `xhigh` and `max` are opt-in: they appear in `/reasoning` only when the map
  gives them an explicit non-null entry
- a level a model does not support snaps to the nearest one it does. `/reasoning`
  warns about the snap rather than doing it silently, and `off` always stays
  off

Anthropic models always carry an explicit thinking marker: disabled when the
level is off, a token budget when on. Temperature is dropped for those requests
rather than erroring, since Anthropic rejects the pair.

## Reasoning budgets

A budget caps the reasoning phase in tokens and overrides whatever the level
would pick:

- `reasoning.budget` in config.json (or `/settings`) sets an explicit cap. It
  reaches anthropic as `budget_tokens` and openrouter as a reasoning token cap.
  Anthropic raises small values to its own 1024-token floor, and the cap always
  leaves room for an answer of at least ~1k tokens
- without one, levels map onto a built-in ladder: minimal 1024, low 2048,
  medium 8192, high 16384, xhigh 32768, max 64000, clamped under the model's
  output cap
- a model's `thinkingBudgets` in models.json overrides that ladder per level
  for models with a reasoning token budget request field (vLLM-style), so a
  local server can get exact values:

```jsonc
{ "id": "qwen3.8-27b", "reasoning": true,
  "thinkingBudgets": { "medium": 4096, "high": 12288 } }
```

## Reasoning retention

`reasoning.retain` controls how much of a model's past thinking rides back
into later requests:

| Policy | What is sent |
|---|---|
| `none` | no thinking at all |
| `lastTurn` | only the most recent assistant message keeps its thinking |
| `wholeTurn` | the current tool-calling turn keeps it, completed turns strip. The default |
| `all` | nothing is ever stripped |

Retention edits what is sent, never the transcript: everything stays on disk
and in context accounting.

Leave `retain` unset to take the model's default: local servers (LM Studio,
llama.cpp) keep `all`, so the replayed prefix stays identical and their KV
cache keeps serving. Priced providers keep `wholeTurn`, since replayed thinking
is billed input every request. A compat block can set this per model with
`"retainDefault"`.

Two safeguards run regardless of the policy, because a request that breaks is
worse than one missing thought:

- a provider that requires its own reasoning back upgrades `none` and
  `lastTurn` to `wholeTurn`
- thinking a provider cannot accept on replay (carried over from another
  provider, or missing its signature) drops rather than poisoning the request,
  which is what lets a transcript resumed under a different model work

## Showing thinking

Thinking streams in the terminal shaded so it does not read as a reply. It is
shown by default: set `"reasoning": {"hide": true}` to keep it out of view, or
flip **Show thinking** in `/settings` for the session. Streaming behavior and
layout are covered in `tui.md`.

## Choosing a model at startup

A fresh session resolves its model this way, first match wins:

1. `-m/--model <key>` (the flag layer)
2. config.json's `model` key, where `/model` persists your most recent choice
3. models.json's `defaultModel`
4. the first known model

A name that resolves to nothing warns and falls back instead of failing the
run. The picker shows each row as key, display name, context window and a
`reasoning` mark when the model thinks.

## See also

- `cli.md` - install, flags, the first-run provider wizard
- `config.md` - every config.json key, its layer, and `AJENT_*` bindings
- `commands.md` - `/model`, `/reasoning`, `/settings`, `/reload` behavior
- `permissions.md` - what read-only modes run without approval
- `agents.md` - running sub-agents and plan phases on a different model
