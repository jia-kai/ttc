.DEFAULT_GOAL := build

.PHONY: prompts build test check format format-check integration rail-integration demo math-test kitty-test ci-test full-test
# Share build prerequisites, but never run suites concurrently, even with -j.
.NOTPARALLEL: ci-test full-test
prompts:
	go run ./cmd/embed-prompts

format:
	git ls-files -z '*.md' '*.yaml' '*.yml' | xargs -0 npx --yes prettier@3.9.10 --write

format-check:
	git ls-files -z '*.md' '*.yaml' '*.yml' | xargs -0 npx --yes prettier@3.9.10 --check

build: prompts
	go build -o ttc ./cmd/ttc
test: prompts
	go test ./...
check: prompts
	go test -race ./...
	go vet ./...

integration: build
	python3 tests/pty_input.py
	python3 tests/pty_control_strings.py
	python3 tests/pty_history.py
	python3 tests/pty_parallel.py
	python3 tests/pty_compaction.py
	python3 tests/pty_compaction_reload.py
	python3 tests/pty_e2e.py
	python3 tests/pty_partial_retry.py
	python3 tests/pty_subagent.py --offline
	python3 tests/pty_subagent.py
	python3 tests/pty_subagent.py --failure
	python3 tests/demo.py
	python3 tests/demo.py --tui

demo: build
	python3 tests/demo.py --interactive

rail-integration: build
	python3 tests/pty_rail.py

kitty-test: build
	python3 tests/kitty_visual.py

math-test: build
	python3 tests/pty_math.py
	python3 tests/pty_math.py --tmux
	python3 tests/pty_math.py --disable-color
	python3 tests/pty_math.py --disable-color no-color
	python3 tests/pty_math.py --tmux --disable-color
	python3 tests/pty_math.py --tmux --disable-color no-color

# Prepare MathJax through startup before real-renderer unit/race tests run.
ci-test: math-test test check integration rail-integration
	python3 -m unittest discover -s tests -p 'test_*.py'

full-test: ci-test kitty-test
	python3 tests/kitty_visual.py --tmux
	python3 tests/kitty_visual.py --offline
	python3 tests/kitty_visual.py --offline --tmux
