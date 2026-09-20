# local-llm-pool

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that spreads requests
for one shared model alias across several local LLM servers, filling them in the order you
configure and spilling over when one is busy.

You give a machine a concurrency limit. Requests for the shared alias go to the first
machine until it is at its limit, then to the next. When a request finishes, its slot comes
back. Traffic you send to a machine's own direct alias counts against the same limit, so
the two paths cannot oversubscribe a backend between them.

## How it works

The plugin implements four host capabilities:

| Capability | Use |
| --- | --- |
| `scheduler` | Chooses which credential serves a request for a group alias. |
| `request_interceptor` | Binds a request to the backend that was actually selected. |
| `request_lifecycle_plugin` | Releases the slot when the request finishes. |
| `management_api` | Serves the `/status` resource. |

It also declares `scheduler_across_priorities`, so the host offers candidates from every
priority tier instead of only the highest one. Without that, a lower-priority spill-over
backend would never appear as a candidate.

One request, end to end:

1. **`scheduler.pick`** maps each candidate credential onto a configured backend and
   remembers which backend owns which auth ID. If the requested model is not a group alias
   the pick is left to the host, but the mapping is still recorded so direct traffic keeps
   being counted.
2. For a group alias, the plugin walks that alias's pool in configuration order and takes
   the first backend where `inflight + pending < max_concurrency`. It reserves a *pending*
   slot so concurrent picks racing at the limit cannot oversubscribe the backend.
3. **`request.intercept_after`** reads the selected auth ID and binds the request to its
   backend as in-flight, consuming the reservation. Only a request for a group alias
   consumes one, since only those create one. The request is passed through unchanged.
4. **`request.complete`** releases the slot. It is idempotent and ignores unknown requests.

A reservation that is never claimed, for example because auth selection failed after the
pick, expires after `pending_ttl_ms`.

## Requirements

- CLIProxyAPI **v7.3.7 or newer** (tested against v7.3.9), built with **cgo enabled**. The host
  loads plugins with `dlopen`; a `CGO_ENABLED=0` build cannot load any plugin. Older hosts lack
  the request completion callback (added in v7.2.103) and cross-priority scheduler candidates
  (added in v7.3.7); the plugin refuses to register on a host that cannot release slots.
- The official Docker image `eceasy/cli-proxy-api` works, including with a read-only root
  filesystem and all capabilities dropped. The plugins directory must not be mounted `noexec`.
- Linux, macOS or FreeBSD for the `.so`/`.dylib` loader, or Windows for `.dll`.
- One or more OpenAI-compatible model servers.

## Install from the plugin store

`local-llm-pool` is packaged for the
[CLIProxyAPI plugin store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store). Once
a plugin is listed there, the host reads its repository's latest release, downloads the
archive for its own platform, checks it against `checksums.txt`, and unpacks the library
into its plugin directory. Until this repository is public and listed in the store registry,
use the manual install below.

Enable it in the proxy config:

```yaml
plugins:
  enabled: true
  configs:
    local-llm-pool:
      enabled: true
      priority: 1
      # ... see Plugin configuration below
```

Pin a specific release by adding a `store.version` to that block; leaving it out tracks the
latest release.

### Install manually

Build and drop the library into the host's plugin directory under its platform
subdirectory:

```bash
make build
install -D bin/local-llm-pool.so \
  /path/to/cliproxy/plugins/linux/amd64/local-llm-pool.so
```

The path is `plugins/<goos>/<goarch>/local-llm-pool.so`, for example
`plugins/darwin/arm64/local-llm-pool.dylib` on Apple silicon. The host also looks in
`plugins/` directly. The plugin ID comes from the filename, so keep it named
`local-llm-pool` to match the `plugins.configs.local-llm-pool` key.

## Releases

Every release is tagged `v<version>` with a dotted numeric version, for example `v0.1.0`,
and carries one archive per platform plus a single `checksums.txt`. Asset names use the tag
without its leading `v`:

```
local-llm-pool_<version>_<goos>_<goarch>.zip
checksums.txt
```

| Platform | Asset | Built on |
| --- | --- | --- |
| Linux x86-64 | `local-llm-pool_<version>_linux_amd64.zip` | `ubuntu-24.04` |
| Linux arm64 | `local-llm-pool_<version>_linux_arm64.zip` | `ubuntu-24.04-arm` |
| macOS Apple silicon | `local-llm-pool_<version>_darwin_arm64.zip` | `macos-15` |
| macOS Intel | `local-llm-pool_<version>_darwin_amd64.zip` | `macos-15-intel` |
| Windows x86-64 | `local-llm-pool_<version>_windows_amd64.zip` | `windows-2025` |

