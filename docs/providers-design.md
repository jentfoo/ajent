# Provider Design

How `pkg/llm` works, why it is shaped this way, and the rules you must not break
when changing it. `pkg/config` is covered here too, since it currently serves
the path and byte handling this layer needs.

## What it is

One streaming interface over five vendors — anthropic, openai, openrouter,
llama.cpp and lm-studio — so nothing above `pkg/llm` knows which one is
answering. Differences that cannot be normalised (reasoning encoding, cache
control, tokenizer availability) are declared as *capabilities* rather than
leaking upward as special cases.

No provider SDKs. HTTP, JSON and SSE are stdlib; the only third-party import is
`go-analyze/bulk` for collection helpers. Five vendors with one SDK each is five
dependency trees and five event models to normalise anyway, and four of the five
speak either OpenAI-compatible JSON or Anthropic Messages JSON over SSE. The
cost of that decision is owning schema drift, which is why every wire struct
decodes into named fields with the unknown ones ignored, and why there are
recorded response fixtures per provider.

## Layers

```
pkg/config/       paths and bytes only, no domain types
  dir.go            ~/.ajent resolution, AJENT_HOME override
  file.go           0600 checks, atomic write
  merge.go          generic deep JSON merge
  unknown.go        reflective unknown-key detection for warnings
  json.go           comment and trailing comma tolerance, located syntax errors,
                    duplicate key detection

pkg/llm/
  types.go          content model, BlockList and its type tagged JSON
  request.go        Request, Provider, Counter, Discoverer, Level, RetainPolicy
  caps.go           Capabilities, TokenizerKind
  event.go          Event, EventType, StopReason, Usage
  stream.go         Stream, Accumulator, Accumulate, SliceStream
  fake.go           ScriptedProvider, for callers that need a provider in tests
  model.go          Model
  errors.go         APIError, sentinels
  enum.go           text encoding shared by every enum
  duration.go       Duration, the config duration form

  sse.go            dialect free frame parser, shared with any SSE consumer
  httpclient.go     timeouts, retry loop, idle reader, redaction, key resolution
  retry.go          backoff, Retry-After, retryable classification

  config.go         models.json schema and loading
  defaults.go       per flavor defaults and the capability merge
  registry.go       Model list, Resolve, Refresh, declared-wins merge
  discover.go       discovery cache, conditional refetch, orchestration
  factory.go        ProviderConfig -> Provider

  prepare.go        Prepare (message normalization)
  retention.go      retention policy helpers
  think.go          inline <think> splitting
  toolacc.go        tool argument accumulation
  classify.go       per flavor error and overflow classification

  anthropic.go      Messages API
  openai.go         Responses API, chat-completions fallback
  openaicompat.go   the shared chat-completions dialect
  openrouter.go     profile over openaicompat
  llamacpp.go       profile over openaicompat, /props, /tokenize
  lmstudio.go       profile over openaicompat, /api/v0/models
  *_wire.go         JSON structs only, no logic
```

Everything above the adapter files is vendor agnostic. Three of the five
providers are a profile over `openaicompat.go` plus a discovery parser; adding a
sixth of that shape is roughly a hundred lines.

The rule `pkg/config ↛ pkg/llm` (see `config-design.md`) is why `models.json`
decodes in `pkg/llm/config.go` rather than in `pkg/config`.

## The content model

```go
type Message struct {
    Role    Role
    Content BlockList
}

type Block interface{ blockType() BlockType }   // sealed
```

`TextBlock`, `ThinkingBlock`, `ToolCallBlock`, `ToolResultBlock`, `ImageBlock`.

Blocks are stored as **values, not pointers**, so a block is immutable once
appended. That is what makes `Prepare` safe to write as a filter.

