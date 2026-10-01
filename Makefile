.PHONY: build test check integration demo kitty-test
build:
	go build -o ttc ./cmd/ttc
test:
	go test ./...
check:
	go test -race ./...
	go vet ./...

integration: build
	python3 tests/pty_input.py
	python3 tests/pty_compaction.py
	python3 tests/pty_e2e.py
	python3 tests/demo.py
	python3 tests/demo.py --tui

demo: build
	python3 tests/demo.py --interactive

kitty-test: build
	python3 tests/kitty_visual.py