Each archive holds **exactly one file at its root**: `local-llm-pool.so`,
`local-llm-pool.dylib` or `local-llm-pool.dll`. No directories and no cgo header.
`checksums.txt` is plain `sha256sum` format, one `<sha256>  <filename>` line per archive.

Because the plugin is a cgo `c-shared` library it cannot be cross-compiled, so each archive
is built natively on a runner of its own architecture. The release build stamps the tag into
the plugin's registration metadata, so a store-installed plugin reports the version it was
released as; a locally built one reports `0.1.0-dev`.

## Proxy configuration

The plugin steers between credentials the proxy already has. Any OpenAI-compatible server
works: vLLM, SGLang, llama.cpp's `llama-server`, Ollama, LM Studio, or anything else that
speaks `/v1/chat/completions`.

The pattern is that **each provider lists its upstream model twice**: once under a direct
alias naming that machine, and once under the shared group alias.

```yaml
openai-compatibility:
  - name: "gpu-box-a"
    base-url: "http://gpu-box-a.internal:8000/v1"
    api-key-entries:
      - api-key: "..."
    models:
      - { name: "model-a", alias: "gpu-box-a" }
      - { name: "model-a", alias: "local-llm-group" }

  - name: "gpu-box-b"
    base-url: "http://gpu-box-b.internal:8000/v1"
    api-key-entries:
      - api-key: "..."
    models:
      - { name: "model-b", alias: "gpu-box-b" }
      - { name: "model-b", alias: "local-llm-group" }
```

`model-a` and `model-b` stand for whatever your servers actually serve, for example the
model id vLLM was started with.

The group alias **resolves per backend**. A request for `local-llm-group` that lands on
`gpu-box-a` is sent upstream as `model-a`; the same request on `gpu-box-b` is sent as
`model-b`. The two machines do not have to serve the same model, though clients will notice
if their behaviour differs a lot.

Each resulting credential carries `compat_name`, `base_url` and `provider_key` as scheduler
attributes. `compat_name` equals the provider's `name`, which is what you normally match on.

## Plugin configuration

### Shorthand: one pool

```yaml
plugins:
  enabled: true
  configs:
    local-llm-pool:
      enabled: true
      priority: 1
      group_aliases: ["local-llm-group"]
      on_saturated: least-loaded
      pending_ttl_ms: 5000
      backends:                          # order is the fill order
        - match: { compat_name: "gpu-box-a" }
          max_concurrency: 8
        - match: { compat_name: "gpu-box-b" }
          max_concurrency: 2
```

### Pools: several independent groups

```yaml
plugins:
  enabled: true
  configs:
    local-llm-pool:
      enabled: true
      priority: 1
      on_saturated: least-loaded         # default every pool inherits
      pending_ttl_ms: 5000
      pools:
        - group_aliases: ["local-llm-group"]
          backends:
            - match: { compat_name: "gpu-box-a" }
              max_concurrency: 8
            - match: { compat_name: "gpu-box-b" }
              max_concurrency: 2

        - group_aliases: ["local-vision-group"]
          on_saturated: reject           # overrides the top-level default
          backends:
            - match: { compat_name: "gpu-box-b" }
              max_concurrency: 2         # same machine, same limit
            - match: { compat_name: "gpu-box-c" }
              max_concurrency: 4
```

`gpu-box-b` appears in both pools. It is one physical machine, so it has **one shared slot
counter**: load that either pool puts on it is visible to the other, and its two entries
must declare the same `max_concurrency`.

### Keys

| Key | Scope | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `group_aliases` | top level *or* pool | yes | — | Model aliases this pool steers. Matched case-insensitively against both the execution model and the client-requested model. |
| `backends` | top level *or* pool | yes | — | Backends in fill order. At least one. |
| `pools` | top level | no | — | Explicit multi-pool form. |
| `on_saturated` | top level and pool | no | `least-loaded` | Behaviour when every backend in the pool is at its limit. A pool inherits the top-level value unless it sets its own. |
| `pending_ttl_ms` | top level only | no | `5000` | Lifetime of an unclaimed pick reservation. Must be greater than zero. |
| `backends[].match` | — | yes | — | Constraints a candidate must satisfy, **all** of them. Keys and values compare case-insensitively. |
| `backends[].max_concurrency` | — | yes | — | Simultaneous requests the backend accepts. Must be greater than zero. |

