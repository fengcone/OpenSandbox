# execd — Go Execution Daemon

## Overview

HTTP daemon running inside sandbox containers. Handles shell command execution, filesystem operations, code interpreting via Jupyter kernels, and host metrics. Exposes a Gin-based API on port 44772.

## Structure

```
execd/
├── main.go                  # Entry point: CLI flags, Beego init, router setup
├── pkg/
│   ├── flag/                # CLI flag parsing (--jupyter-host, --port, etc.)
│   ├── web/
│   │   ├── controller/      # HTTP handlers (files, code, command, metrics)
│   │   └── model/           # Request/response models, SSE event types
│   ├── runtime/             # Command dispatch: routes to Jupyter or shell
│   │   ├── command.go       # Foreground command execution
│   │   ├── command_status.go # Background command tracking
│   │   ├── bash_session.go  # Bash session management
│   │   ├── interrupt.go     # Signal forwarding to process groups
│   │   ├── ctrl.go          # Runtime controller
│   │   └── env.go           # Environment variable handling
│   ├── jupyter/             # Jupyter client integration
│   │   ├── client.go        # HTTP + WebSocket client
│   │   ├── kernel/          # Kernel lifecycle (start, restart, interrupt)
│   │   ├── session/         # Session management
│   │   ├── execute/         # Code execution: request, parse, stream results
│   │   └── auth/            # Jupyter token auth
│   ├── log/                 # Structured logging (Beego logs wrapper)
│   └── util/                # Utilities (safe goroutine, glob helpers)
├── tests/                   # Manual test scripts (jupyter.sh, etc.)
└── Dockerfile               # Container image build
```

## Where to Look

| Task | File | Notes |
|------|------|-------|
| Add HTTP endpoint | `pkg/web/controller/` | Register route in `main.go` |
| Add execution backend | `pkg/runtime/` | Dispatch in `ctrl.go` |
| Jupyter kernel ops | `pkg/jupyter/kernel/` | Start, restart, interrupt kernels |
| Code execution parsing | `pkg/jupyter/execute/` | Stream parser, result types |
| SSE streaming | `pkg/web/controller/` | SSE helpers for code/command output |
| File operations | `pkg/web/controller/` | CRUD, glob search, chunked upload/download |
| CLI flag/config | `pkg/flag/` | All flags have env var equivalents |
| Build version info | `../internal/version/` | Injected via ldflags at build time |

## Conventions

- **HTTP framework**: Gin (`github.com/gin-gonic/gin`).
- **Logging**: Beego `logs` package (levels 0-7; default 6=Info).
- **Error handling**: Explicit error returns; no panic in handlers.
- **Build**: `make build` injects version/build-time/commit via ldflags into `internal/version`.
- **Go version**: 1.24. Uses `go.mod replace` for `../internal` shared package.
- **SSE**: Code and command output streamed via Server-Sent Events.
- **Graceful shutdown**: `--graceful-shutdown-timeout` (default 3s) keeps SSE alive for tail drain.

## Anti-Patterns

- Do NOT use `log.Fatal` or `os.Exit` outside `main.go`.
- Integration tests requiring real Jupyter are gated by env vars (`JUPYTER_URL`, `JUPYTER_TOKEN`).
- `pkg/jupyter/execute/zz_generated.deepcopy.go` is auto-generated — do not hand-edit.

## Commands

```bash
make build                      # build bin/execd with ldflags
make test                       # go test with coverage
make golint                     # golangci-lint
make multi-build                # cross-compile linux/windows/darwin amd64/arm64
```
