.DEFAULT_GOAL := build

.PHONY: prompts build test check integration demo kitty-test
prompts:
	go run ./cmd/embed-prompts

build: prompts
	go build -o ttc ./cmd/ttc
test: prompts
	go test ./...
check: prompts
	go test -race ./...
	go vet ./...

integration: build
	python3 tests/pty_input.py
	python3 tests/pty_history.py
	python3 tests/pty_parallel.py
	python3 tests/pty_compaction.py
	python3 tests/pty_e2e.py
	python3 tests/pty_subagent.py --offline
	python3 tests/pty_subagent.py
	python3 tests/demo.py
	python3 tests/demo.py --tui

demo: build
	python3 tests/demo.py --interactive

kitty-test: build
	python3 tests/kitty_visual.py
