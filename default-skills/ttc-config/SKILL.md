---
name: ttc-config
description: Configure TTC's supported rail, web-search and launch settings from a user request, preserving credentials and isolation boundaries.
---

# Configure TTC

Use this skill when the user asks to configure TTC itself, not application code.
Consult the installed version's `ttc --help`, `ttc rail --help` and TTC README
for supported options; do not invent a general settings file or undocumented
UI settings. Make routine, clearly requested changes without extra permission
prompts. Ask when scope is materially ambiguous or a security boundary changes,
particularly broad read-write mounts or global Docker authorization.

## Choose the supported surface

- **Rail, global:** `${XDG_CONFIG_HOME:-$HOME/.config}/ttc/rail.json`.
  Changes affect future rail instances across projects.
- **Rail, project:** `<workdir>/ttc-rail.json`, with no ancestor search.
  Prefer this for project-specific mounts and denies.
- **Web search:** `${XDG_CONFIG_HOME:-$HOME/.config}/ttc/web-search.json`, or
  the file selected by `--web-search-config PATH`. Documented fields are
  `api_key` and optional HTTP(S) `endpoint`; the public Exa endpoint works
  without a key. Restart TTC after changes.
- **Launch options:** `--model`, `--variant`, `--workdir`, `--data-dir`,
  `--plain` and other flags shown by `ttc --help`. Provide a launch command or
  edit a user-requested launcher; do not pretend these are JSON settings.
  Use available model IDs/reasoning presets, not guesses; `/model` is the
  interactive selector. Never edit saved model selection directly.
  `TTC_DATA_DIR` selects an absolute default data root; `--data-dir` overrides
  it. Rail sets it to the instance's shared state directory.
- **Authentication:** use the supported `ttc --login` or
  `ttc --import-codex-auth PATH` workflow, letting the user handle private
  authorization. Never read, print or directly edit TTC/Codex credentials,
  tokens or the history database. A data-directory change selects storage;
  it is not an instruction to migrate or modify history.

## Inspect and change safely

1. Resolve the actual workdir, XDG config directory and any explicit config
   override. Read applicable AGENTS.md files and existing **non-secret** configs
   before editing. Inspect both rail layers to understand the effective result.
   Do not silently replace a missing or invalid explicitly selected path with
   a default; report the error.
2. Use `read`, `edit`, `patch` or `write` for config/launcher changes. Preserve
   unrelated settings and make the smallest change. Workspace changes participate
   in shared undo. Global files outside the workspace are editable when permissions
   allow, but those edits are **not undoable** and can block undo across the change;
   explain this and their cross-project scope before editing. Never use shell
   redirection as an alternative editing path.
3. Do not load secret-bearing files into tool output or copy secrets into
   prompts, diffs, commands or logs. Have the user enter web-search keys privately
   and keep credential files mode 0600. If an existing web-search config contains
   a key, give the user a minimal private-edit instruction rather than exposing
   its contents through file tools or their undo previews.
4. Existing rail configs are mounted read-only inside rail. If an edit is denied,
   explain that the user must edit from the host. Never remount, bypass the
   sandbox, or use Docker to escape it. Do not stop a running rail instance or
   restart TTC without coordinating with the user; that can interrupt work.

## Rail rules to preserve

Read the README's rail configuration section for complete examples and limits.
The top-level fields are `allow`, `deny` and `authorize_services`; unknown fields
and malformed JSON fail startup. Rail config entrypoints must be regular files,
not symlinks. Global and project lists combine, with project
allows replacing global allows at the same destination; **denies always win**.

Path strings allow read-only mounts at the same path. Allow objects use `source`,
optional `dest` and `mode` (`ro` or `rw`). Relative paths resolve against the
containing config's directory; `~` expands to home without shell expansion.
Explicit sources must exist. Deny strings name container paths; deny objects use
`source` as the default destination or explicit `dest`. Denies hide contents,
including subpaths of writable mounts, but do not block the same host data at
another independently allowed destination. Reserved process/device/control paths
and denies covering the entire workdir are rejected.

`docker` is the only service shorthand and mounts `/var/run/docker.sock`
read-write. It requires `authorize_services: ["docker"]` in **global** config;
projects cannot authorize themselves. `deny: ["docker"]` disables it; custom
`DOCKER_HOST` endpoints are unsupported by the shorthand. Explain that Docker
access through ordinary mounts of the standard socket or its parent also requires
the enabled authorized service, even for read-only mounts. Docker
access can control the host and defeats filesystem isolation. Rail also shares
host networking: read-only secrets remain readable and can be transmitted.
Do not broaden access or remove denies just to make a command succeed.

## Verify and report

Check JSON syntax without dumping sensitive contents, validate requested paths
and flags against the installed help/README, and inspect the final non-secret
diff. `ttc rail --list` shows live instances; it does **not** validate new config.
Rail validates configuration when creating an instance and fails closed; do not
launch an interactive sandbox merely as an unattended validation command.
Changes require stopping and recreating the instance, not reattaching. Report
which file or launch command changed, its scope, checks performed, and the exact
restart/recreation step still needed. Do not claim a running instance adopted
new settings before it has been recreated.
