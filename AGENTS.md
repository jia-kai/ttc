# TTC agent guidance

TTC is a coding agent for research workflows in the terminal. Read
`README.md` before changing product behavior. Keep this file aligned with the
features and constraints documented there as the project grows. Go is the
implementation language. The repository contains a runnable initial runtime and
specifications for remaining features. Linux hosts are the only supported
platform. See `docs/design.md` for module and persistence contracts.

## Product constraints

- The primary environment is a headless server accessed through SSH and tmux.
  Core workflows must work without a graphical desktop or browser.
- The interface is terminal native. Keep output readable in a terminal, and
  account for narrow panes, redirected output, and long-running sessions.

## Working in this repository

- Inspect the relevant code and documentation before editing. Follow the
  conventions already present in the files you touch.
- Keep changes scoped to the requested behavior. Update `README.md` when a
  change affects documented capabilities, setup, or supported environments.
- Validate changes with the checks available in the repository. For terminal
  behavior, exercise the affected workflow in a headless terminal when
  practical; for rendering changes, verify Kitty-specific behavior.

After significant coding work, ask separate, independent agents to review
correctness, implementation efficiency (including algorithm complexity),
architecture and design (whether the abstractions are reasonable, concerns
separated, whether there are duplicated functionalities that should be unified,
etc.), documentation accuracy, and historical burden (obsolete paths,
compatibility shims, redundant abstractions, and stale comments). Address their
findings and run relevant checks before declaring the work complete.
Documentation review must also check that final docs are concise, direct and
easy to read. Shorten repetition and unnecessary detail without losing useful
information, exact behavior, setup requirements or material limitations. Review
requirements coverage too: compare the implementation with user requests and
requirement documents, and flag missing functionality explicitly. Do not rewrite
requirements documents to catalog completed features unless requested.

## Coding guidelines

- **Simplicity over compatibility.** When changing interfaces or data formats,
  update callers and reject incompatible inputs explicitly. Do not add aliases,
  fallback paths, migration behavior, or compatibility shims unless requested.
- **Clean up as you go.** Remove obsolete branches, redundant wrappers, and
  stale documentation in code you change. Keep related callers and docs aligned.
- Use idiomatic Go types and small interfaces defined where they are consumed.
  Avoid type assertions when a concrete type or explicit interface suffices.
- Give exported Go declarations Go-style doc comments. Document behavior,
  side effects, units, return semantics, and non-obvious edge cases without
  restating the signature. Use inline comments to explain why.
- Document fields and parameters whose meaning is not clear from their names,
  including units, defaults, and ownership. Keep related documentation near
  the declaration.
- State array and tensor shapes with axis meanings, and include dtype, units,
  or coordinate frame when they affect the contract.
- Describe current behavior and exact semantics in documentation, rather than
  change history. For algorithms and data contracts, specify relevant units,
  probability meanings, state ownership, and edge-case behavior.
- **Fail loudly.** Return errors for missing or invalid required inputs. Handle
  errors deliberately, wrap them with useful context, and reserve panics for
  programmer errors or impossible internal states.
- **Program defensively.** Check cheap invariants when the same information
  has multiple representations. Avoid expensive repeated validation on hot
  paths unless it is needed at an external boundary or for correctness.
- Make goroutine lifetimes, cancellation, and ownership of background work
  explicit. Pass `context.Context` through operations that may block or outlive
  the current request, and clean up commands and timers when canceled.
- Use the repository's logging facilities for runtime messages rather than
  bare `print()` calls in library code. Choose levels that match the event.
- For non-trivial one-off analysis, write a patchable script in a temporary
  directory instead of embedding a long program in a shell command.
- Pad Markdown table columns so the source remains readable in a terminal.
- New tests should be self-contained and avoid machine-local paths or external
  datasets; embed small fixtures or track larger ones with the repository.
- Before reproducing a failure, save its complete exception output and useful
  context to a durable file instead of relying on terminal or tmux history.
- After coding, run `gofmt` on changed Go files and the relevant `go test` and
  `go vet` checks once a Go module exists.
