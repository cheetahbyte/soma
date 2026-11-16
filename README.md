# Soma

Soma extracts axon's model access into `github.com/cheetahbyte/soma`, a Go library with no dependencies outside the standard library.

Implementation is in progress. Local providers and basic HTTP streaming are available as libraries. Blob transfer, partial replies on abort, session recovery, configuration reload, and the `soma` command aren't implemented yet.

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

Streaming replies preserve deltas, messages, usage, and provider error kinds. The client treats quiet or incomplete streams as network errors. Blob uploads and abort recovery require later implementation tasks.

## Checks

Go 1.27 or later is required. Run formatting checks, vet, build, and the local unit tests:

```sh
./scripts/check.sh
```

See the [design](docs/designs/soma.md) and [implementation plan](docs/plans/2026-11-16-soma.md) for the server and axon integration work.
