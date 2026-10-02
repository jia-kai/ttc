#!/usr/bin/env python3
"""Deterministic stdio LSP fixture. No network, packages, or language index."""
import json
import os
import sys

encoding = os.environ.get("TTC_LSP_ENCODING", "utf-16")
mode = os.environ.get("TTC_LSP_MODE", "normal")
documents = {}
pending = None
requests_sent = False


def send(message):
    data = json.dumps(message, ensure_ascii=False).encode()
    sys.stdout.buffer.write(f"Content-Length: {len(data)}\r\n\r\n".encode())
    # Fragment the payload to exercise reads across transport boundaries.
    mid = len(data) // 2
    sys.stdout.buffer.write(data[:mid])
    sys.stdout.buffer.flush()
    sys.stdout.buffer.write(data[mid:])
    sys.stdout.buffer.flush()


def reply(identifier, result):
    send({"jsonrpc": "2.0", "id": identifier, "result": result})


def trace(message):
    print(message, file=sys.stderr, flush=True)


while True:
    headers = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            sys.exit(0)
        if line == b"\r\n":
            break
        name, value = line.decode().strip().split(":", 1)
        headers[name.lower()] = value.strip()
    data = sys.stdin.buffer.read(int(headers["content-length"]))
    message = json.loads(data)
    method = message.get("method")
    identifier = message.get("id")
    params = message.get("params", {})
    if method == "initialize":
        if mode == "bad_frame":
            sys.stdout.buffer.write(b"Content-Length: 9000000\r\n\r\n")
            sys.stdout.buffer.flush()
            continue
        caps = {"positionEncoding": encoding, "textDocumentSync": {"openClose": True, "change": 2},
                "definitionProvider": True, "referencesProvider": True, "hoverProvider": True,
                "documentSymbolProvider": True, "workspaceSymbolProvider": {"resolveProvider": True}}
        if mode == "unsupported":
            caps["hoverProvider"] = False
        reply(identifier, {"capabilities": caps})
        trace("initialized once")
    elif method == "initialized":
        # String server IDs deliberately differ from the client's numeric IDs.
        send({"jsonrpc": "2.0", "id": "config", "method": "workspace/configuration", "params": {"items": [{"section": "fixture"}]}})
        send({"jsonrpc": "2.0", "id": "folders", "method": "workspace/workspaceFolders", "params": {}})
        send({"jsonrpc": "2.0", "id": "edit", "method": "workspace/applyEdit", "params": {"edit": {}}})
        send({"jsonrpc": "2.0", "id": "unknown", "method": "unsupported/request", "params": {}})
    elif method is None:
        trace("server reply " + str(identifier) + " " + json.dumps(message))
    elif method == "textDocument/didOpen":
        doc = params["textDocument"]
        documents[doc["uri"]] = doc["text"]
        trace("opened " + str(doc["version"]))
    elif method == "textDocument/didChange":
        doc = params["textDocument"]
        changes = params["contentChanges"]
        if "range" not in changes[0]:
            trace("missing incremental range")
        documents[doc["uri"]] = changes[0]["text"]
        trace("changed " + str(doc["version"]))
    elif method == "textDocument/didClose":
        documents.pop(params["textDocument"]["uri"], None)
        trace("closed")
    elif method == "$/cancelRequest":
        trace("cancelled " + str(params["id"]))
        # A late canceled response must not complete the next request.
        reply(params["id"], {"contents": "late canceled response"})
    elif method == "textDocument/hover":
        if mode == "slow" and pending is None:
            pending = identifier
            trace("slow request received")
            continue
        if mode == "error":
            send({"jsonrpc": "2.0", "id": identifier, "error": {"code": -32602, "message": "use a valid path and position\u001b[31m"}})
            continue
        trace("position " + json.dumps(params["position"], sort_keys=True))
        text = documents[params["textDocument"]["uri"]]
        # A foreign response ID must be ignored.
        reply(1234567, {"contents": "foreign reply"})
        reply(identifier, {"contents": {"kind": "markdown", "value": "**fixture** " + text}})
    elif method in ("textDocument/definition", "textDocument/references"):
        uri = params["textDocument"]["uri"]
        end = {"utf-8": 7, "utf-16": 4, "utf-32": 3}[encoding]
        span = {"start": {"line": 0, "character": 1}, "end": {"line": 0, "character": end}}
        if method.endswith("definition"):
            reply(identifier, [{"targetUri": uri, "targetRange": span, "targetSelectionRange": span}] * 3)
        else:
            reply(identifier, [{"uri": uri, "range": span}] * 3)
    elif method == "textDocument/documentSymbol":
        span = {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 1}}
        reply(identifier, [{"name": "parent", "kind": 5, "range": span, "selectionRange": span,
                            "children": [{"name": "child", "kind": 12, "range": span, "selectionRange": span}]}])
    elif method == "workspace/symbol":
        if mode == "resolve":
            reply(identifier, [{"name": "resolved", "kind": 12, "location": {"uri": next(iter(documents))}}])
        else:
            reply(identifier, [])
    elif method == "workspaceSymbol/resolve":
        symbol = dict(params)
        symbol["location"]["range"] = {"start": {"line": 0, "character": 0}, "end": {"line": 0, "character": 1}}
        reply(identifier, symbol)
    elif identifier is not None:
        send({"jsonrpc": "2.0", "id": identifier, "error": {"code": -32601, "message": "method not found"}})