`BlockList` carries the JSON round trip. A bare `[]Block` cannot be decoded — the
concrete type is lost — so `BlockList.MarshalJSON` writes a `{type, data}`
envelope per block and `UnmarshalJSON` reads it back. `Message.Content` and
`ToolResultBlock.Content` are both `BlockList`, so the session log can round-trip
a transcript directly.

`ThinkingBlock` deliberately carries every provider's replay token at once:

| Field | Provider | Without it |
|---|---|---|
| `Signature` | anthropic | the block is rejected on replay |
| `Redacted` | anthropic | the redacted payload cannot be sent back |
| `ItemID` | openai responses | the reasoning item cannot be referenced |
| `Encrypted` | openai responses | a stateless replay is rejected |
| `Details` | openrouter | signatures are lost when routed to anthropic |

`Redacted` is a `string` holding the base64 exactly as received, not `[]byte`.
Decoding and re-encoding risks a non-byte-exact round trip, and anthropic
tolerates a redacted block's absence but not its corruption.

## Life of a request

```
Request
  -> Prepare(req)                                one normalization pass (see below)
  -> build<Dialect>Body(req)                     pure, no network; calls Prepare itself
  -> httpClient.do(ctx, httpReq{...})            all retries happen here
  -> <dialect>Stream over SSEReader              synchronous pull
  -> Event...                                    normalised, same for every vendor
  -> Accumulator                                 rebuilds the assistant Message
```

Body construction is a pure function of the request, which is why request shape
can be asserted in tests without a server.

### Normalization (`Prepare`)

Every request path — each `build*Body`, the token estimator, and llamacpp's exact
counter — passes through one entry point so what is counted is exactly what is sent:

- **Image downgrade** when the model cannot read images: an image becomes a text
  placeholder (`(image omitted: ...)`) instead of failing the request. Consecutive
  placeholders collapse to one; assistant content is untouched.
- **Cross-model degradation** keyed on each message's `Origin` (provider + dialect +
  model, stamped at append and rebuild, never written to the transcript). A message whose
  origin differs from the target is untrusted: redacted thinking is dropped, other foreign
  thinking flattens to plain text, responses signatures are stripped, and tool-call ids are
  normalized with their matching results rewritten in step. Unknown provenance counts as
  foreign.
- **Retention** (below) merged into the same pass so a `none` policy can still strip a
  block that degradation did not already turn to text.
- **Orphan repair**: an unanswered tool call gets a synthetic error result before any later
  turn or at the end of the list; assistant turns whose stop reason is `error` are skipped,
  and their tool results dropped with them. This makes well-formedness a request-build
  invariant rather than just an abort-time repair.

A placeholder ladder gives empty or image-only tool results something to say: `(no tool
output)`, or `(see attached image)` when an image survives. Some chat-completions providers
require the reasoning-as-text shape, a tool-result name, an assistant reply between result
turns and the next user message, or explicit `strict: false` on tools — all driven by their
capability gates.

**Traps**: adapters never call the retention helpers directly; they go through `Prepare`,
which must stay idempotent (the Responses fallback builds the same body twice in a session).
A provider that genuinely never sends a chat-completions finish reason must declare
`supportsFinishReason: false`, or an otherwise clean stream is reported as truncated.

## The event stream

`Event` is a struct with a type tag, not an interface. The repo already makes
this call twice — `histLine` and `key` in `pkg/tui` are tagged structs — and
events are transient, share `(Index, Text)` in most cases, and go through a hot
loop. An interface would force a heap allocation per event and a type switch
with a binding per arm at every consumer.

The order a well behaved provider produces:

```
message_start                        model id, request id
thinking_start / delta / end         end carries the replay tokens
text_start / delta / end
tool_call_start                      id and name known
tool_call_delta                      partial JSON arguments
tool_call_end                        complete, validated input
usage                                may arrive more than once, last wins
done                                 StopReason, or Err when the turn failed
```

`Index` is the content block index, pairing a start with its deltas and its end.
`Accumulate(Stream)` builds the final `Message` from the end events, falling back
to concatenating deltas for a block whose end never arrived, so an aborted stream
still yields what was received.

