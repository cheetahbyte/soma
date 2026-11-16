# Soma: model access for axon, as a library and a server

**Status:** draft, reviewed once against the axon code on 2026-11-16

Terms used here. **Axon** is the owner's terminal coding agent (`github.com/cheetahbyte/axon`). A **provider** is the code that talks to one model service: today axon has `openai-responses`, `openai-chat`, `codex` (a ChatGPT subscription through an OAuth login) and `claude-code` (a Claude subscription through the installed `claude` program). A **root** is the directory that holds `models.json`, `auth.json` and the model caches; axon's root is `~/.axon`. **Soma** is the new project this spec describes. A **soma server** is one running `soma serve` process. The **asking side** is the program that sends model requests, which is axon. **Local mode** means axon runs providers in its own process through soma's library. **Remote mode** means axon sends each model request to a soma server. An **entry** is one provider in axon's `models.json`; an **upstream** is a provider on a soma server.

## Problem

- An axon session that runs somewhere other than the owner's laptop, such as a Railway sandbox, needs model access. The owner does not want a provider login or key inside a sandbox.
- A ChatGPT login cannot be copied to a second machine. Its refresh token rotates on use (`internal/auth/refresh.go` in axon), so two holders invalidate each other.
- The Claude subscription works only through the `claude` program under its own login, so a remote session cannot use Claude at all.
- The owner and their partner both want to use the same setup, each with their own logins.
- The provider code lives under axon's `internal/` directory, so no other program can use it.

Not solved here: on a laptop, any command the agent runs can read `~/.axon/auth.json`. Laptops keep their local logins (see Decisions), so that stays true. A machine that lists only soma entries holds no login.

## Solution

Soma becomes the one owner of model access. The provider code, the logins and the model information move from axon into soma, a separate public Go module at `github.com/cheetahbyte/soma`.

Soma is used in two ways, with the same provider code behind both:

- **As a library.** Axon imports soma and runs providers in its own process. This is today's behavior, with today's files.
- **As a server.** A `soma serve` process runs on an always-on machine, holds that person's logins, and answers model requests in axon's own request and reply types. A remote axon holds the server's address and a token, and no provider login.

Each person runs their own soma server, one Railway service per person. Sandboxes reach it over Railway's private network; laptops reach it over Tailscale.

## Behavior

### Local mode

1. **Nothing changes for an existing setup.** Axon with its current `~/.axon/models.json` and `~/.axon/auth.json` behaves as it does today, including its messages, its user agent, and the implicit providers a login adds. No file is migrated, and no soma server is needed.
2. **`axon login codex` and `axon login melious [--stdin]`** work as today and write to `~/.axon/auth.json`. They call soma's library.

### Remote mode, seen from axon

