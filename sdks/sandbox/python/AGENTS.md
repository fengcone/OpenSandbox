# Python SDK

## Overview

Async + sync Python SDK for OpenSandbox lifecycle management. Uses OpenAPI-generated clients for standard endpoints; handwritten adapters for SSE streaming and sync wrappers.

## Structure

```
sdks/sandbox/python/
├── src/opensandbox/
│   ├── __init__.py          # Public exports (Sandbox, SandboxSync, models)
│   ├── sandbox.py           # Sandbox / SandboxSync classes (main public API)
│   ├── manager.py           # SandboxManager (admin: list, kill)
│   ├── config.py            # ConnectionConfig / ConnectionConfigSync
│   ├── exceptions.py        # SandboxException with error code + request ID
│   ├── api/                 # ⚠️ GENERATED — do not edit
│   │   ├── lifecycle/       # Lifecycle API client, models
│   │   └── execd/           # Execd API client, models
│   ├── adapters/            # Handwritten converter + transport
│   │   └── converter/       # Model converters (generated ↔ SDK types)
│   └── sync/                # Sync wrappers over async implementation
│       └── adapters/        # Sync transport adapters
├── tests/                   # pytest suite (asyncio_mode = "auto")
├── scripts/                 # Code generation scripts
├── pyproject.toml           # ruff (line-length=88), pyright, pytest config
└── Makefile                 # generate, lint, test targets
```

## Where to Look

| Task | File | Notes |
|------|------|-------|
| Add public method | `sandbox.py` | Add to both `Sandbox` (async) and `SandboxSync` |
| Add model conversion | `adapters/converter/` | Map between generated API models and SDK types |
| Add SSE/streaming path | Handwritten transport in `adapters/` | NOT in `api/` |
| Add generated endpoint | Regenerate from `specs/` → `api/` | **Never edit `api/` directly** |
| Config new option | `config.py` | `ConnectionConfig` (async) / `ConnectionConfigSync` |
| Error handling | `exceptions.py` | `SandboxException` with `.error.code`, `.request_id` |
| Tests | `tests/` | Follow `test_*.py` naming; use `pytest-asyncio` |

## Conventions

- **Generated vs handwritten**: `api/` = generated from OpenAPI specs; `adapters/` + `sync/` = handwritten.
- **Ruff**: line-length=88 (narrower than server's 100). `api/` excluded from ruff via config.
- **Pyright**: standard mode. `api/` excluded from type checking.
- **Versioning**: Hatch VCS, tag pattern `python/sandbox/v*`.
- **Testing**: `pytest` with `asyncio_mode = "auto"`. All async tests auto-detected.
- **SDK entry point**: `Sandbox.create()` (async) / `SandboxSync.create()` (sync).

## Anti-Patterns

- **NEVER edit files in `src/opensandbox/api/`** — always regenerate from specs.
- Do not suppress type errors (`as any`, `# type: ignore`) in handwritten code.
- `metadata` keys with `opensandbox.io/` prefix are rejected by server.
- Do not create SDK models that duplicate generated ones — import from `api/` instead.
- Streaming paths must keep wire contracts aligned with OpenAPI field names.

## Commands

```bash
uv sync                      # install deps
uv run pytest                # run tests
uv run ruff check && ruff format  # lint + format
make generate                # regenerate from OpenAPI specs (if Makefile target exists)
```
