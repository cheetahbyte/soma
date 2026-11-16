# Soma

Soma extracts axon's model access into `github.com/cheetahbyte/soma`, a Go library with no dependencies outside the standard library.

Implementation is in progress. Local providers, HTTP streaming, blob transfer, partial replies on abort, session recovery, configuration reload, and the `soma` command are available.

## Library

- The root package defines requests, messages, streaming deltas, provider interfaces, and errors.
- `config` reads provider configuration from `models.json` under a caller-supplied root. It also handles model information and caches.
- `auth` handles ChatGPT logins, token refresh, and API-key storage in that root's `auth.json`.
- `provider` holds the registry. Import `provider/openai`, `provider/chat`, or `provider/claudecode` to register their providers.
- `toolserver` serves tools to the installed `claude` program over loopback HTTP.
- `wire` defines the versioned JSON protocol.
- `server` provides an authenticated HTTP handler for streaming replies, provider information, and end-of-run signals.
- Importing `client` registers the remote `soma` provider and its model lister.
- `somatest` contains scripted model, chat, Claude, and login fakes for local tests.

Load configuration with `config.Load(root)`, then build a provider with `provider.New(cfg, cfg.Providers[name])`. `Config.SuggestedDefault()` reports the default model suggested by subscription providers without reading or writing application settings.

## ChatGPT models

A provider with `"api": "codex"` and no `models` takes its models from ChatGPT's model list. `provider.FetchModels` reads it with the stored login, refreshing the token once if ChatGPT rejects it, and the caller caches the result with `config.SaveCatalog`, as for other live model lists. The list is cached for 24 hours, and a failed fetch for 10 minutes.

- Models that ChatGPT hides from its model picker are left out.
- The models keep ChatGPT's priority order. `SuggestedDefault` names the first model of the cached list; without a usable cached list, that provider suggests none.
- `contextWindow` is the model's context window.
- `thinking` is those of `low`, `medium`, `high`, and `xhigh` the model supports. A model that supports `low` also gets `minimal`, sent as `low`. ChatGPT's `max` and `ultra` are left out. A model without reasoning levels gets `reasoning: false`.
- `off` is not offered, because the list doesn't say whether a model accepts the effort `none`. Add it per model with `modelOverrides`:

```json
"modelOverrides": {
  "gpt-6.1-terra": {
    "thinking": ["off", "minimal", "low", "medium", "high", "xhigh"],
    "effort": { "off": "none", "minimal": "low" }
  }
}
```

Until a fetch succeeds, any model id is accepted with the default values: a 128,000-token context window and every thinking level. A `codex` provider with explicit `models` reads no list.

Before using the library, set `soma.Program` and `soma.Version` to identify the embedding program in messages, login pages, and HTTP user agents. They default to `soma` and `dev`. Set them once, before concurrent use.

Soma preserves axon's existing file formats. It doesn't migrate credentials or manage Claude Code's credentials.

## HTTP libraries

Create a handler with `server.New(server.Options{Root: root, Token: token})`, then serve its `Handler()` with Go's HTTP server. Call `Shutdown(ctx)` when stopping the embedding program. The handler requires a bearer token and the `Soma-Protocol` version header on every request.

Configure a remote provider in the asking program's `models.json`:

```json
{
  "providers": {
    "home": {
      "api": "soma",
      "baseUrl": "http://127.0.0.1:7662",
      "apiKey": "$SOMA_TOKEN",
      "upstream": "chatgpt"
    }
  }
}
```

Set `SOMA_TOKEN` to the server's token. `upstream` defaults to the entry's name. Without an explicit model list, call `provider.FetchModels` to discover models and cache the upstream's conversation capability. Explicit model entries retain their overrides while inheriting cached server metadata.

Streaming replies preserve deltas, messages, usage, and provider error kinds. The client treats quiet or incomplete streams as network errors. Abort recovery requires a later implementation task.

## The program

The `soma` command lives in the nested module `cmd/soma`, the only module that requires `tailscale.com`. The repository's `go.work` joins both modules. Build it from the repository root:

```sh
go build ./cmd/soma
```

Commands:

- `soma serve [--listen <addr>] [--hostname <name>]` serves the HTTP API on `--listen` (default `127.0.0.1:7662`). It refuses to start, with exit status 2, when `SOMA_TOKEN` is not set. When `TS_AUTHKEY` is set, it also joins the tailnet as `--hostname` (default `soma`) and serves on port 80 there, keeping the Tailscale state in `<root>/tailscale`. On `SIGTERM` or `SIGINT` it shuts down and exits with status 0.
- `soma login codex` prints the ChatGPT sign-in address and reads back the address the browser was sent to after signing in. It stores the login in the root, then reads and caches the ChatGPT model list. A list it can't read is a warning; the login still succeeds.
- `soma login melious --stdin` reads a Melious API key from the first line of standard input, checks it against Melious, and stores it.
- `soma version` prints the version.

Environment:

- `SOMA_HOME`: the root holding `models.json`, `auth.json`, and the caches. Defaults to `~/.soma`.
- `SOMA_TOKEN`: the bearer token every request must carry.
- `TS_AUTHKEY`: optional Tailscale auth key.

## Checks

Go 1.27 or later is required; `cmd/soma` needs Go 1.27.4 or later, the version `tailscale.com` requires. Run formatting checks, vet, build, and the local unit tests for both modules:

```sh
./scripts/check.sh
```

See the [design](docs/designs/soma.md) and [implementation plan](docs/plans/2026-11-16-soma.md) for the server and axon integration work.
