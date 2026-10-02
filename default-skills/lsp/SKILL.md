---
name: lsp
description: Use existing stdio language servers for read-only definitions, references, hover and symbols.
---

Use `lsp_query` when semantic navigation is more useful than text search.
Check that the server is installed (`command -v SERVER`) and that the project
has its normal dependency/configuration files. Start it in the project root:

```json
{"command":"exec gopls","background":true,"protocol":"lsp"}
```

Common commands: `gopls`, `pyright-langserver --stdio`, `clangd`,
`rust-analyzer`, `typescript-language-server --stdio`. Choose the server for
the project language; do not install dependencies merely to answer a query.
Use `workdir` when the language-server workspace differs from cwd.

Pass the returned `job_id` to `lsp_query`. `definition`, `references` and
`hover` require `path`, `line` and `column`. Positions use **1-based Unicode
code points**, including non-ASCII characters. `document_symbols` requires
only `path`; `workspace_symbols` requires `query` (empty means all symbols).
For unknown file extensions, provide `language_id`. Files are synchronized
from disk on each query. Locations and symbols support `offset` and `limit`
(default 100, maximum 500); use `next_offset` to request another page.

The client owns protocol stdin/stdout. Use `job_read` with `stream:"stderr"`
for diagnostics, and `job_stop` to stop the server. Never send raw RPC through
shell or read protocol stdout. Queries default to a 30-second timeout; increase
`timeout_ms` up to 120000 for a cold index. On a stopped or failed initialization,
inspect diagnostics and restart. These live handles do not survive session
switching or exit. Queries are read-only; use file tools to make changes.