`ScriptedProvider` and `SliceStream` are exported for exactly this reason: the
agent loop's tests and compaction all need a fake provider, and several packages
each inventing their own would drift.

## Capabilities

`Capabilities` is the fully resolved quirk set for **one model**, not one
provider. That distinction matters: a single lm-studio endpoint serves one model
that takes a reasoning effort and another that does not, and a provider-level
answer cannot express it. So capabilities live on `Model.Caps`, and the adapter
reads `req.Model.Caps`.

Resolution is four layers, field by field, later winning. The detection layer
sits between the flavor defaults and configured compat: it derives the quirks pi
auto-detects from a chat-completions provider's name and base URL (never a model
id except openrouter's `anthropic/` / `openai/` prefixes), so an entry carrying
only a name and endpoint resolves like pi without any explicit config.

```
flavorDefaults[flavor].caps  ->  detected(name, baseURL)  ->  ProviderConfig.Compat  ->  ModelConfig.Compat
```

Detection returns a sparse `Compat` (a zero one when no vendor family matches)
and never sets `Reasoning`, which comes from the model entry. It runs only for
chat-completions; anthropic and responses providers are not detected.

Every `Compat` field is a pointer so "unset" is distinguishable from "explicitly
false". Without that tri-state, `"supportsTemperature": false` is
indistinguishable from not mentioning it, and the default silently wins.

`Timeouts` fields are `*Duration` for the same reason: nil takes the dialect
default, an explicit `"0s"` disables the bound. A plain zero cannot say
"disabled", which is precisely what an lm-studio endpoint needs.

## Reasoning

The feature with the least agreement between vendors, so it gets the most
machinery.

**Levels** are the standard seven — `off, minimal, low, medium, high, xhigh,
max` — so a `thinkingLevelMap` written against them maps every key. A model may
translate them with `Capabilities.LevelMap`; a `null` entry omits the parameter
entirely for that level.

**Thinking formats** decide how reasoning is encoded and parsed, carried as
`Capabilities.Thinking`. The canonical values are pi's eleven (`openai`,
`openrouter`, `deepseek`, `together`, `baseten`, `zai`, `qwen`, `chat-template`,
`qwen-chat-template`, `string-thinking`, `ant-ling`) plus `none`, and two ajent
extensions: `anthropic` (the Messages budget shape) and `think-tags` (no request
parameter; reasoning is parsed back out of content with inline tags). Response
parsing for the tag formats uses `ThinkOpen` / `ThinkClose`. An unknown value in
configuration warns rather than defaulting silently.