The shorthand form is exactly equivalent to a single pool. Giving **both** the top-level
`group_aliases`/`backends` and a `pools` list is a configuration error.

### Saturation policies

- **`least-loaded`** picks the backend with the lowest
  `(inflight + pending) / max_concurrency` ratio, breaking ties in configuration order.
- **`first`** picks the first backend in the pool's configuration order.
- **`reject`** fails the pick with a scheduler error carrying `http_status: 429`.

### Match keys

`match` normally reads the candidate's scheduler attributes, most usefully:

| Attribute | Value |
| --- | --- |
| `compat_name` | The `openai-compatibility` provider `name`. |
| `base_url` | The upstream base URL configured for that provider. |
| `provider_key` | The provider key the host resolved. |

Two keys are **reserved** and read the candidate's own fields instead:

| Reserved key | Source |
| --- | --- |
| `provider` | The auth provider name. |
| `auth_id` | The auth record ID. |

An attribute literally named `provider` or `auth_id` is **shadowed** by these: the
candidate field always wins.

```yaml
backends:
  - match: { provider: "openai-compatibility", compat_name: "gpu-box-a" }
    max_concurrency: 8
  - match: { auth_id: "my-specific-credential" }
    max_concurrency: 2
```

The host strips sensitive attributes such as API keys, tokens and proxy URLs before handing
candidates to a scheduler plugin, so they are not matchable.

### Validation

An invalid configuration fails `plugin.register` and `plugin.reconfigure` with an
`invalid_config` error, and the previous configuration stays in effect. Rejected cases
include: both configuration forms at once, an alias claimed by two pools, the same backend
declared with two different `max_concurrency` values, a duplicate backend within one pool,
an empty `match`, and a non-positive `max_concurrency` or `pending_ttl_ms`.

A successful reconfigure preserves the live in-flight and pending counts of every backend
whose `match` set is unchanged, so limits can be retuned without dropping running requests.

Backends are named in `/status` after their match: a single-key match uses the bare value
(`gpu-box-a`), and any collision falls back to `key=value` form for every backend.

## Status endpoint

```
GET /v0/resource/plugins/local-llm-pool/status
```

This is a browser-navigable plugin resource, not a management API route, so the host does
not require management authentication for it.

```json
{
  "pools": [
    {
      "name": "local-llm-group",
      "group_aliases": ["local-llm-group"],
      "on_saturated": "least-loaded",
      "backends": ["gpu-box-a", "gpu-box-b"]
    },
    {
      "name": "local-vision-group",
      "group_aliases": ["local-vision-group"],
      "on_saturated": "reject",
      "backends": ["gpu-box-b", "gpu-box-c"]
    }
  ],
  "backends": [
    {
      "name": "gpu-box-a",
      "match": { "compat_name": "gpu-box-a" },
      "max_concurrency": 8,
      "inflight": 3,
      "pending": 1,
      "available": 4
    },
    {
      "name": "gpu-box-b",
      "match": { "compat_name": "gpu-box-b" },
      "max_concurrency": 2,
      "inflight": 2,
      "pending": 0,
      "available": 0
    },
    {
      "name": "gpu-box-c",
      "match": { "compat_name": "gpu-box-c" },
      "max_concurrency": 4,
      "inflight": 0,
      "pending": 0,
      "available": 4
    }
  ],
  "totals": {
    "max_concurrency": 14,
    "inflight": 5,
    "pending": 1,
    "available": 8,
    "tracked_requests": 5
  },
  "config": {
    "group_aliases": ["local-llm-group", "local-vision-group"],
    "on_saturated": "least-loaded",
    "pending_ttl_ms": 5000
  }
}
```

`backends` lists each physical backend once, even when several pools use it. `pools` shows
the routing; `config.group_aliases` is the union across pools and `config.on_saturated` is
the top-level default.

## Behaviour notes

- **Responses name the backend's model, not the group alias.** A request for
  `local-llm-group` comes back with the upstream model of whichever backend served it, for
  example `model-a`. Clients that log or branch on the response model will see it change
  from request to request as the pool fills.
