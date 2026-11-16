# Fake Claude Code

A fake of the `claude` program in headless stream-json mode, for local tests.
Call `Run(args, stdin, stdout, stderr)` from a test program and use its return
value as the exit status. Set the provider's `command` to a wrapper that runs
that program. The fake takes the flags documented here and returns exit
status 2 for any other flag.

It writes the JSON lines the real program writes (the spike's formats, see
below) and acts as an MCP client over HTTP toward the one server of
`--mcp-config`, expanding `${VAR}` in header values from its own environment.
Order: `server/discover` (error ignored), `initialize`, `notifications/initialized`,
GET (ignored), `tools/list`. A turn starts when a stdin line arrives; it begins
with an `init` line (every turn has one, as in the captures), and each
model message is preceded by `system/status` `requesting` and streamed; a `tool_use` block is followed at once by a
`tools/call` (params `name` without the `mcp__<server>__` prefix, `arguments`,
`_meta."claudecode/toolUseId"`). Tools with `readOnlyHint` are called all at
once; other calls one after the other. The turn continues when every call of
the message is answered. A `result` line ends each turn.

## Environment

- `TESTCLAUDE_SCRIPT`: path of the script file. Without it every turn is one
  message with the text `ok`.
- `TESTCLAUDE_LOG`: path of the log file, appended to by every process.

## Script file

A JSON object. `turns` is a list of turns, a turn is a list of model messages.
A turn starts when a stdin line arrives while no turn runs. Turns are taken in
order across all processes that share the log file (the count of `turn` records
in the log), so a resumed process continues where the previous one stopped. A
turn beyond the list is one message `ok`.

A message: `text` (list of chunks, one `text_delta` each), `thinking` (true
adds an empty thinking block), `calls` (list of `{"name","args"}`; `name` is
the tool's name on the server, `args` any JSON). A message with calls ends with
`stop_reason` `tool_use`, one without with `end_turn`. A call whose tool is not
in `tools/list` is sent anyway. When the server is not connected, calls are
dropped.

```json
{
  "turns": [
    [
      {"text": ["Let me ", "look."], "calls": [{"name": "read", "args": {"path": "a.txt"}}]},
      {"text": ["Done."]}
    ]
  ],
  "costPerTurn": 0.01
}
```

Switches (all optional):

- `overage`: every `rate_limit_event` has `isUsingOverage: true`.
- `failInit`: no connection to the server; `init` reports it `failed`, no tools, calls dropped.
- `notLoggedIn`: after the first stdin line, writes an error `result` with the text `Not logged in · Please run /login` and exits 1; no `init`.
- `exitAfter` N: exits 1 with a stderr line `testclaude: exiting as scripted` after the Nth model message of the process, without a `result`.
- `carryOver`: `total_cost_usd` and the tokens of `modelUsage` count every turn of the script so far, as the real program does for a resumed conversation; without it they count the turns of this process.
- `rejectResume`: with `--resume <id>`, writes an error `result` (`is_error: true`, `errors: ["No conversation found with session ID: <id>"]`), the same text on stderr, and exits 1.
- `retryNotice`: before the first message of turn 0, writes a `system` line with `subtype` `api_retry`.
- `costPerTurn` F: `total_cost_usd` of a `result` is F times the turns finished in this process, including this one.
- `apiKeySource` S: the `init` line reports `apiKeySource` S instead of `none`.
- `progressToken`: `tools/call` carries `_meta.progressToken` (the tool-use id). Off by default because the spike does not show that the real program sends it.

Other behavior: `--resume <id>` keeps the session id, otherwise a random UUID is
used. A stdin line that arrives while calls are held is consumed with the
results and is part of the same turn; one that arrives otherwise starts the next
turn. SIGINT cancels held calls, writes a `result` (zero usage, `subtype`
`error_during_execution`, `is_error` true, `errors` with one diagnostic line,
no `result` text, `stop_reason` null) when a turn runs, and exits 0. Closing stdin lets the
running turn finish, then exits 0. Usage of every model message is 100 input
and 20 output tokens; cache counts are 0.

## Output lines

All lines carry `session_id` and `uuid`. In order of a message:
`system/init`, `system/status`; `stream_event`
with `message_start`, `content_block_start/delta/stop` (`thinking` with two
`system/thinking_tokens` lines, a `thinking_delta` and a `signature_delta`;
`text` with `text_delta`; `tool_use` with an empty and then the full
`input_json_delta`), `message_delta` (`stop_reason`, usage with
`output_tokens_details.thinking_tokens`), `message_stop`; one `assistant` line
per finished block, written before that block's `content_block_stop`;
`rate_limit_event` (`rate_limit_info`), only after the first message of the
process; a `user` line with a `tool_result` block
per answered call, in completion order; `result` (`subtype`, `is_error`,
`result`, `stop_reason`, `num_turns`, `total_cost_usd`, `usage`).

## Log file

JSON lines, appended; every record has `event`:

- `start`: `pid`, `args` (argv without the program name, secret unexpanded), `env` (sorted variable names only, no values), `cwd`.
- `stdin`: `line`, every non-empty stdin line as read.
- `mcp`: `server`, `status` (`connected` or `failed`), `tools` (`mcp__<server>__<name>`), `readOnly` (tool names with the hint).
- `turn`: `n`, the global turn index used in the script.
- `call`: `id` (tool-use id), `name`, `args`, `isError`, `text` (the text parts of the answer), `images` (the MIME types of its image parts), written when the answer arrived.
- `interrupt`: SIGINT received.
- `exit`: `code`.

## Alignment with the captures

Checked against `/tmp/axon-wt/spike-claude/logs/raw-*.jsonl`. Not shown by any
capture, so still assumptions: the `not logged in` result, the rejected
`--resume`, `system/api_retry`, whether `init` is written before the first stdin
line, whether `rate_limit_event` appears again after a changed status, and what
a process does after SIGINT during a held call (the fake exits). Not imitated:
`tool_progress` heartbeats every 30 s while a call is held, subagent lines.