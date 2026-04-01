# Server — Python FastAPI Control Plane

## Overview

FastAPI service managing sandbox lifecycle (create, start, pause, resume, delete) across Docker and Kubernetes runtimes.

## Structure

```
server/
├── opensandbox_server/
│   ├── main.py              # Uvicorn entry point, app factory
│   ├── cli.py               # CLI entry point (opensandbox-server command)
│   ├── config.py            # TOML config loading (server, runtime, docker, k8s, egress, ingress)
│   ├── api/
│   │   ├── lifecycle.py     # Sandbox CRUD endpoints (/v1/sandboxes)
│   │   ├── pool.py          # Pool-related endpoints
│   │   ├── proxy.py         # Server-side proxy to sandbox execd
│   │   └── schema.py        # Request/response schemas
│   ├── services/
│   │   ├── factory.py       # Runtime dispatch (docker vs kubernetes)
│   │   ├── docker.py        # Docker runtime implementation
│   │   ├── helpers.py       # Shared service utilities
│   │   ├── constants.py     # Service-layer constants
│   │   ├── endpoint_auth.py # Egress endpoint auth token handling
│   │   ├── extension_service.py
│   │   └── k8s/             # Kubernetes runtime sub-services
│   │       ├── client.py              # K8s client setup
│   │       ├── batchsandbox_provider.py  # BatchSandbox CRD provider
│   │       ├── agent_sandbox_provider.py # Agent-Sandbox CRD provider
│   │       └── *_template.py          # Pod/CRD YAML templates
│   ├── middleware/
│   │   ├── auth.py          # API key authentication (OPEN-SANDBOX-API-KEY header)
│   │   └── request_id.py    # Request ID middleware
│   ├── integrations/
│   │   └── renew_intent/    # Experimental: auto-renew on access (Redis-based)
│   └── extensions/          # Pydantic extensions, validation, codec
└── tests/                   # pytest test suite
    ├── test_docker_service.py
    └── k8s/                 # K8s runtime tests
```

## Where to Look

| Task | File | Notes |
|------|------|-------|
| Add REST endpoint | `api/lifecycle.py` | Register router in `main.py` |
| Add runtime backend | `services/factory.py` | Dispatch by `runtime.type` config |
| Docker container ops | `services/docker.py` | Create, start, stop, remove containers |
| K8s workload creation | `services/k8s/batchsandbox_provider.py` | CRD-based sandbox delivery |
| Config new setting | `config.py` | Add field to config dataclass + example.config.toml |
| Auth changes | `middleware/auth.py` | API key check; disabled when key empty |
| Proxy to sandbox | `api/proxy.py` | Reverse-proxy to sandbox execd port |
| Auto-renew feature | `integrations/renew_intent/` | OSEP-0009; Redis queue + consumer |

## Conventions

- **Config**: `~/.sandbox.toml` (env `SANDBOX_CONFIG_PATH` to override). `cli.py` generates with `init-config`.
- **Runtime dispatch**: `factory.py` creates the right service based on `runtime.type` ("docker" | "kubernetes").
- **Lifecycle states**: Pending → Running → Paused → Stopping → Terminated/Failed. State machine enforced in service layer.
- **Type checking**: `pyright` standard mode. Server `line-length=100` (wider than SDK's 88).
- **Testing**: `pytest` with `pytest-asyncio`. Async tests use `asyncio_mode = "auto"`.

## Anti-Patterns

- `metadata` keys prefixed `opensandbox.io/` are rejected in create requests — system-reserved.
- `timeout` must be ≥60s. Omit for manual-cleanup sandboxes (no auto-expiry).
- Bridge mode required when using egress sidecar. Host mode + `networkPolicy` is rejected.
- `pause`/`resume` return `501` on Kubernetes runtime.
- `max_sandbox_timeout_seconds` caps TTL; server rejects exceeding requests.

## Commands

```bash
uv sync                                                        # install deps
uv run python -m opensandbox_server.main                       # start server
opensandbox-server init-config ~/.sandbox.toml --example docker # generate config
uv run ruff check && uv run ruff format                        # lint + format
uv run pytest --cov=opensandbox_server                          # test
```