- **Direct-alias traffic counts but is never steered or rejected.** A request for
  `gpu-box-a` goes to `gpu-box-a`, whatever the load. The plugin counts it against that
  backend's limit so group traffic sees the real load, which means `inflight` can exceed
  `max_concurrency`. The saturation policy then applies to group traffic only.
- **Dead-backend retry is the host's job.** The plugin picks a credential; if that upstream
  is down, CLIProxyAPI's own retry moves the request to another credential and the plugin
  rebinds the slot. The plugin does not health-check backends.
- **Counters are per proxy process.** State lives in the loaded library. Two CLIProxyAPI
  processes in front of the same machines keep separate counters and will together exceed
  `max_concurrency`. There is no cluster-wide coordination. A restart resets every counter.
- **Only one scheduler plugin is active at a time.** The host uses the first enabled,
  non-fused plugin that declares the `scheduler` capability, ordered by plugin priority.
  Running this alongside another scheduler plugin means one of them is silently ignored.
- **Plugins are trusted in-process code.** A CLIProxyAPI plugin is a native library loaded
  into the proxy with `dlopen`. It runs with the proxy's full privileges and there is no
  sandbox. Install only plugins you trust, and read the source if you can. This one makes
  no network calls, writes no files, and logs nothing.
- **Slots are released by `request.complete`.** If the host never delivers a completion for
  a bound request, its slot stays held until the process restarts. Unclaimed pick
  reservations, unlike bound slots, do time out.
- **The status resource is unauthenticated.** It exposes backend names, match attributes
  and concurrency counts, never credentials.

## Worked example

Two machines behind one proxy:

- **`gpu-box-a`** — a big GPU box running a large model on vLLM. Comfortable with 8
  concurrent requests.
- **`gpu-box-b`** — a smaller box running a faster model. Only 2 concurrent requests before
  latency suffers.

You want one alias, `local-llm-group`, that fills the big box first and only overflows to
the small one, while still being able to address either machine by name.

```yaml
openai-compatibility:
  - name: "gpu-box-a"
    base-url: "http://gpu-box-a.internal:8000/v1"
    api-key-entries:
      - api-key: "..."
    models:
      - { name: "model-a", alias: "gpu-box-a" }
      - { name: "model-a", alias: "local-llm-group" }

  - name: "gpu-box-b"
    base-url: "http://gpu-box-b.internal:30000/v1"
    api-key-entries:
      - api-key: "..."
    models:
      - { name: "model-b", alias: "gpu-box-b" }
      - { name: "model-b", alias: "local-llm-group" }

plugins:
  enabled: true
  configs:
    local-llm-pool:
      enabled: true
      priority: 1
      group_aliases: ["local-llm-group"]
      on_saturated: least-loaded
      pending_ttl_ms: 5000
      backends:
        - match: { compat_name: "gpu-box-a" }
          max_concurrency: 8
        - match: { compat_name: "gpu-box-b" }
          max_concurrency: 2
```

What you get:

- Requests 1 to 8 for `local-llm-group` go to `gpu-box-a`.
- Requests 9 and 10 go to `gpu-box-b`.
- Request 11 finds everything full. Under `least-loaded` it goes to whichever box is
  proportionally least busy, breaking the tie toward `gpu-box-a`. Set `on_saturated: reject`
  instead if you would rather the client get a 429 and back off.
- A request sent directly to `gpu-box-b` occupies one of its two slots, so group traffic
  spills over one request sooner.
- `GET /v0/resource/plugins/local-llm-pool/status` shows the live picture.

## Client setup

These examples point three coding clients at the group alias through CLIProxyAPI. Replace
`https://your-proxy.example` with your proxy and `YOUR_PROXY_API_KEY` with a key the proxy
accepts.

Each client uses a different proxy endpoint. CLIProxyAPI translates between them, so the
local backend only ever sees chat-completions:

| Client | Proxy endpoint |
| --- | --- |
| pi | `POST /v1/chat/completions` |
| Claude Code | `POST /v1/messages` |
| Codex CLI | `POST /v1/responses` |

**Tool calling and reasoning quality depend on the local model you are serving, not on this
plugin.** A small local model driving an agentic coding client will make more tool-call
mistakes than a frontier model, however well the routing works.

### pi

