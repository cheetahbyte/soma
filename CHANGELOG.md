# Changelog

## Unreleased

- The server keeps a usage log, `<root>/usage.jsonl`: one line per provider call that reported tokens, with the upstream, API, model, session id, usage, and stop reason, and no message content. It is bounded by `Options.UsageLogBytes` (default 16 MiB). `GET /v1/usage?after=<seq>` serves it in pages to a token-holding client, with the log's id and the oldest kept `seq`.
- A `codex` provider without `models` takes its models from ChatGPT's model list instead of a bundled table. Hidden models are left out, the models keep ChatGPT's priority order, and `SuggestedDefault` names the first cached one. Each model gets ChatGPT's context window and `minimal` plus the supported levels from `low` to `xhigh`; `off` is offered only through `modelOverrides`. Until a fetch succeeds, any model id is accepted with the default values. `soma login codex` caches the list after the sign-in and warns when it can't read it. Explicit `models` stay authoritative and read no list.
- Add versioned HTTP streaming libraries with bearer-token authentication and strict request decoding.
- Add remote model information, conversation-capability caching, and bounded end-of-run signals.
- Preserve provider errors and detect quiet or incomplete remote streams. Blob transfer and abort recovery remain pending.
- The server reloads `models.json` and `auth.json` when they change, so a login after start is used by the next request. It runs `claude` in `<root>/claude-work` and removes `SOMA_TOKEN` and `TS_AUTHKEY` from its environment.