3. **Configuration.** An entry with `"api": "soma"` names a soma server (`baseUrl`), a token (`apiKey`, literal or `$ENV_NAME`), and the upstream it stands for (`upstream`, default the entry's own name). One entry stands for one upstream. An entry may list `models`; without a list, its models are the upstream's, fetched from the server.
4. **A request.** The reply streams as it does locally: text, thinking and tool calls as they arrive, then the complete message with its usage and cost.
5. **Names.** The session log records the entry's name as the provider, as for any provider, so a resumed session resolves to the same entry. The API and model recorded are the upstream's. In every message it sends, the soma client replaces the entry's name with the upstream's name, so the server recognizes its own earlier replies and can resume a Claude conversation or replay encrypted reasoning.
6. **Claude.** The `claude` program runs on the soma server under the login there. Its tool calls are returned to axon and run where axon runs, with axon's hooks, session log and tool lines. The next run continues the same Claude conversation by the session id axon records; the conversation files `claude` keeps are on the server.
7. **Claude's own environment description names the server.** Claude Code adds a description of its working directory and platform that documented options do not remove (`docs/designs/claude-code-provider.md` in axon). The server runs `claude` in an empty directory under its root, so the description names nothing real. Axon's system prompt states the real directory. This is accepted.
8. **End of a run.** For an upstream that keeps the conversation itself, axon tells the server, which ends that session's `claude` process. The call is bounded to 5 seconds and its failure is ignored. For other upstreams nothing is sent.
9. **Abort.** Esc or Ctrl+C stops the reply, and the session keeps the text received so far, as in local mode. The stream's first line carries a stream id; the asking side sends an abort request naming it; the stream's final line then carries the partial message.
10. **Images and stored tool output** referenced by the conversation reach the provider as they do locally. The asking side uploads each blob once under its content hash; a request names hashes; the server answers with the hashes it lacks, and the asking side uploads them and sends the request again. The server's blob cache is bounded on disk.
11. **Errors keep their kind.** Rate limit, exhausted quota, context overflow, provider authentication and server errors arrive with the same kind, status and retry delay, so axon retries, compacts or stops exactly as in local mode.
12. **Soma-side failures are named as such.** An unreachable server, or a stream that ends early, is a network error retried under axon's existing rules (3 attempts over about 14 seconds); the final message names the soma server. A wrong token is an authentication error that names soma, not the provider. The retry budget is not changed for soma.
13. **A quiet stream.** While a reply is in flight and nothing else was sent for 30 seconds, the server sends a keep-alive line. The asking side treats 5 minutes without any line as a network error, the limit axon's HTTP providers use today.
14. **A missing login on the server** fails the first request with a message that names the command to run on the server (`soma login codex`). Local mode reports the same condition at startup; remote mode cannot know before it asks.
15. **Model information** (context window, output limit, thinking levels, prices, the model list, and whether the upstream keeps the conversation itself) comes from the server. A soma entry without a model list is treated like today's live-catalog provider: fetched at first use with the existing 10-second limit, cached in the catalog cache, refreshed as catalogs are today, with a failed fetch cached for 10 minutes. Until a fetch succeeds, a model gets today's defaults for an unknown model, and the upstream is taken not to keep the conversation.
16. **Version mismatch.** Every request carries a protocol version. The server refuses a version it does not speak, and refuses requests with fields it does not know, so nothing is dropped silently. The error says which side is older.
17. **Subagents and several sessions** can use the same server at once.

### The server

18. **Start.** `soma serve` reads its root (`SOMA_HOME`, default `~/.soma`), its listen address (`--listen`), its token (`SOMA_TOKEN`), and optionally a Tailscale auth key (`TS_AUTHKEY`) and host name. It refuses to start without a token.
19. **One server serves one person.** It has one token and one root. A second person runs a second server.
20. **Timers.** Soma starts no timer of its own while no reply is in flight and no session holds a `claude` process. Axon's idle rule does not bind soma; the Tailscale library has its own activity.
21. **Configuration is re-read when it changes.** On each request the server compares the modification times of `models.json` and `auth.json` with the ones it loaded, and reloads when they differ. A provider that currently holds a `claude` process is replaced when its run ends. A login performed after the server started is therefore used by the next request, without a restart.
22. **A server without `models.json`** offers the implicit providers soma's subscription defaults add: `chatgpt` once a ChatGPT login exists, `claude` when the `claude` program is on `PATH`, and `melious` once its key is stored. A `models.json` is optional; the person writes it through a shell on the service.
23. **A server's `models.json` may not contain a soma entry.** A request for such an upstream is refused.
24. **Logging in to ChatGPT without a browser on the machine.** `soma login codex` is a new login mode next to the loopback listener: it prints the sign-in address, the person opens it in any browser, and pastes back the address the browser was sent to after signing in. The same state check applies. The login is stored in the server's root. The person reaches the service's shell with Railway's own tools.
25. **API-key logins.** `soma login melious --stdin` reads a key as `axon login melious --stdin` does today.
26. **Claude.** Soma never performs a Claude login and never reads, stores or forwards Claude Code's credentials. The person logs `claude` in on the server's machine by its own means. A request without that login fails with a message saying so.
27. **Secrets stay out of child processes.** `SOMA_TOKEN` and `TS_AUTHKEY` are removed from the environment of every `claude` process, in addition to the names axon removes today.
28. **The root must persist.** A lost root loses the ChatGPT login, because the stored refresh token was the only valid one. Nothing copies a login between machines.
29. **Shutdown.** On a termination signal the server stops accepting requests, lets a token refresh in progress finish and reach the file, ends every `claude` process it holds, and exits.
30. **An abandoned run.** When a stream's connection drops without an abort request, the server ends that session's `claude` process and marks the session. The asking side also marks its retry as following a failed stream. Either mark makes the next request for that session start a new Claude conversation from the messages axon sends, not resume the old one, so Claude never sees a turn axon did not record. If both sides restarted in between, Claude may see one user message twice; this is accepted.
31. **A run the asking side never ended.** While a session holds a `claude` process and no request for it is in flight, a one-shot timer of 10 minutes is armed; when it fires the process is ended. The next request for that session resumes by session id, as after a normal end of run.
32. **A restart or redeploy during a reply** breaks the stream. A redeploy usually outlasts axon's retries, so the run ends with a network error that names soma, and the person resumes the session.
33. **What the server writes.** Logins, model caches, and the blob cache. It writes no prompts or replies. The conversation files `claude` keeps for itself are the one exception, and soma does not manage them.

### The token

34. **The token is a credential.** Whoever holds it can spend that person's subscriptions through that server for as long as it is valid. It reveals no login, cannot refresh anything, and is useless without a network path to the server: the person's own tailnet, or the private network of the person's own Railway project. It is changed by setting a new `SOMA_TOKEN` and restarting the server; every asking side then needs the new value. A sandbox holds this token and nothing else.

### Deployment

35. **One Railway service per person,** built from source into an image holding `soma` and the `claude` program, with a volume for the root, the home directory and the Tailscale state. The service has no public domain.
36. **A person's sandboxes reach their server over Railway's private network** (`railway sandbox create --private-network`). Each person's server and sandboxes live in that person's own Railway project.
37. **A person's laptop reaches their server over Tailscale.** `soma serve` joins the person's tailnet itself when it has an auth key. Axon's `baseUrl` is the server's tailnet address. No separate Tailscale program runs in the container.
38. **Two listeners.** With Tailscale on, the server listens on the tailnet for laptops and on `--listen` for sandboxes. Without an auth key it listens only on `--listen`.
39. **Laptops keep their local logins.** A laptop lists its local providers next to its soma entries, under different names, so axon still works when the server is unreachable. Choosing between them is choosing a model, as today.

## Decisions

- **Soma owns the provider code and axon imports soma.** The owner's decision. Soma then depends on nothing, other programs can use it, and axon still works without a server.
  - Rejected: soma importing axon's providers. Soma would follow axon's releases and depend on a terminal agent.
  - Rejected: axon as a pure client of a soma server. Axon would stop working without one.
- **Move the provider boundary, not the credential.** Axon reaches every model through one call that takes a request and streams a reply, plus an end-of-run signal. Forwarding that call means the remote side needs no login and no provider-specific code. Rejected alternatives:
  - A login or key inside the sandbox: the owner's rule forbids it.
  - A TLS-intercepting credential proxy, as Crumble uses for its workspaces: it swaps fixed secrets and cannot own a rotating login, and axon controls its own endpoint, so interception is unnecessary.
  - A key held by a privileged process in the sandbox, as `codex-responses-api-proxy` does: the key is still on the sandbox, and it covers API keys only.
  - Reusing Crumble's provider logins through Pi AI: it needs a translation between Pi's message format and axon's, and ties axon to Crumble.
- **The wire carries axon's own types, not an OpenAI-compatible API.** Both sides run the same code, so nothing is translated and thinking, effort levels and replay data pass through intact. An OpenAI-compatible proxy in the style of CLIProxyAPI was rejected for that reason, and because axon's Claude Code design requires driving the real `claude` program through interfaces Anthropic documents.
- **The wire form is defined by its own JSON types and an integer version,** separate from the in-process structs, which have no JSON tags and hold a function. Any change to a wire type raises the version.
- **Go.** The server is small because it reuses the providers unchanged. In Bun they would be rewritten.
- **Streaming HTTP, not WebSocket.** A spike on 2026-11-16 ran axon's whole end-to-end suite through a prototype over plain streaming HTTP. The owner allows a WebSocket library, and none is needed.
- **One server per person, not one multi-user server.** The container isolates logins, each person's `claude` uses its own login, and soma needs no multi-user code.
- **One Railway service per person, with a volume.** The owner's choice of host. The logins then live on Railway; the design guarantees only that they are in one place and never in a sandbox.
- **Laptops reach the server over Tailscale, not a public address.** The owner's decision. The server can spend the subscriptions, so it is not exposed to the internet; the token stays as a second check. A public HTTPS domain with a long token was rejected for that exposure.
- **Tailscale is built into `soma serve` through its Go library (`tailscale.com/tsnet`).** The owner's suggestion. The library joins a tailnet from inside the process and needs no network device, which suits a container, and the image holds one program fewer. The owner sets up the tailnet and the auth key.
- **The soma repository holds two Go modules.** The root module has no requirement outside the standard library; axon imports it and inherits nothing. The program lives in a nested module at `cmd/soma`, the only place that requires `tailscale.com`. A build tag was rejected because the requirement would still sit in the module axon depends on.
- **Laptops keep local logins as a fallback.** The owner's decision. A server outage then does not stop work on the laptop.
- **The soma repository is public.** The owner's decision. Builds need no private-module setup.
- **File formats and axon's root are unchanged.** Soma's library takes a root directory, as `internal/auth` already does. Nobody's setup migrates.
- **Names that say "axon" become parameters.** The embedding program passes its name for messages such as `run "axon login codex"`, the login page title, and the user agent; axon passes `axon`, so local mode is unchanged, and the server passes `soma`. Test-only environment variables are renamed with a `SOMA_` prefix.
- **Soma is a published module that axon pins.** `axon update` runs `go install`, which refuses a `go.mod` with replace directives. Work across both repositories uses a Go workspace.
- **Soma never handles Claude's credentials.** Carried over from axon's Claude Code design.
- **Abort, blob transfer, the abandoned-run rule and the 10-minute timer** (scenarios 9, 10, 30, 31) were proposed by the spec's author. The owner was asked, raised no objection, and asked for the plan to be written.

Evidence from the spike: with every model call routed through a separate gateway process, 368 of axon's 380 end-to-end tests passed unchanged, including all 26 Claude Code tests. The 12 failures were: model information not crossing (6, scenario 15), the login living on the server as intended (4, scenario 14), abort losing the partial reply (1, scenario 9), and one test that asserts no file changes while the test hook rewrote `models.json` (1). The prototype sent blobs inline with every request, which scenario 10 replaces. The prototype is not kept.

## Interfaces and data

### The repository

| Path | Module | Holds |
| --- | --- | --- |
| `soma` (root package) | root | The shared types: request, message, block, usage, delta, tool spec, provider error, and the provider interfaces. |
| `config/` | root | Provider and model configuration: reading `models.json`, key lookup, model information, catalogs, the models.dev cache and its fetch, subscription defaults, thinking levels. |
| `auth/` | root | Logins, the `auth.json` store, and the browser and page helpers axon's MCP login also uses. |
| `provider/...` | root | The provider registry, the four providers and their shared HTTP code. |
| `toolserver/` | root | The loopback tool server the Claude provider hosts, with the JSON-RPC types and protocol constants it shares with axon's MCP client. |
| `wire/` | root | The JSON types of the HTTP API and the protocol version. |
| `client/` | root | The remote provider: the asking side of the HTTP API. |
| `server/` | root | The HTTP handler, on the standard library only. |
| `somatest/...` | root | The scripted fake model, Claude, chat and login servers. |
| `cmd/soma/` | nested | The `soma` program: `serve`, `login`, Tailscale. |

### HTTP API of a soma server

Every request carries `Authorization: Bearer <token>` and the protocol version.

| Request | Purpose |
| --- | --- |
| Stream a reply | Takes an upstream name, a request with blob hashes, and whether it follows a failed stream. Answers with a list of missing blob hashes, or with a stream of JSON lines: the stream id, deltas, keep-alives, then one final line with the message and, on failure, the error with its kind. |
| Abort | Takes a stream id. |
| Upload a blob | Takes a content hash and the bytes. |
| End a run | Takes an upstream name and a session id. |
| Provider information | Whether the upstream keeps the conversation itself, and its models with their information. |

### Configuration

- **Axon `models.json`:** new `api` value `soma`, using `baseUrl`, `apiKey`, `models`, and the new field `upstream`. Existing entries are unaffected.
- **Axon's catalog cache file:** gains whether the upstream keeps the conversation. An older file without the field reads as "does not".
- **Soma server:** `SOMA_HOME`, `SOMA_TOKEN`, `--listen`, and optionally `TS_AUTHKEY` and a host name. Its root uses the same `models.json` and `auth.json` formats as axon's.
- **Session logs:** unchanged.

### The configuration types after the split

Soma's `config` package owns one type holding the root, the providers and the models.dev cache. It is loaded from a root, reads `models.json` only, adds the implicit providers, and reports the default model those defaults suggest without storing it anywhere. Provider factories take this type.

Axon's `internal/config` keeps its type with the settings and holds soma's type inside it. It keeps `Root()` and `AXON_HOME`, loads `settings.json` itself, applies the suggested default model when the setting is empty, and forwards the model methods its 32 importers use today, so those files change little.

## Affected code

Soma: everything is new. The code that moves keeps its structure, and its unit tests move with it.

Axon (`github.com/cheetahbyte/axon`):

| Path | Change |
| --- | --- |
| `internal/provider/` | Moves to soma, with its tests. |
| `internal/auth/` | Moves to soma. `internal/mcp/oauth.go` and `cmd/axon/mcp.go` then import its helpers from soma. |
| `internal/core/provider.go`, `internal/core/message.go` | The types move to soma; axon keeps aliases so the files that import `core` are untouched. |
| `internal/config/` | Splits as described above. `cache.go`, `modelsdev.go`, `subscriptions.go`, `thinking.go` and the provider half of `config.go` move; `settings_write.go`, `state.go` and the settings half stay. |
| `internal/app/modelsdev.go` | The models.dev fetch moves to soma so a server can run it; the call that triggers it after the first interactive prompt stays. |
| `internal/mcp/serve.go`, and from `internal/mcp/client.go` the types `CallResult`, `Content`, `rpcMsg`, `RPCError` and the constants `protocolVersion`, `supportedVersions`, `clientName` | Move to `toolserver/`. Axon's MCP client stays and uses them from there. |
| `internal/testmodel/`, `internal/testclaude/`, `internal/testchat/`, `internal/testauth/` | Move to `somatest/`. |
| `cmd/axon/reg_openai.go`, `reg_chat.go`, `reg_claudecode.go` | Import the providers from soma; one new file registers the soma client. |
| `cmd/axon/reg_e2e_*.go` | Keep their role, set soma's renamed test hooks, and gain a hidden `testsoma` command that runs soma's `server` package. |
| `cmd/axon/login.go` | Calls soma's library. |
| `internal/app/app.go`, `internal/subagent/child.go` | Build providers and fetch model lists through soma. |
| `e2e/helper_test.go`, `scripts/check.sh` | The switch that runs the suite through a soma server, and a second run of the suite with it on. |
| `AGENTS.md`, `docs/designs/axon.md` | The dependency rule gains soma's root module; the configuration section names soma as the owner of `models.json` and `auth.json`. |
| `docs/providers.md`, `docs/chatgpt-subscription.md`, `docs/claude-subscription.md`, `docs/configuration.md` | Describe remote mode. |

Prior art followed: the provider registry (`internal/provider/provider.go`), the root parameter of `internal/auth/store.go`, the catalog cache (`internal/config/cache.go`), the stripped environment of the `claude` child (`internal/provider/claudecode/process.go`), the rule that decides between resuming and starting a Claude conversation (`internal/provider/claudecode/plan.go`), and the hidden test commands compiled under the `e2e` tag (`cmd/axon/reg_e2e_claude.go`).

## Testing

Agreed with the owner on 2026-11-16. No test uses real keys, paid APIs, or anything beyond localhost.

1. **Axon's end-to-end seam through `axon ask`, unchanged.** All tests in `e2e/` stay green with providers running in-process from soma. This proves the move changed no behavior.
2. **The same axon suite a second time through a soma server on localhost.** The helper starts the test binary's hidden `testsoma` command with the test's provider configuration and points the test's axon at it. Using axon's own test binary means the test-only hooks and the fake `claude` are present in that process, and axon's module never builds the `soma` program or meets Tailscale. This proves remote mode matches local mode. The login tests are adapted, since the login then lives in the server's root. `scripts/check.sh` runs both passes.
3. **Soma's own seam: the `server` package over HTTP on localhost,** driven by `client` against the fakes in `somatest`. This proves the token check, version mismatch, provider information, blob transfer, abort, keep-alive, the abandoned-run rule, the timer, configuration reload and the paste-back login. Provider behavior tests that live in axon's `e2e/` today (token refresh, model catalogs, replay) move here over time.
4. **Unit tests of the packages that move** move with them.
5. **By hand, once, recorded in this spec** as axon's Claude Code design records its "Real check": real ChatGPT and Claude logins on a Railway service, a request from a laptop over Tailscale, a request from a sandbox over the private network, and a redeploy during a reply.

## Out of scope

- The workspace manager extracted from Crumble, axon's `/remote`, and worktree sessions (`axon -w`). They are separate work that will use soma.
- A multi-user server, user accounts, per-sandbox tokens, or an admin interface.
- An OpenAI-compatible API, and any client other than axon.
- Crumble using soma.
- WebSocket transport.
- Spending caps and per-request budgets.
- A longer retry budget for soma network errors.
- Usage reporting for toktrack inside soma. Instead, the server keeps a bounded usage log, `<root>/usage.jsonl`, with one line per provider call that reported tokens and no message content, and serves it on the authenticated `GET /v1/usage?after=<seq>`. A script on the owner's Mac pulls it and feeds toktrack, beside the existing mirror script for local sessions; nothing in soma's `client` or in axon calls the route.
- One setting that makes all of a server's providers appear in axon at once.
- Release binaries for soma. The image builds from source.

## Open questions

All are facts to check during implementation or the hand check; none blocks the plan.

1. **The Tailscale library's exact use** (implementer). Unverified against its documentation: how the auth key and the state directory are passed, and that a server built this way accepts inbound connections inside a Railway service.
2. **The paste-back ChatGPT login** is unverified against the real sign-in service.
3. **Claude's login on a machine without a browser.** Which method `claude` offers has not been checked.
4. **Subscription logins used from a datacenter address.** Untested for both ChatGPT and Claude.
5. **`railway sandbox create --private-network`** reaching a service in the same project is taken from the CLI's help text, not tested.
6. **How long Railway waits between the termination signal and the kill.** Scenario 29 needs a token refresh to finish inside it.