Add a provider to `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "cli-proxy": {
      "name": "CLI Proxy",
      "baseUrl": "https://your-proxy.example/v1",
      "api": "openai-completions",
      "apiKey": "YOUR_PROXY_API_KEY",
      "authHeader": true,
      "compat": {
        "supportsDeveloperRole": false,
        "supportsReasoningEffort": true,
        "supportsUsageInStreaming": true,
        "supportsFinishReason": true,
        "maxTokensField": "max_tokens",
        "supportsStrictMode": false,
        "supportsLongCacheRetention": false
      },
      "models": [
        {
          "id": "local-llm-group",
          "name": "Local LLM group (CLI Proxy)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 262144,
          "maxTokens": 32768,
          "cost": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0 }
        }
      ]
    }
  }
}
```

Notes:

- `baseUrl` must end in `/v1`. `"api": "openai-completions"` selects the chat-completions
  wire format, and `"authHeader": true` sends the key as an `Authorization` header.
- The compat flags matter for local OpenAI-compatible servers.
  `"supportsDeveloperRole": false` keeps pi from sending the OpenAI `developer` role, which
  most local servers reject, and `"maxTokensField": "max_tokens"` sends the older field
  name rather than `max_completion_tokens`. Drop `"supportsReasoningEffort"` to `false` if
  your server rejects a `reasoning_effort` field.
- If your model emits reasoning in a family-specific format, add the optional per-model
  `thinkingFormat` so pi knows how to read it. It takes the format name for your model
  family; omit the key entirely when the default handling already works:

  ```json
  "compat": { "thinkingFormat": "<your-model-family>-chat-template" }
  ```

- `contextWindow` and `maxTokens` should reflect what you actually started the server with.
  Cost is zero for a local model.
- Add the direct aliases as extra entries in the same `models` array to address one machine
  on purpose:

```json
{ "id": "gpu-box-a", "name": "GPU box A (CLI Proxy)", "reasoning": true, "input": ["text"],
  "contextWindow": 262144, "maxTokens": 32768,
  "cost": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0 } }
```

### Claude Code

Claude Code talks to the proxy's **Anthropic** endpoint, `POST /v1/messages`. CLIProxyAPI
translates the Anthropic request into chat-completions for the local backend.

`ANTHROPIC_BASE_URL` is the proxy **root**, without `/v1`; Claude Code appends the path.

```bash
export ANTHROPIC_BASE_URL="https://your-proxy.example"
export ANTHROPIC_AUTH_TOKEN="YOUR_PROXY_API_KEY"
export ANTHROPIC_MODEL="local-llm-group"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="local-llm-group"
claude
```

Or persist it in `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://your-proxy.example",
    "ANTHROPIC_AUTH_TOKEN": "YOUR_PROXY_API_KEY",
    "ANTHROPIC_MODEL": "local-llm-group",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "local-llm-group"
  }
}
```

Notes:

- `ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer <value>`.
- `ANTHROPIC_DEFAULT_HAIKU_MODEL` is the small/fast model used for background work such as
  conversation titles. Setting it keeps those calls on the proxy too, instead of falling
  back to the Anthropic API. `ANTHROPIC_SMALL_FAST_MODEL` is the older name for this and is
  **deprecated**; prefer `ANTHROPIC_DEFAULT_HAIKU_MODEL`.
- Set `ANTHROPIC_MODEL` to a direct alias such as `gpu-box-a` to pin one machine.
- Claude Code disables some first-party features when `ANTHROPIC_BASE_URL` points away from
  `api.anthropic.com`, including Remote Control and MCP tool search.

### Codex CLI

Codex talks to the proxy's **OpenAI Responses** endpoint, `POST /v1/responses`.

In `~/.codex/config.toml`:

```toml
model = "local-llm-group"
model_provider = "cliproxy"

[model_providers.cliproxy]
name = "CLIProxyAPI"
base_url = "https://your-proxy.example/v1"
env_key = "CLIPROXY_API_KEY"
wire_api = "responses"
```

Then export the key named by `env_key` and run `codex`:

```bash
export CLIPROXY_API_KEY="YOUR_PROXY_API_KEY"
codex
```

Notes:

- `base_url` ends in `/v1`; Codex appends `/responses`.
- `env_key` names an **environment variable**, not the key itself. Codex reads the key from
  that variable at startup.
- `wire_api = "responses"` selects the Responses API. Use `"chat"` only if you point Codex
  at `/v1/chat/completions` instead.
