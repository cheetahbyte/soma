# Deploying soma

This guide sets up one person's soma server on Railway. Each person runs their own service in their own Railway project. The service holds that person's logins and answers model requests from axon on a laptop (over Tailscale) and from sandboxes (over Railway's private network).

Steps marked **Confirm** describe Railway or Tailscale behavior that this repository does not verify. Check each one against the current Railway and Tailscale documentation or the dashboard before relying on it.

## What the image holds

The `Dockerfile` builds `cmd/soma` from source and installs Node.js and Claude Code (`@anthropic-ai/claude-code`), so `claude` is on `PATH`. The default command is `soma serve --listen [::]:7662`. The image sets:

| Variable | Value | Meaning |
| --- | --- | --- |
| `SOMA_HOME` | `/data/soma` | The root: `models.json`, `auth.json`, caches, the usage log `usage.jsonl`, and the Tailscale state in `tailscale/`. |
| `HOME` | `/data/home` | Claude's home directory, so its login and conversation files persist. |

Both paths sit under `/data`, so one volume keeps everything. Losing the volume loses the ChatGPT login, because the stored refresh token was the only valid one. Nothing copies a login between machines.

`/data/soma/usage.jsonl` records the token usage of every request the server serves: upstream, model, session id, token counts, cost, and stop reason, never message content. It stays under 16 MiB by dropping its oldest lines. A script holding `SOMA_TOKEN` pulls it with `GET /v1/usage?after=<seq>`, for example to feed toktrack on a Mac.

Build and check the image locally, if Docker is available:

```sh
docker build -t soma .
docker run --rm soma claude --version
docker run --rm -e SOMA_TOKEN=t soma soma version
```

The build copies the whole repository, including `go.work` and `go.work.sum`, because `cmd/soma` builds only in workspace mode. It needs Go 1.27.4 or later.

## Prerequisites

- A Railway account and the Railway CLI.
- A Tailscale tailnet and an auth key for it. Create the key in the Tailscale admin console. Decide whether the key is reusable or tagged. **Confirm** that the key type you choose lets a container node rejoin after a restart.
- A long random token for `SOMA_TOKEN`. Generate one:

  ```sh
  openssl rand -hex 32
  ```

## Create the service

1. Create a Railway project for this person.
2. Add one service from this repository. Railway builds the `Dockerfile` at the repository root. **Confirm** that Railway picks up the `Dockerfile` without extra configuration.
3. Add a volume to the service and mount it at `/data`.
4. Set the service variables:

   | Variable | Value |
   | --- | --- |
   | `SOMA_TOKEN` | The token you generated. The server refuses to start without it. |
   | `TS_AUTHKEY` | The Tailscale auth key. Without it the server listens only on `--listen`, and laptops cannot reach it. |

   Never commit either value. Use placeholders in notes and scripts.
5. Do not generate a public domain for the service. The token is a second check; the first is that the server is reachable only from the person's tailnet and the project's private network.
6. Deploy. The log shows `soma: listening on [::]:7662`, and with `TS_AUTHKEY` set, `soma: listening on the tailnet as soma, port 80`. The tailnet host name is `soma` unless you change the command. To use another name, set a start command of `soma serve --listen [::]:7662 --hostname <name>`. **Confirm** that the new machine appears in the Tailscale admin console.

To change the token, set a new `SOMA_TOKEN` and restart the service. Every asking side then needs the new value.

## Log in

Open a shell on the service with Railway's own tools. **Confirm** the current command (for example `railway ssh`, or the dashboard's shell) and that it runs inside the deployed container.

### ChatGPT

Run in the service shell:

```sh
soma login codex
```

The command prints a sign-in address. Open it in any browser, sign in, then paste back the address the browser was sent to after signing in. The login is stored in `/data/soma/auth.json`. **Confirm** that the paste-back flow works against the real sign-in service from a datacenter address.

### Claude

Soma never performs or stores a Claude login. Log `claude` in through its own login on the service, in the same shell:

