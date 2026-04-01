# OpenSandbox — Repository Knowledge Base

**Commit:** `134653b` | **Branch:** `main`

## Overview

General-purpose sandbox platform for AI applications. Multi-language monorepo: Python FastAPI control plane, Go execution daemon + ingress/egress proxies, Kubernetes operator, SDKs in 4 languages.

## Structure

```
opensandbox/
├── server/              # Python FastAPI control plane (sandbox lifecycle API)
├── components/
│   ├── execd/           # Go execution daemon (commands, files, code-interpreting via Jupyter)
│   ├── ingress/         # Go L7 reverse proxy (wildcard/URI/header routing to sandboxes)
│   ├── egress/          # Go DNS proxy + nftables egress filter sidecar
│   └── internal/        # Go shared packages (version, etc.) — consumed via go.mod replace
├── kubernetes/          # Go K8s operator (BatchSandbox, Pool CRDs, scheduler, task-executor)
├── sdks/
│   ├── sandbox/         # Sandbox lifecycle SDKs (python, kotlin, javascript, csharp)
│   ├── code-interpreter/# Code Interpreter SDKs (python, kotlin, javascript, csharp)
│   └── mcp/             # MCP integration (python)
├── specs/               # OpenAPI specs (execd-api.yaml, sandbox-lifecycle.yml)
├── examples/            # E2E examples (claude-code, playwright, chrome, RL training, etc.)
├── tests/               # Cross-component E2E tests (tests/python/, tests/java/)
├── sandboxes/           # Runtime sandbox container images
├── cli/                 # CLI tool (Python)
├── oseps/               # OpenSandbox Enhancement Proposals
├── docs/                # Architecture and design documentation
└── scripts/             # Dev automation scripts
```

## Where to Look

| Task | Location | Notes |
|------|----------|-------|
| Sandbox lifecycle API | `server/opensandbox_server/api/lifecycle.py` | REST endpoints |
| Docker/K8s runtime logic | `server/opensandbox_server/services/` | `factory.py` dispatches by runtime type |
| Sandbox execution (commands, files, code) | `components/execd/pkg/runtime/`, `pkg/web/controller/` | Gin HTTP API |
| Jupyter kernel integration | `components/execd/pkg/jupyter/` | Kernel/session lifecycle, WebSocket |
| K8s CRD types | `kubernetes/apis/sandbox/v1alpha1/` | BatchSandbox, Pool, SandboxSnapshot |
| K8s controller reconciliation | `kubernetes/internal/controller/` | BatchSandbox + Pool controllers |
| K8s scheduler/allocation | `kubernetes/internal/scheduler/` | Pool allocation logic |
| Ingress proxy routing | `components/ingress/pkg/proxy/` | HTTP + WebSocket reverse proxy |
| Egress DNS filtering | `components/egress/pkg/dnsproxy/` | DNS proxy with allow/deny |
| Egress nftables rules | `components/egress/pkg/nftables/` | CIDR/IP enforcement (dns+nft mode) |
| Python SDK public API | `sdks/sandbox/python/src/opensandbox/sandbox.py` | `Sandbox` / `SandboxSync` classes |
| SDK generated models | `sdks/sandbox/python/src/opensandbox/api/` | **DO NOT EDIT** — OpenAPI-generated |
| SDK handwritten adapters | `sdks/sandbox/python/src/opensandbox/adapters/` | Converter + transport logic |
| OpenAPI specs | `specs/execd-api.yaml`, `specs/sandbox-lifecycle.yml` | Source of truth for API contracts |
| E2E tests | `tests/python/tests/` | Cross-component integration |

## Conventions

- **Python**: `ruff` lint + format, `pyright` type-checking (standard mode). Server `line-length=100`, SDK `line-length=88`.
- **Go**: `gofmt`, `golangci-lint` (execd/ingress/egress/k8s each have own go.mod). Go 1.24.
- **Kotlin**: `ktlint`, Gradle Kotlin DSL. Multi-module with BOM.
- **Versioning**: Hatch VCS with tag-based versioning per component (`server/v*`, `python/sandbox/v*`).
- **Pre-commit**: trailing-whitespace, end-of-file-fixer, check-merge-conflict, check-yaml, detect-private-key.

## SDK Implementation Rules

- **Generated vs handwritten split**: `api/` is OpenAPI-generated; `adapters/` and `sync/` are handwritten.
- Use generated clients for standard request/response; handwritten transport only for SSE/streaming.
- **NEVER edit generated files** (`src/opensandbox/api/**`). Regenerate from specs first, then adapt.
- Streaming paths must align wire contracts with OpenAPI field names; cover with focused tests.

## Anti-Patterns

- `metadata` keys under `opensandbox.io/` prefix are system-reserved — user requests with these are rejected.
- `pause`/`resume` lifecycle APIs return `501` on Kubernetes runtime.
- Egress `dns` mode does NOT enforce CIDR/IP rules — must use `dns+nft` mode.
- Bridge network mode required for egress sidecar; host mode + networkPolicy is rejected.
- `timeout` must be ≥60s when specified; omit for manual-cleanup sandboxes.

## Commands

```bash
# Server
cd server && uv sync
cp opensandbox_server/examples/example.config.toml ~/.sandbox.toml
uv run python -m opensandbox_server.main          # or: opensandbox-server
uv run ruff check && uv run ruff format            # lint + format
uv run pytest --cov=opensandbox_server              # test with coverage

# execd
cd components/execd && make build                   # builds bin/execd with ldflags
make test                                           # go test with coverage
make golint                                         # golangci-lint

# Kubernetes operator
cd kubernetes && make install                       # install CRDs
make deploy CONTROLLER_IMG=... TASK_EXECUTOR_IMG=...# deploy controller
make docker-build                                   # build image

# Python SDK
cd sdks/sandbox/python && uv sync && uv run pytest
uv run ruff check && uv run ruff format

# Kotlin SDK
cd sdks/sandbox/kotlin && ./gradlew build

# Specs
node scripts/spec-doc/generate-spec.js              # regenerate spec docs
```

## Notes

- Server config at `~/.sandbox.toml` (env `SANDBOX_CONFIG_PATH` to override).
- execd runs inside sandbox containers on port 44772; Jupyter integration optional.
- Ingress supports 3 routing modes: wildcard, URI prefix, header-based.
- Egress sidecar shares network namespace with sandbox container; sandbox drops `NET_ADMIN`.
- K8s operator has BatchSandbox (O(1) batch delivery) vs Sig Agent-Sandbox (O(N)).
- Secure container runtimes supported: gVisor, Kata, Firecracker (K8s only).
- Experimental: auto-renew on access via `[renew_intent]` config + Redis.