- To keep this as one option among several rather than the default, put the two model lines
  in a **profile file** at `~/.codex/<name>.config.toml` and select it with `-p <name>`:

```toml
# ~/.codex/local-llm.config.toml
model = "local-llm-group"
model_provider = "cliproxy"
```

```bash
codex -p local-llm
```

  On codex-cli 0.153.4, `-p/--profile` layers `$CODEX_HOME/<name>.config.toml` over the base
  config. Older Codex releases used `[profiles.<name>]` tables inside `config.toml` instead;
  that form is not selectable on 0.153.4.

### Client config keys

The three client configurations above were checked against the tools themselves rather than
written from memory: the pi keys come from a working `~/.pi/agent/models.json` provider
pointing at a CLIProxyAPI instance; every `ANTHROPIC_*` name was confirmed in Claude Code
2.1.278 and against its
[environment variables reference](https://code.claude.com/docs/en/env-vars), which is also
where the `ANTHROPIC_SMALL_FAST_MODEL` deprecation is stated; and the Codex `config.toml`
was loaded with `codex doctor` on codex-cli 0.153.4, which reported `config loaded` and
resolved `model local-llm-group · cliproxy`.

## Verified behaviour

The following was exercised end to end against a real CLIProxyAPI host, built with cgo from
v7 `main`, with fake OpenAI-compatible backends standing in for the model servers:

**Routing and accounting**

- Fill order and limits are honoured: requests land on the first backend until it reaches
  `max_concurrency`, then on the next.
- Direct-alias traffic is counted against the same backend limit, so it reduces the room
  left for group traffic.
- A dead backend is served by the next backend in the pool, through the host's own retry.
- Two pools sharing one backend share its counter: load put on it through one pool is
  visible to the other.
- `on_saturated: reject` returns HTTP 429 to the client once the pool is full.

**Interaction with provider priority**

- Inside a pool, the configured fill order wins over the providers' own `priority` values.
- Models that are not in any pool still honour provider `priority` as usual, because the
  plugin leaves those picks to the host.

**Slot lifetime**

- Streaming requests hold their slot until the stream ends, not just until the first chunk.
- A client disconnect releases the slot and aborts the upstream request, for both streaming
  and non-streaming requests.

**Protocols**

- `/v1/chat/completions`, `/v1/messages` and `/v1/responses` all work through the group
  alias, streaming and non-streaming.

**Not covered by that run:** real pi, Claude Code and Codex sessions against a real model
server. The client configurations above are verified as configuration, and the proxy paths
they use are verified, but no agent was driven end to end through a live local model as part
of this.

## Contributing

Issues and pull requests are welcome. Please open an issue first for anything beyond an
obvious bug fix, since the config keys and the `/status` payload are contracts other people
depend on. [CONTRIBUTING.md](CONTRIBUTING.md) has the build commands, the architecture rules
and the PR checklist.

## Security

A CLIProxyAPI plugin is a native library loaded into the proxy process, with no sandbox, so
install only plugins you trust. Report vulnerabilities privately through this repository's
[Security tab](https://github.com/rhicnl/cpa-plugin-local-llm-pool/security), not
as a public issue. See [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).

## Development

```bash
make build            # -> bin/local-llm-pool.{so,dylib,dll}
make test             # go test -race -count=1 ./...
make vet              # go vet ./...
make package          # -> dist/local-llm-pool_<version>_<goos>_<goarch>.zip + checksums.txt
make verify-package   # re-open the archive and assert the store's layout rules
make clean
```

`make package` and `make verify-package` run the same code the release workflow runs, in
`internal/pkgzip`, so the archive layout can be checked without cutting a release. Pass
`VERSION=1.2.3` to either to exercise a release-shaped name.

The build needs cgo and a C toolchain. All routing logic lives in `internal/tracker` as
pure Go with an injectable clock; `main.go` is the cgo ABI shim and `dispatch.go` is the
JSON envelope dispatch.

CI runs gofmt, `go vet`, the race test suite and a packaging round trip on every push and
pull request. The release workflow builds all five platforms natively on tag push, and can
be run manually as a dry run that builds and uploads the archives as workflow artifacts
without creating a release.

The module depends on `github.com/router-for-me/CLIProxyAPI/v7` only for `sdk/pluginabi`
and `sdk/pluginapi`, which are stdlib-only packages.