```sh
claude
```

Follow the login prompts that `claude` shows. **Confirm** which login method `claude` offers on a machine without a browser, and that its login persists under `/data/home`.

### Melious (optional)

```sh
soma login melious --stdin
```

Paste the API key on one line, then press Enter and Ctrl-D. The key is checked against Melious and stored.

A login performed after the server started is used by the next request. No restart is needed.

### Models

A server without a `models.json` offers `chatgpt` once a ChatGPT login exists, `claude` when `claude` is on `PATH`, and `melious` once its key is stored. To define more providers, write `/data/soma/models.json` in the service shell. Its format matches axon's.

## Point axon at the server

Each remote entry in axon's `~/.axon/models.json` uses `"api": "soma"`. `upstream` names the provider on the server and defaults to the entry's name. Set `SOMA_TOKEN` in the environment of the machine that runs axon, to the same value as on the service.

### Laptop (tailnet address)

The server listens on port 80 on the tailnet. Use the tailnet host name, or its MagicDNS name or tailnet IP address. The laptop must be signed in to the same tailnet. **Confirm** the address in the Tailscale admin console.

```json
{
  "providers": {
    "home-chatgpt": {
      "api": "soma",
      "baseUrl": "http://soma",
      "apiKey": "$SOMA_TOKEN",
      "upstream": "chatgpt"
    },
    "home-claude": {
      "api": "soma",
      "baseUrl": "http://soma",
      "apiKey": "$SOMA_TOKEN",
      "upstream": "claude"
    }
  }
}
```

Keep the laptop's local providers in the same file under different names, so axon still works when the server is unreachable. Choosing between them is choosing a model.

### Sandbox (private address)

Create the sandbox in the same Railway project with the private network enabled:

```sh
railway sandbox create --private-network
```

**Confirm** the flag with `railway sandbox create --help`, and that the sandbox reaches the service.

The server listens on port 7662 on every address, including IPv6. Use the service's private DNS name. **Confirm** the exact name in the Railway dashboard; it is normally `<service-name>.railway.internal`.

```json
{
  "providers": {
    "home-chatgpt": {
      "api": "soma",
      "baseUrl": "http://<service-name>.railway.internal:7662",
      "apiKey": "$SOMA_TOKEN",
      "upstream": "chatgpt"
    }
  }
}
```

The sandbox holds this token and no provider login. Set `SOMA_TOKEN` in the sandbox environment, never in the file.

## Hand check

Run this once with real logins and record each result. Record the date, the commit, and what you saw. Do not paste tokens or keys into the record.

- [ ] Image builds on Railway and the service starts.
  Result:
- [ ] The log shows the listener on `[::]:7662` and, with `TS_AUTHKEY`, the tailnet listener.
  Result:
- [ ] `soma login codex` completes through the paste-back flow, and `/data/soma/auth.json` exists.
  Result:
- [ ] Claude is logged in through the service shell, and `claude --version` and a short request work there.
  Result:
- [ ] A request from a laptop over Tailscale to a ChatGPT model streams a reply.
  Result:
- [ ] A request from a laptop over Tailscale to a Claude model streams a reply, and a second run in the same session resumes the same Claude conversation.
  Result:
- [ ] A request from a sandbox over the private network streams a reply.
  Result:
- [ ] A request with a wrong token is rejected.
  Result:
- [ ] The service has no public domain, and the token alone does not reach it from the internet.
  Result:
- [ ] A login performed while the server is running is used by the next request without a restart.
  Result:
- [ ] A redeploy during a reply: the stream breaks, axon reports a network error that names soma, and resuming the session works.
  Result:
- [ ] After a redeploy, the ChatGPT login, the Claude login and the Tailscale node persist (the volume kept them).
  Result:
- [ ] How long Railway waits between the termination signal and the kill, noted for token refresh.
  Result:
- [ ] ChatGPT and Claude logins keep working from a datacenter address across a day.
  Result:
