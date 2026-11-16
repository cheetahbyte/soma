# Soma

Soma extracts axon's model access into `github.com/cheetahbyte/soma`, a Go library with no dependencies outside the standard library.

Implementation is in progress. The local library is available; the HTTP server, remote client, and `soma` command aren't implemented yet.

## Library

- The root package defines requests, messages, streaming deltas, provider interfaces, and errors.
- `config` reads provider configuration from `models.json` under a caller-supplied root. It also handles model information and caches.
- `auth` handles ChatGPT logins, token refresh, and API-key storage in that root's `auth.json`.
- `provider` holds the registry. Import `provider/openai`, `provider/chat`, or `provider/claudecode` to register their providers.
- `toolserver` serves tools to the installed `claude` program over loopback HTTP.
- `somatest` contains scripted model, chat, Claude, and login fakes for local tests.

Load configuration with `config.Load(root)`, then build a provider with `provider.New(cfg, cfg.Providers[name])`. `Config.SuggestedDefault()` reports the default model suggested by subscription providers without reading or writing application settings.

Before using the library, set `soma.Program` and `soma.Version` to identify the embedding program in messages, login pages, and HTTP user agents. They default to `soma` and `dev`. Set them once, before concurrent use.

Soma preserves axon's existing file formats. It doesn't migrate credentials or manage Claude Code's credentials.

## Checks

Go 1.27 or later is required. Run formatting checks, vet, build, and the local unit tests:

```sh
./scripts/check.sh
```

See the [design](docs/designs/soma.md) and [implementation plan](docs/plans/2026-11-16-soma.md) for the server and axon integration work.
