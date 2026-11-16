# Changelog

## Unreleased

- Add versioned HTTP streaming libraries with bearer-token authentication and strict request decoding.
- Add remote model information, conversation-capability caching, and bounded end-of-run signals.
- Preserve provider errors and detect quiet or incomplete remote streams. Blob transfer and abort recovery remain pending.
- The server reloads `models.json` and `auth.json` when they change, so a login after start is used by the next request. It runs `claude` in `<root>/claude-work` and removes `SOMA_TOKEN` and `TS_AUTHKEY` from its environment.
