---
name: ttc-config
description: Configure TTC's supported rail, web-search and launch settings from a user request, preserving credentials and isolation boundaries.
---

# Configure TTC

Use this skill when the user asks to configure TTC itself, not application code.
Consult the installed version's `ttc --help`, `ttc rail --help` and the bundled
reference below for supported options; do not invent a general settings file or undocumented
UI settings. Make routine, clearly requested changes without extra permission
prompts. Ask when scope is materially ambiguous or a security boundary changes,
particularly broad read-write mounts or global Docker/SSH-agent authorization.

## Choose the supported surface

- **Rail, global:** `${XDG_CONFIG_HOME:-$HOME/.config}/ttc/rail.json`.
  Changes affect future rail instances across projects.
- **Rail, project:** `<workdir>/ttc-rail.json`, with no ancestor search.
  Prefer this for project-specific mounts and denies.
- **Web search:** `${XDG_CONFIG_HOME:-$HOME/.config}/ttc/web-search.json`, or
  the file selected by `--web-search-config PATH`. Documented fields are
  `api_key` and optional HTTP(S) `endpoint`; the public Exa endpoint works
  without a key. Restart TTC after changes; recreate rail if an imported config
  was replaced on the host (see the restart rules below).
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

The [reference below](#rail-configuration-reference) owns the detailed contract
and is embedded with this skill; no checkout or external documentation is needed.
Preserve these configuration boundaries:

- Denies win; project config cannot authorize Docker/SSH-agent services or enable
  TTC's `SSH_AUTH_SOCK` inheritance. Socket/parent mounts cannot bypass service gates.
- Explain material access changes: shared networking exposes readable secrets;
  Docker can control the host and SSH-agent access permits key-backed signing.
- Do not broaden access or remove denies just to make a command succeed.

## Verify and report

Check JSON syntax without dumping sensitive contents, validate requested paths
and flags against the installed help/README, and inspect the final non-secret
diff. `ttc rail --list` shows live instances; it does **not** validate new config.
Rail validates configuration when creating an instance and fails closed; do not
launch an interactive sandbox merely as an unattended validation command.
Follow the restart rules below. Report
which file or launch command changed, its scope, checks performed, and the exact
restart/recreation step still needed. Do not claim a running instance adopted
new settings before it has been recreated.

## Rail configuration reference

Rail reads global `${XDG_CONFIG_HOME:-~/.config}/ttc/rail.json`, then
`<workdir>/ttc-rail.json`, with no ancestor search:

- Allow/deny lists combine; project allows replace global allows at the same
  destination. **Denies always win**, including over default imports.
- Config entrypoints must be regular, singly linked, nonsymlink files so rail can protect them
  read-only. JSON rejects unknown/duplicate fields, wrong types and trailing data;
  files are limited to 1 MiB.
- Each file has at most 256 combined `allow`/`deny` entries, counting services and
  duplicates. The merged policy has at most 256 combined mount/deny paths after
  exact-destination replacement/deduplication, before deny filtering. Defaults
  and generated protective overlays do not count.
- `allow` strings mount read-only at the same path. Objects require `source` and
  accept `dest` (default source) and `mode` (`ro` by default, or `rw`). Explicit
  sources must exist.
- `deny` strings name container paths; objects use `dest` or default it to
  `source`. Masks hide contents, including beneath writable mounts, not the same
  data at another independently allowed destination (except SSH-agent aliases).
- Relative paths resolve against the containing config's directory. Only `~`
  and `~/` expand; other shell expressions are literal. Paths overlapping `/proc`,
  `/dev` or `/run/ttc-rail`, and denies covering the entire workdir, are rejected.

### Default imports

- Workdir and existing TTC data/cache roots are writable. Host system paths and
  `/etc` are read-only; home, `/tmp` and `/run` are private. Networking is shared.
- Existing non-hidden `/etc/ssh/ssh_config.d/*.conf` files in the effective
  namespace are snapshotted read-only at their resolved targets with launcher
  ownership; symlinks stay intact. Sources must be regular files owned by root
  or the launcher and not group/other-writable. Denied drop-in files are empty,
  read-only launcher-owned masks; directory denies still hide the directory.
  Bounds: 256 directory entries, 1 MiB per file, 4 MiB total snapshot bytes.
  Recreate rail to refresh selected file contents. Directory membership and
  ancestor replacement remain live; nested/external SSH `Include` paths are not
  snapshotted.
  Launching from another user namespace can make root ownership appear as UID
  65534; that owner is not trusted. Launch from the host or explicitly import
  appropriately owned client-config fixtures; never change host system ownership.
- Existing Bash/Zsh startup files, `~/.gitconfig`, Zsh/tmux/Neovim config directories
  (both `~/.config` and XDG config home), TTC config and ancestor AGENTS/skills are
  read-only. An optional config directory naming the same underlying directory
  as a required writable root remains writable; mandatory rail configs stay read-only.
- Existing regular Zsh history is writable: the launcher's exported absolute
  `$HISTFILE` at instance creation, otherwise `~/.zsh_history`. A value set only in
  `.zshrc` does not select this import. Missing files are not created. The file bind
  cannot be atomically replaced; use `unsetopt HIST_SAVE_BY_COPY` for in-place saves.
- Configs sourcing other home files need explicit allows. Mounts still undergo
  service/deny audits; protected regular files with multiple hardlinks are rejected.

### Services and TTC's environment

Names in `authorize_services` are **global-only**. Authorization alone does not
enable a service; request it in either layer's `allow`. `deny` disables it.
Requests require authorization even when denied:

- `docker` mounts `/var/run/docker.sock` read-write. The shorthand supports only
  empty `DOCKER_HOST` or `unix:///var/run/docker.sock`.
- `ssh-agent` forwards the launcher's existing absolute `$SSH_AUTH_SOCK` to
  `/run/ssh-agent.sock` and sets `SSH_AUTH_SOCK` in rail shells. Without forwarding,
  rail unsets it. Reattachments retain the original socket; recreate rail if the
  host agent changes. A deny covering the host endpoint or forwarded destination
  also masks its known mount aliases and unsets the variable.

Any nonempty inherited `SSH_AUTH_SOCK` must be absolute, even without forwarding.
A missing absolute socket is permitted when forwarding is disabled.

Ordinary mounts exposing either endpoint or its parent require the enabled,
authorized service, even read-only. Socket access permits connections regardless
of mount writability. Multiply linked service sockets are unsupported and rejected
rather than recursively scanning host directories for aliases.

`ttc_allow_ssh_auth_sock` is a **global-only boolean**, default false. TTC removes
`SSH_AUTH_SOCK` at startup inside and outside rail unless it is true. This setting
does not enable forwarding or mount a socket. Environment removal does not stop
explicitly locating an accessible socket or using other SSH credentials.

### Examples

Global authorization, without enabling services everywhere:

```json
{
  "authorize_services": ["docker", "ssh-agent"],
  "ttc_allow_ssh_auth_sock": false,
  "allow": ["~/tools"],
  "deny": ["~/.ssh"]
}
```

Project mounts and enabled services:

```json
{
  "allow": [
    {"source": "/datasets", "dest": "/data"},
    {"source": "../results", "mode": "rw"},
    "docker",
    "ssh-agent"
  ],
  "deny": ["./secrets", {"dest": "/data/private"}]
}
```

### Restart rules

- Mount/service changes require stopping and recreating rail, not reattaching.
- TTC environment/web-search policy is read at TTC startup. Outside rail, restart
  TTC. Inside rail, recreation is also required if the host replaced an imported
  config file: a file bind keeps the old inode. In-place edits are visible, but TTC
  still needs a restart. Coordinate interruptions with the user.