**Reasoning replay round-trips to its source field.** On chat-completions ingest,
the first non-empty of `reasoning_content` → `reasoning` → `reasoning_text` wins
(pi's order, since some providers echo the same text into two fields), and that
delta name is recorded on the thinking block (`ThinkingBlock.Field`, inline-tag
blocks leave it empty). On replay every surviving non-blank thinking block joins
with `"\n"` (pi) and is written back under `Capabilities.ReasoningField` when
set, else the first block's own `Field`. That resolves `ReasoningContentField` —
which detection sets to `reasoning_content` for opencode-go, reproducing pi's
hardcoded remap through configuration. A compat-dialect block with a non-empty
`Field` is replayable regardless of policy.

**Tool-result images split out on chat-completions.** When the model accepts
images (`DialectOpenAICompletions && caps.Images`), `Prepare` moves image blocks
after the placeholder ladder runs — so the result keeps a `(see attached image)`
text part and the following user message carries them as text + `image_url` parts,
optionally preceded by an assistant bridge when
`requiresAssistantAfterToolResult`. Anthropic and Responses keep images inside the
tool result, which is what pi does there.

**Retention** is what the user configures, applied at request build time only.
The transcript always holds everything.

- `none` — strip entirely.
- `lastTurn` — keep the most recent assistant message.
- `wholeTurn` — keep the current tool calling turn, strip completed turns. The
  configured default.
- `all` — never strip for policy reasons.

Two rules make it survive contact with real providers:

1. A provider that **requires replay** upgrades `none` and `lastTurn` to
   `wholeTurn`. Stripping thinking from the current tool-use turn makes the
   request invalid, and a turn may contain several assistant messages, so
   `lastTurn` is not enough either.
2. A block carrying no token the provider accepts is **dropped whatever the
   policy says**, including under `all`. It cannot be sent, so keeping it only
   guarantees a rejected request. This is what stops a transcript resumed under a
   different provider from failing on its first request.

A turn boundary is a `RoleUser` message containing at least one block that is not
a `ToolResultBlock`. Anthropic delivers tool results as user-role messages, so a
naive "last user message" makes every tool round trip look like a new turn and
collapses `wholeTurn` into `lastTurn`.

An assistant message emptied by stripping is dropped entirely — an empty
assistant message is rejected by anthropic and confuses local chat templates —
unless it still holds tool calls.

## Configuration

`~/.ajent/models.json`, kept in the familiar shape so an existing configuration
ports over with only the `cost` blocks removed:

```json
{
  "providers": {
    "lmstudio": {
      "baseUrl": "http://192.168.1.100:1111/v1",
      "api": "openai-completions",
      "apiKey": "lmstudio",
      "timeouts": { "idle": "0s" },
      "models": [
        {
          "id": "deepseek-v4-flash-0731",
          "name": "DeepSeek V4 Flash",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 400000,
          "maxTokens": 40000,
          "compat": {
            "thinkingFormat": "deepseek",
            "maxTokensField": "max_tokens",
            "requiresReasoningContentOnAssistantMessages": true
          },
          "thinkingLevelMap": { "off": "none", "xhigh": "high", "max": "high" }
        }
      ]
    },
    "openrouter": { "apiKeyEnv": "OPENROUTER_API_KEY" }
  }
}
```

Deliberate choices in this loader:

- **`api` and `flavor` are separate.** `api` is the wire dialect —
  `anthropic-messages` (canonical; legacy `anthropic` still loads through an alias) |
  `openai-responses` | `openai-completions`; `flavor` selects
  discovery and quirk defaults. `flavor` defaults to the provider key when that
  names a known one, so `"lmstudio"` needs no `flavor` field, but an
  OpenAI-compatible proxy in front of a known server can say
  `{"flavor": "lmstudio"}` and still get the right defaults.
- **Lenient syntax.** `//` line comments and trailing commas are accepted
  because a config you cannot paste in is not a config you can use. Comments
  and commas are blanked rather than deleted, so byte offsets
  still refer to the original file and a genuine syntax error reports the line,
  column and the text it is on.
- **Duplicate keys warn.** `encoding/json` keeps the last silently, so a repeated
  key is a setting that looks applied and is not. Hand-written configs do this:
  `thinkingFormat` appears twice in one `compat` block.
- **`reasoning` is boolean-only** and matches pi: `true` enables reasoning with
  the model's resolved thinking format; there is no style-name form.
- **Unrecognised keys warn rather than fail.** A typo silently ignored is worse
  than a warning, and a hard failure locks the user out of their agent.
- **No `cost` block.** See "Deliberately not done".

`models.json` is user scope only, because it may hold a literal `apiKey`. The
loader warns when such a file is group or world readable. Key resolution order
is the configured env var, then the literal, then the dialect's conventional
variable, then an error naming what to set.

## Registry and discovery

Nothing about models is compiled in except per-flavor endpoint and capability
defaults. Models come from `models.json` and from asking the provider.

`Registry.Resolve` matches, in order: alias, `provider/id`, bare id, unique id
suffix, unique key substring, unique name substring. An ambiguous name is an
error listing the candidates, never a coin flip.

**The merge rule:** when a provider declares models, that list *is* the list for
that provider. Discovery may fill fields the declaration left unset but never
adds or removes an entry. Discovery supplies the whole list only for a provider
that declares nothing. This is what lets you name three lm-studio models you
actually use without the picker filling with everything the server has, while
still learning the real loaded context length of the ones you named.

Discovery endpoints: openrouter `GET /models`, lm-studio `GET /api/v0/models`,
llama.cpp `GET /props`. The local ones report the context length the model was
*loaded* with, which is often smaller than its maximum and which nothing else
can know.

Refetch is conditional on `ETag` / `Last-Modified`; a `304` keeps the models and
only bumps the check time. Results cache to `~/.ajent/models-cache.json` at
`0600`. Time to live is 24h for hosted catalogues and one minute for local
servers, whose loaded model changes far more often.

Discovery **never blocks startup and is never fatal**: `NewRegistry` is cache
only, `Registry.Refresh` runs in the background, and a provider that fails keeps
whatever was cached. Starting offline with a populated cache works; starting
offline with no cache still resolves declared models.

## Errors, retry and overflow

```go
type APIError struct {
    Provider   string
    Status     int
    Code       string
    Message    string
    Retryable  bool
    RetryAfter time.Duration
    Body       []byte
}
```

Retry covers 408, 429, 425, 5xx and connection errors, plus 409 only when the
server sent a `Retry-After` (some gateways use it for "model loading").
Exponential backoff with jitter, honouring `Retry-After` but **capped at 60s** —
beyond that the request fails immediately, because an agent that silently sleeps
for an hour is indistinguishable from a hang.

Every provider signals "too many input tokens" differently, so each flavor has a
phrase table in `classify.go` and maps its form to `ErrContextOverflow`, which
callers catch with `errors.Is`. Note llama.cpp reports it as a **500**, not a
client error. Matching vendor prose is fragile by nature; treat the table as
something that will need additions.

Timeouts are five separate bounds because one `http.Client.Timeout` covers the
body read, which is the one thing that must be allowed to take minutes:

| Bound | Default | Why |
|---|---|---|
| connect | 10s (5s local) | |
| TLS | 10s | |
| header | 60s, **0 for lm-studio and llama.cpp** | a just-in-time model load holds the headers for minutes |
| idle | 5m hosted, **0 local** | gap between reads, not total duration |
| total | disabled | opt-in only |

The idle bound wraps the response body rather than living in the SSE reader, so
a `: ping` comment frame counts as progress even though it dispatches no event.

## Prompt caching

Anthropic gets explicit `cache_control` breakpoints on the last system block, the
last tool definition and the last `KeepLast` message boundaries, recomputed every
request so the cached prefix grows with the conversation. Breakpoints start one
message back, because the newest message is still changing. The API allows four;
the code will not emit more. Models declaring `supportsLongCacheRetention` get
the extended `ttl` tier.

OpenAI and openrouter cache automatically and report it through
`Usage.CacheRead`. Local providers reuse their own KV cache and need nothing
sent, beyond llama.cpp's `cache_prompt`.

## Provider notes

| Provider | Worth knowing |
|---|---|
| anthropic | System is a top-level field, not a message. Tool results ride on **user** messages, never a tool role. Consecutive same-role messages must be merged. Temperature must be dropped when thinking is enabled, or the request 400s. `count_tokens` is exact but billed, so it is a method and never called automatically. |
| openai | Responses API primary. Tools are flat, not nested under a `function` key. `max_output_tokens`, not `max_tokens`. Reasoning items replay by id, and statelessly only with `encrypted_content`, which requires asking for it via `include`. Falls back to chat-completions per model, decided from resolved capabilities rather than sniffed. |
| openrouter | Carries reasoning in a `reasoning` object rather than `reasoning_effort`. `reasoning_details` must be echoed back verbatim or a model routed to anthropic loses its signatures. Pricing fields in the discovery response are dropped. |
| llama.cpp | Older builds reject an unknown `stream_options`, so stream usage starts off and discovery turns it on. `/tokenize` is exact, local and cheap, so it is the one tokenizer used freely. |
| lm-studio | Header and idle bounds default to disabled for JIT loads. Tool support varies per loaded model, so it is a capability. |

## Invariants

These are load bearing. Each exists because breaking it produced, or would
produce, a real bug.

**1. Retry happens only before the first body byte.** `httpClient.do` performs
every attempt and returns only once the status is 2xx and headers are read. The
stream then reads with no retry underneath it. There is no code path that *can*
re-emit deltas, which is what would duplicate them in the transcript. A
mid-stream failure surfaces the partial plus an error for the caller to handle.

**2. A stream is a synchronous pull, not a goroutine and a channel.** Each
adapter's stream holds the response, the SSE reader and a pending queue. Nothing
leaks when a caller abandons a stream, `Close` is closing the body, and
cancellation is the ctx-bound body unblocking the read. The goroutine shape is
what people reach for first and it leaks on every early `Close`.

**3. `Close` abandons whatever is still buffered, and is not an error.** `Next`
checks the closed flag before draining pending events, and `Err` returns nil
after a deliberate close. The agent loop's interrupt depends on both halves.

**4. Thinking blocks keep every provider's replay token, and unreplayable ones
are dropped.** See "The content model" and "Reasoning".

**5. Retention edits the request, never the transcript.** Everything is still on
disk; only what is sent shrinks.

**6. A tool call completes at a structural boundary, not when its JSON parses.**
Partial arguments parse successfully far more often than not — `{"a":1}` is
valid long before `,"b":2` arrives — so completion is decided by a new index
appearing, a finish reason, or the stream ending. Validation happens there.

**7. A malformed tool call fails the call, not the turn.** The end event is still
emitted with `Err` set, so the agent can hand the model a tool error it can
correct. Local models produce broken argument JSON routinely, and losing the
whole turn each time is unusable. No repair heuristics: an argument that
"repairs" into a valid-but-wrong call is worse than a visible failure.

**8. Declared models are the whole list for their provider.** Discovery
enriches, never adds or removes.

**9. Discovery never blocks startup and never fails the run.**

**10. Credentials never reach a log or an error.** Redaction happens inside the
client before the hook is called, so a hook cannot leak by forgetting. Masked:
`Authorization`, `X-Api-Key`, `Api-Key`, `Proxy-Authorization`, `Cookie`,
`Set-Cookie`, `X-Goog-Api-Key`, `Openai-Organization`, the `key`, `api_key` and
`access_token` query parameters, and the truncated error body, which some
providers echo the key into.

**11. `sse.go` knows no dialect.** No JSON, no vendor names, one const for the
`[DONE]` sentinel and a method to report it, with the policy left to the caller.
An MCP transport can reuse it; `openaicompat` breaks on `[DONE]` while MCP
ignores it.

## Testing

Fixtures are **raw wire bytes**, byte-identical to what the vendor sends, under
`pkg/llm/testdata/<provider>/`. No invented fixture dialect, so a recorded
response can be dropped in.

Two replay servers. `sseServer` writes one frame at a time; `sseServerChunked`
writes fixed-size chunks so a write boundary lands inside a JSON escape and
proves the SSE reader reassembles before the decoder sees anything. The chunk
size in those tests is coprime with the frame lengths on purpose.

Golden expectations are Go literals, not files: a field rename becomes a compile
error instead of a silent mismatch, and there is no `-update` flag to fight CI's
`git diff --exit-code`. Request bodies compare with `assert.JSONEq`, since key
order is not part of the contract.

**No test touches the network, and no test sleeps.** Retry asserts the durations
handed to an injected `sleep`; the idle timeout fires through an injected
`afterFunc` whose armed timer is handed to the firing goroutine over a channel,
so the expiry is ordered against the read with no polling.

Scenarios covered per dialect: text only, thinking plus text, single tool call,
parallel tool calls, arguments split mid-token, usage-only final frame,
mid-stream error, close mid-stream, and overflow classification. Plus the
retention matrix (four policies against five capability sets), the `<think>`
splitter table including a tag split across three deltas, and the tool
accumulator including arguments that parse early.

## Traps

- **Do not add a model to `flavorDefaults`.** A stale context window silently
  corrupts the context bar, which is worse than not knowing it. A test asserts
  the table ships no models.
- **Do not mutate `httpClient.headers` for a per-call header.** Discovery runs in
  the background; use `httpReq.headers`, which merges over the client's.
  `anthropicHeaders` is the real caller: it merges the `anthropic-beta` value
  (interleaved thinking, fine-grained tool streaming) over `req.Model.Headers`
  per request without touching either shared map.
- **Anthropic always sends a thinking shape.** Reasoning models emit
  `thinking:{"type":"disabled"}` when the level resolves to off (suppressed only
  by an explicit `off:null` in the level map), the budget shape when on, and the
  adaptive shape (`thinking:{"type":"adaptive"}` plus `output_config.effort`) on
  models that require it. `max_tokens` is inflated by the thinking budget first,
  capped at the model cap, so the reply keeps its full window. `display` is
  deliberately never sent.
- **A `Compat` bool must stay a pointer** and a `Timeouts` duration must stay a
  pointer. Both need the unset/explicit distinction.
- **Enums encode as text, not JSON.** `encoding/json` uses `TextUnmarshaler` for
  map keys, and `thinkingLevelMap` is keyed by `Level`. Implementing only
  `UnmarshalJSON` compiles and then fails at runtime on that map.
- **Anthropic rejects a temperature alongside thinking.** It is dropped silently
  rather than erroring, because erroring would make a `/settings` temperature
  change fail confusingly on exactly the models people reason with.
- **`<think>` extraction must withhold a partial tag.** Emitting `"<"` as text
  and correcting later is visible corruption; the splitter holds back the longest
  suffix that could still become the tag.

## Extending

Adding a provider that speaks chat-completions:

1. Check whether detection (`pkg/llm/detect.go`) already covers the vendor by
   name or base URL — most chat-completions families pi detects are handled
   there and need no flavor at all.
2. If it still needs its own defaults, add a `Flavor` and an entry in
   `flavorDefaults` — base URL, dialect, key variable, capabilities. No models.
4. If it needs request fields nobody else sends, write a `decorate` hook. If it
   needs response fields nobody else reads, write an `extra` hook. Needing a
   third hook is the signal that the thing belongs in the shared layer.
5. If it can list its own models, write a parser and add it to
   `discoverySpecs`.
6. Add its overflow phrases to `overflowPhrases`.
7. Record fixtures and run them through the same `collect` helper every other
   provider uses. That the assertions differ only in content, never in shape, is
   the real proof that normalisation worked.

A genuinely new dialect is a new `Dialect`, a `*_wire.go`, an adapter
implementing `Provider`, and a case in `factory.go`.

## Deliberately not done

- **Pricing.** `Model` has no pricing field, `models.json` has no `cost` block,
  and openrouter's pricing fields are dropped rather than cached. Nothing can go
  quietly stale and be wrong about money. Restoring it would need an explicit
decision before any cost reporting is built.
- **A built-in model catalogue.** Endpoints and quirks only. See traps.
- **Token estimation.** `Capabilities.Tokenizer` declares what each provider can
  do, but no consumer uses it yet.
- **Azure, Bedrock and Vertex.** They are OpenAI- and Anthropic-shaped with
  different auth. `baseUrl`, `headers`, `flavor` and `compat.extraBody` should
  cover most of it without code.
- **Repairing malformed tool arguments.** See invariant 7.
