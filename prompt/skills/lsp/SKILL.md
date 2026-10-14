---
name: lsp
description:
  Use existing stdio language servers for read-only definitions, references,
  hover and symbols.
---

Use `lsp_query` when semantic navigation is more useful than text search. First
use `job_list` to find an accessible live server for the project. Reuse it
rather than launching one per query, including a server owned by another actor.
Do not stop a borrowed server. If none is available and `shell` is absent,
report the missing server and use read/grep navigation instead.

With `shell` available, check that the server is installed (`command -v SERVER`)
and that the project has its normal dependency/configuration files. Start it in
the project root:

```json
{
  "command": "exec gopls",
  "background": true,
  "protocol": "lsp",
  "wake_on_exit": false
}
```

Common commands: `gopls`, `pyright-langserver --stdio`, `clangd`,
`rust-analyzer`, `typescript-language-server --stdio`. Choose the server for the
project language; do not install dependencies merely to answer a query. Use
`workdir` when the language-server workspace differs from cwd.

Pass the accessible server's `job_id` to `lsp_query`. `definition`, `references`
and `hover` require `path`, `line` and `column`. Positions use **1-based Unicode
code points**, including non-ASCII characters. `document_symbols` requires only
`path`; `workspace_symbols` requires `query` (empty means all symbols). For
unknown file extensions, provide `language_id`. Each file query synchronizes
only its requested file from disk; workspace-symbol queries synchronize none.
After editing previously queried files, synchronize them with file queries
before relying on cross-file results. If the server does not support text
changes, restart an owned server with available lifecycle tools. For a borrowed
server, or when restart tools are unavailable, report its job ID and stale-file
limitation to the owning/main agent rather than relying on stale results.
Locations and symbols support zero-based `offset` and `limit` (default 100,
maximum 500); use `next_offset` to request another page. Hover does not accept
pagination. For example, substitute the returned job ID:

```json
{
  "job_id": "job_xyz",
  "operation": "definition",
  "path": "main.go",
  "line": 12,
  "column": 3
}
```

The client owns protocol stdin/stdout. Use `job_read` with `stream:"stderr"` for
diagnostics, and `job_stop` when available to stop an owned server. Never send
raw RPC through shell or read protocol stdout. Queries default to a 30-second
timeout; increase `timeout_ms` up to 120000 for a cold index. On a stopped or
failed initialization, inspect diagnostics, then reuse another accessible server
or restart with available lifecycle tools; otherwise report the missing server.
Compaction preserves these live handles; session switching or exit closes them.
A disposable subagent also stops its owned server on completion. Queries are
read-only; use file-mutation tools when available for changes, otherwise report
proposed changes.
