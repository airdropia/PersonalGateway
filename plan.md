# pgw — Native Go, Single Binary, OpenAI-Compatible

## 1. Project Objective

Build an independent, public, personal-use AI gateway. The product is a single
native Windows amd64 executable with an embedded dashboard. The gateway speaks
the OpenAI-compatible HTTP API and treats every upstream provider as an
OpenAI-compatible endpoint behind a user-supplied `base_url` and `api_key`.

Goals:

- Single Go binary that ships with everything: dashboard, SQLite driver, all
  HTTP routes, all adapters.
- No CGo, no Electron, no Node.js, no external runtime required on the host.
- Operator-friendly first run: drop the `.exe`, double-click, browser opens
  to the dashboard at `http://127.0.0.1:8080/admin/dashboard`, no YAML edit
  required, no pre-configured provider required.
- Provider model is uniform: every configured upstream is one
  OpenAI-compatible adapter, identified by a user-chosen display name. The
  provider registry collapses to a single generic adapter.
- Per-provider model catalog is auto-discovered from `GET {base_url}/v1/models`
  at provider-add time, then user-curated by ticking desired models.
- Per-model metadata (context window, max output tokens, supported features)
  is parsed from the `/v1/models` response where the provider publishes it
  (using smart heuristics across common field names) and is always editable
  from the dashboard.
- Server lifecycle is operator-controlled: start, stop, restart, quit — from
  the terminal, the dashboard, or an optional Windows tray icon.
- All build, test, package, and verification happens on GitHub Actions. The
  local Windows workstation is only used for source editing and orchestration.

This is a personal-use product fork. It does not silently claim enterprise
support, multi-tenant isolation, team billing, or feature parity with
upstream GoModel after removal.

---

## 2. Final Architectural Decisions

### 2.1 Provider model = single OpenAI-compatible adapter

The gateway accepts one provider shape only:

```text
Provider
  id           = stable internal id (uuid)
  display_name = user-chosen name (e.g. "OpenAI", "Work Anthropic")
  base_url     = https://api.openai.com/v1
  api_key      = user secret, stored encrypted at rest
  enabled      = bool
```

Every configured provider is an instance of one internal generic adapter.
There are no provider-specific packages. There is no per-provider routing
logic. There is no per-provider response shape normalization beyond the
OpenAI standard.

### 2.2 Auto-discovery on provider add

When the operator adds a provider, the gateway:

1. Calls `GET {base_url}/v1/models` with `Authorization: Bearer {api_key}`.
2. Parses the OpenAI standard response shape (`object: "list"`, `data: [...]`,
   each item has `id`, `object: "model"`, `owned_by`, optional `created`).
3. Stores every returned model id as a `discovered` row, initially `enabled =
   false` (unticked) so the dashboard does not flood with hundreds of models.
4. The operator ticks the models they want on the Models page or the Provider
   page; everything else stays hidden from both the dashboard and
   `GET /v1/models`.
5. If the response is paginated (the response body includes a cursor or a
   next-page hint), the gateway offers a "Load more" button in the dashboard
   that issues the next request; auto-pagination is opt-in, never silent.

### 2.3 Per-model metadata

The `/v1/models` response is the primary source. The gateway uses smart
heuristics for common non-standard fields:

| Heuristic key candidates                       | Maps to                |
|-----------------------------------------------|------------------------|
| `max_context_length`, `context_length`,       | `context_window`       |
| `context_window`, `max_input_tokens`          |                        |
| `max_tokens`, `max_output_tokens`,            | `max_output_tokens`    |
| `max_completion_tokens`                       |                        |
| `capabilities.tools`, `supports.tools`,       | `supports_tools`       |
| `tools`, `function_calling`                   |                        |
| `capabilities.vision`, `supports.vision`,     | `supports_vision`      |
| `vision`, `multimodal`                        |                        |
| `capabilities.streaming`,                     | `supports_streaming`   |
| `stream`, `streaming`                         |                        |
| `pricing.prompt`, `price_input`,              | `input_price_per_1m`   |
| `input_cost`                                  |                        |
| `pricing.completion`, `price_output`,         | `output_price_per_1m`  |
| `output_cost`                                 |                        |

Anything the heuristics miss is editable from the dashboard on a per-model
basis (a single "Edit metadata" action per row). Manual edits always win over
auto-discovered values and are persisted as user overrides.

### 2.4 Database hygiene

Every mutating action on a provider, model, credential, preference, or
usage row follows these rules:

- SQLite foreign keys are enabled at every connection (`PRAGMA
  foreign_keys = ON`).
- Cascading deletes are explicit and exhaustive: removing a provider removes
  its credentials, model rows, user metadata overrides, and any usage rows
  that reference only that provider.
- A periodic vacuum job (controlled by `storage.vacuum_interval_hours`,
  default 168 = weekly) runs `PRAGMA incremental_vacuum` to reclaim space.
- Orphaned rows are impossible because every foreign key has `ON DELETE
  CASCADE` and every insert validates its parent.

### 2.5 Server lifecycle

Three shutdown paths, all equivalent:

- Terminal: `Ctrl+C` sends SIGINT to the binary.
- Dashboard: a "Stop Server" button on the Settings page issues an
  authenticated `POST /admin/stop` that calls `app.Shutdown(ctx)` with a
  bounded grace period.
- Tray: an optional tray icon (started with `--tray`) exposes "Open
  dashboard", "Stop gateway", "Open data folder", "Exit". Tray mode is
  optional, opt-in, and never starts the gateway automatically — it only
  controls an already-running gateway.

The binary accepts these CLI flags:

```text
--help                 show usage
--version              print version + commit + build date
--config <path>        load config.yaml (default: next to exe, then cwd)
--data-dir <path>      SQLite + uploads dir (default: ./data)
--port <port>          override server.port
--tray                 launch in tray mode (requires a running gateway)
--stop                 ask the running gateway to exit (PID file based)
```

### 2.6 Single-binary deployment

The shipped artifact is one `pgw.exe` with:

- All HTTP routes compiled in.
- SQLite via `modernc.org/sqlite` (pure Go, no CGo).
- Dashboard assets embedded via `go:embed`.
- All assets either embedded or written under `--data-dir` (default
  `./data`).

There is no `config.yaml` required for the first run. There is no `.env`
required. The dashboard is the configuration surface.

### 2.7 Upstream decoupling

The `upstream` remote (ENTERPILOT/GoModel) is removed from this repository.
The reference points in `plan.md`, `README.md`, and `AGENTS.md` that point
at upstream are rewritten to point at this fork's own docs. There is no
ongoing sync job and no expectation of upstream compatibility.

---

## 3. Repository State and Remotes

### 3.1 Current remotes

```text
origin   = this fork's public repository
upstream = ENTERPILOT/GoModel   (to be removed)
```

### 3.2 Remote changes

- Remove the `upstream` remote: `git remote remove upstream`.
- Add a new `upstream` remote pointing at this fork's public mirror, or
  drop the concept entirely if there is no mirror.
- `origin` stays the sole push target.

### 3.3 Module path

The current `go.mod` declares `module github.com/enterpilot/gomodel`.
Stage 7 migrates the module path to the fork's own path
(`github.com/<owner>/pgw`) and updates every internal import.
Until then, the upstream path is kept to make the deletion work easier.

---

## 4. Scope: What This Plan Removes

The following are deleted from the source tree. Removal is total: package
directory, all imports, all references in `internal/app`, all admin handler
files, all dashboard pages, all tests.

### 4.1 Provider packages to delete

All33 packages under `internal/providers/` are removed. A single new
package `internal/provider` replaces them.

```text
internal/providers/anthropic/
internal/providers/azure/
internal/providers/bailian/
internal/providers/bedrock/
internal/providers/bedrockmantle/
internal/providers/chatgpt/
internal/providers/chutes/
internal/providers/cohere/
internal/providers/deepseek/
internal/providers/elevenlabs/
internal/providers/fireworks/
internal/providers/gemini/
internal/providers/googlecommon/
internal/providers/groq/
internal/providers/health/
internal/providers/hetzner/
internal/providers/kilo/
internal/providers/kimicode/
internal/providers/llamacpp/
internal/providers/llmd/
internal/providers/meta/
internal/providers/minimax/
internal/providers/ollama/
internal/providers/openai/
internal/providers/opencodego/
internal/providers/openrouter/
internal/providers/oracle/
internal/providers/sglang/
internal/providers/vertex/
internal/providers/vllm/
internal/providers/xai/
internal/providers/xiaomi/
internal/providers/zai/
```

### 4.2 Subsystems to delete

```text
internal/anthropicapi/
internal/auditlog/
internal/authkeys/
internal/batch/
internal/batchrewrite/
internal/budget/
internal/codeximport/
internal/codexoauth/
internal/conversationstore/
internal/embedding/
internal/filestore/
internal/guardrails/
internal/live/
internal/mcpgateway/
internal/observability/
internal/pricingoverrides/
internal/ratelimit/
internal/realtime/
internal/responsestore/
internal/runtimesettings/
internal/session/
internal/tagging/
internal/usage/
internal/versioncheck/
internal/workflows/
```

### 4.3 Features to delete

- All `/v1/embeddings`, `/v1/audio/*`, `/v1/files`, `/v1/batches`,
  `/v1/messages/batches`, `/v1/realtime*`, `/v1/responses*` endpoints
  except `/v1/chat/completions` and `/v1/models`.
- All `/admin/audit*`, `/admin/budgets*`, `/admin/rate-limits*`,
  `/admin/workflows*`, `/admin/guardrails*`, `/admin/mcp*`,
  `/admin/version-check*`, `/admin/auth-keys*`,
  `/admin/codex*` (import + oauth), `/admin/usage*` endpoints except
  `/admin/runtime/config`, `/admin/providers`, `/admin/models`,
  `/admin/models/{id}/metadata`, `/admin/stop`, `/admin/restart`.
- The Playwright/E2E/Contract test suites under `tests/`.
- The Helm chart under `helm/`.
- `Dockerfile`, `docker-compose.yaml`, `prometheus.yml`.
- `ext/` package — not needed because there is no third-party extension
  surface.

---

## 5. Scope: What This Plan Keeps

### 5.1 Core HTTP server

`internal/server/` is kept and slimmed down. The server keeps:

- `/v1/chat/completions` (POST, streaming + non-streaming)
- `/v1/models` (GET)
- `/healthz`, `/readyz`
- `/admin/dashboard/*` (embedded SPA)
- `/admin/runtime/config`
- `/admin/providers` (CRUD)
- `/admin/providers/{id}/discover` (POST, triggers `/v1/models` fetch)
- `/admin/providers/{id}/models` (GET, list with tick state)
- `/admin/models` (GET, list)
- `/admin/models/{id}` (PATCH for metadata overrides, DELETE)
- `/admin/models/{id}/tick` (POST, toggle enabled)
- `/admin/stop`, `/admin/restart`
- `/v1/auth/keys` — a single master key, no per-user key hierarchy

### 5.2 Storage

`internal/storage/` is kept but only the SQLite backend. PostgreSQL and
MongoDB backends are removed. The driver is `modernc.org/sqlite` (pure Go,
CGo-free).

### 5.3 Config

`config/config.go` and `config/pgw.example.yaml` are kept. The
defaults are rewritten to match this plan's scope:

```text
server.port                  = 8080
server.body_size_limit       = 10M
server.master_key            = random on first boot, persisted to data/secret.key
storage.type                 = sqlite
storage.sqlite.path          = data/pgw.db
storage.vacuum_interval_hours = 168
usage.enabled                = false       # removed; see §5.4
models.enabled_by_default    = false       # auto-discovery is opt-in tick
resilience.retry             = on
resilience.circuit_breaker   = on
```

### 5.4 Usage tracking

The `usage` subsystem is removed. The dashboard keeps a single
`RequestLog` table for the last N requests (default 1000) so the operator
can see recent activity, but there is no per-request cost accounting, no
pricing recalculation, no monthly aggregation, no budget enforcement.

### 5.5 Virtual models

`internal/virtualmodels/` is kept. Virtual models (aliases, redirects,
weighted round-robin, failover chains) remain because they are useful for
a pgw that hits multiple providers.

### 5.6 Model preferences (hide/show)

The `internal/modelpreferences/` package is kept because it underpins the
"tick to enable" UX on the Models page. Its semantics change:

- `enabled` (new boolean) controls whether the model is routable and
  appears in `/v1/models`.
- `hidden` (old boolean) is removed; hidden is the default for unticked
  models, so the field is redundant.

### 5.7 Dashboard

`web/dashboard/` is rebuilt around five pages: Overview, Providers, Models,
Playground, Settings. The other six pages (Usage, API Keys, Budgets, Rate
Limits, Workflows, Guardrails, MCP Servers, Audit Logs) are removed
because their backing subsystems are removed.

The dashboard is rebuilt with SvelteKit + Svelte 5 runes, compiled to
static assets, and embedded via `go:embed`.
---

## 6. Provider Model: Single Generic Adapter

### 6.1 Storage shape

```text
providers
  id             TEXT PRIMARY KEY
  display_name   TEXT NOT NULL UNIQUE
  base_url       TEXT NOT NULL
  api_key_ref    TEXT NOT NULL   -- FK -> secrets(id)
  enabled        INTEGER NOT NULL DEFAULT 1
  last_sync_at   TIMESTAMP
  created_at     TIMESTAMP NOT NULL
  updated_at     TIMESTAMP NOT NULL

secrets
  id             TEXT PRIMARY KEY
  ciphertext     BLOB NOT NULL   -- AES-GCM with KEK from data/secret.key
  created_at     TIMESTAMP NOT NULL

provider_models
  provider_id    TEXT NOT NULL   -- FK -> providers(id) ON DELETE CASCADE
  model_id       TEXT NOT NULL   -- upstream id, e.g. "gpt-4o-mini"
  enabled        INTEGER NOT NULL DEFAULT 0
  context_window INTEGER         -- nullable; null = unknown
  max_output_tokens INTEGER     -- nullable
  supports_tools INTEGER NOT NULL DEFAULT 0
  supports_vision INTEGER NOT NULL DEFAULT 0
  supports_streaming INTEGER NOT NULL DEFAULT 1
  input_price_per_1m REAL       -- nullable
  output_price_per_1m REAL      -- nullable
  user_overrides JSON           -- nullable; merges on top of heuristics
  PRIMARY KEY (provider_id, model_id)
```

### 6.2 Generic adapter

```go
package provider

type Adapter struct {
    id          string
    displayName string
    baseURL     string
    apiKey      string
    httpClient  *http.Client
}

func (a *Adapter) ListModels(ctx context.Context) ([]DiscoveredModel, error)
func (a *Adapter) ChatCompletion(ctx context.Context, req ChatRequest, w io.Writer) error
func (a *Adapter) Ping(ctx context.Context) error
```

All three methods follow the OpenAI standard exactly. The chat completion
implementation proxies the request body unchanged and returns the upstream
response body (parsed for SSE if `stream: true`). No provider-specific
shimming.

### 6.3 Routing

Routing is "find one enabled provider model whose `model_id` matches the
request and whose provider is `enabled`". If no match, return 404. There is
no failover chain logic at the provider level; failover chains live in
`virtualmodels` as before.

### 6.4 Self-test endpoints

The Providers page calls `GET /admin/providers/{id}/discover` to trigger
discovery and report status:

```text
200 OK
{
  "provider": { ... },
  "discovered": [
    { "model_id": "gpt-4o-mini", "context_window": 128000, ... },
    ...
  ],
  "errors": []
}

502 Bad Gateway
{
  "error": "upstream returned 401",
  "hint": "check api_key"
}
```

---

## 7. Custom Provider UX

### 7.1 Add provider flow

```text
1. Operator opens the Providers page, clicks "Add provider".
2. Modal asks for:
   - Display name (required, unique)
   - Base URL (required, must end with /v1 or be OpenAI-compatible)
   - API key (required, write-only field)
3. On submit, the gateway:
   a. Creates the provider row (api_key_ref pointing at a new secrets row).
   b. Calls GET {base_url}/v1/models with the api key.
   c. On success, populates provider_models with one row per discovered id,
      all enabled=0.
   d. On failure, creates the provider but leaves provider_models empty
      and shows the error on the Providers page; the operator can retry
      discover.
4. The operator ticks the models they want on the Models page.
```

### 7.2 Edit / delete provider

- Edit: change `display_name`, `base_url`, `api_key`, `enabled`. Changing
  `base_url` invalidates `provider_models` and triggers a fresh discovery
  on save.
- Delete: confirms via typed name. Cascades to `secrets`,
  `provider_models`. Any virtual model that targets a deleted provider's
  model is left in place but flagged as `unavailable` in the dashboard.

### 7.3 Refresh model list

A "Refresh models" button on the Providers row calls
`POST /admin/providers/{id}/discover` again. The result is diffed against
the current `provider_models`:
- New ids are added as `enabled=0`.
- Removed ids are deleted from `provider_models` and from any virtual
  model's target list (the virtual model is left but marked unavailable).
- Existing ids have their heuristic metadata re-parsed only if the operator
  has not set a `user_overrides` value for that field.

### 7.4 Per-model metadata editor

The Models page has a "Edit metadata" action on each row. The modal
exposes:

```text
display_name           (optional human label; cosmetic)
context_window         (integer or empty)
max_output_tokens      (integer or empty)
supports_tools         (checkbox)
supports_vision        (checkbox)
supports_streaming     (checkbox)
input_price_per_1m     (number or empty)
output_price_per_1m    (number or empty)
```

Every field has a "Reset to discovered" button next to it. When the
operator types a value, the row is saved immediately (autosave on blur).
Manual values are stored in `user_overrides` JSON, never in the heuristic
fields. Refresh never overwrites manual values.

---

## 8. Dashboard Pages (Final)

### 8.1 Page inventory

| Route                      | Purpose                                                    |
|----------------------------|------------------------------------------------------------|
| `/admin/dashboard/overview` | Status, recent requests, server uptime, configured providers count, enabled model count |
| `/admin/dashboard/providers` | List, add, edit, delete, refresh, per-row model count    |
| `/admin/dashboard/models`   | All enabled models across all providers; tick/untick; edit metadata; per-model "test in playground" link |
| `/admin/dashboard/playground` | Single-model chat tester, streams responses, shows token estimate |
| `/admin/dashboard/settings` | Master key rotation, data folder path, vacuum interval, port, stop server, view logs |

### 8.2 Removed pages

The following pages are deleted from `web/dashboard/src/pages/` along with
their navigation entry:

```text
audit-logs/        usage/           auth-keys/
budgets/           rate-limits/     workflows/
guardrails/        mcp-servers/
```

### 8.3 Navigation

```js
export const NAV_ITEMS = [
  { page: "overview",  label: m.navigation_overview,  icon: LayoutDashboard },
  { page: "providers", label: m.navigation_providers, icon: ServerCog },
  { page: "models",    label: m.navigation_models,    icon: Box },
  { page: "playground",label: m.navigation_playground,icon: FlaskConical },
  { page: "settings",  label: m.navigation_settings,  icon: Settings,
    notify: () => versionStore.updateAvailable },
];
```

Settings is the only page with a notification dot, and only when a newer
release exists in the same-channel manifest.

### 8.4 Dashboard route map

```text
/                            -> redirects to /admin/dashboard
/admin/dashboard             -> SPA index.html (embedded)
/admin/dashboard/*           -> SPA handles in-app routing
/admin/runtime/config        -> JSON config flags for the SPA
/admin/providers             -> JSON list (CRUD)
/admin/providers/{id}        -> JSON
/admin/providers/{id}/discover -> POST: refresh + diff
/admin/models                -> JSON list
/admin/models/{id}           -> PATCH / DELETE
/admin/stop                  -> POST: graceful shutdown
/admin/restart               -> POST: stop + respawn same binary
/v1/chat/completions         -> OpenAI standard
/v1/models                   -> OpenAI standard
/healthz, /readyz            -> liveness / readiness
```

---

## 9. Subsystem Deletion Plan

### 9.1 Order of operations

Deletion happens in this order so the build stays green at each commit:

1. Delete `internal/provider/<old>` packages and their imports in
   `internal/app/app.go`. Keep `internal/providers/openai/` last so the
   build never breaks.
2. Add the new `internal/provider` package and switch `run/providers.go`
   to register only the new generic adapter.
3. Delete `internal/codexoauth/`, `internal/codeximport/`,
   `internal/providers/chatgpt/`, all admin handler files that touch them.
4. Delete `internal/usage/`, `internal/auditlog/`,
   `internal/pricingoverrides/`, the corresponding admin handler files,
   and the SQL migrations.
5. Delete `internal/live/`, `internal/authkeys/`,
   `internal/conversationstore/`, `i

---

## 10. Storage Layout

### 10.1 Driver

`modernc.org/sqlite`, registered as a `database/sql` driver. Pure Go, no
CGo, no external `.dll`.

### 10.2 Pragmas at every connection

```sql
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = MEMORY;
```

### 10.3 Tables

```sql
CREATE TABLE providers (
  id           TEXT PRIMARY KEY,
  display_name TEXT NOT NULL UNIQUE,
  base_url     TEXT NOT NULL,
  api_key_ref  TEXT NOT NULL REFERENCES secrets(id) ON DELETE RESTRICT,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_sync_at TIMESTAMP,
  created_at   TIMESTAMP NOT NULL,
  updated_at   TIMESTAMP NOT NULL
);

CREATE TABLE secrets (
  id         TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL,
  created_at TIMESTAMP NOT NULL
);

CREATE TABLE provider_models (
  provider_id          TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  model_id             TEXT NOT NULL,
  enabled              INTEGER NOT NULL DEFAULT 0,
  context_window       INTEGER,
  max_output_tokens    INTEGER,
  supports_tools       INTEGER NOT NULL DEFAULT 0,
  supports_vision      INTEGER NOT NULL DEFAULT 0,
  supports_streaming   INTEGER NOT NULL DEFAULT 1,
  input_price_per_1m   REAL,
  output_price_per_1m  REAL,
  user_overrides       TEXT,
  discovered_at        TIMESTAMP NOT NULL,
  updated_at           TIMESTAMP NOT NULL,
  PRIMARY KEY (provider_id, model_id)
);

CREATE TABLE virtual_models (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  strategy    TEXT NOT NULL,         -- alias | round_robin | cost | failover
  targets     TEXT NOT NULL,         -- JSON
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL
);

CREATE TABLE request_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts            TIMESTAMP NOT NULL,
  provider_id   TEXT REFERENCES providers(id) ON DELETE SET NULL,
  model_id      TEXT,
  status        INTEGER NOT NULL,
  duration_ms   INTEGER NOT NULL,
  prompt_tokens INTEGER,
  output_tokens INTEGER,
  error         TEXT
);
CREATE INDEX request_log_ts_idx ON request_log(ts);

CREATE TABLE kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
```

`kv` holds non-secret runtime settings (vacuum interval, last version
check timestamp, dashboard preferences).

### 10.4 Encryption at rest

The KEK is generated on first boot and stored at `data/secret.key` with
`0600` permissions on POSIX and equivalent ACL on Windows. The KEK encrypts
each `secrets.ciphertext` row with AES-GCM. Losing `secret.key` means
re-entering every provider's API key; this is documented in the README and
on the Settings page.

### 10.5 Vacuum

A background goroutine runs every `vacuum_interval_hours` and issues
`PRAGMA incremental_vacuum`. The vacuum is skipped while requests are in
flight. The vacuum result (bytes reclaimed) is logged at info level.

---

## 11. Configuration Surface

### 11.1 Minimal defaults

`config/pgw.example.yaml` shrinks to:

```yaml
server:
  port: "8080"
  base_path: "/"
  body_size_limit: "10M"
  master_key: ""     # empty => random on first boot, persisted to data/secret.key

storage:
  type: "sqlite"
  sqlite:
    path: "data/pgw.db"
  vacuum_interval_hours: 168

resilience:
  retry:
    max_retries: 3
    initial_backoff: 1s
    max_backoff: 30s
    backoff_factor: 2.0
    jitter_factor: 0.1
  circuit_breaker:
    enabled: true
    failure_threshold: 5
    success_threshold: 2
    timeout: 30s

http:
  timeout: 600

models:
  enabled_by_default: false

providers: {}   # populated from the dashboard, not from this file
```

### 11.2 First-run behavior

- If `data/` does not exist, it is created.
- If `data/pgw.db` does not exist, it is created and migrated.
- If `data/secret.key` does not exist, it is generated and `0600` is set.
- If `server.master_key` is empty, a 32-byte random value is generated,
  persisted to `data/master.key`, and used as the bearer token for every
  `/admin/*` request. The operator reads it from the terminal stdout and
  pastes it into the dashboard on first login.

### 11.3 Env overrides

Only the following env vars override YAML:

```text
PGW_PORT
PGW_DATA_DIR
PGW_CONFIG
PGW_MASTER_KEY
```

There is no `.env.template`. The `.env.template` file is removed.

---

## 12. Codebase Hygiene Rules

Every change in this plan follows these rules. They are not aspirational;
they are the definition of done.

1. No commit adds a new file unless it is required by a §5 kept
   subsystem or a §7-§9 new feature.
2. No commit re-adds an enterprise subsystem (§4.2) without a written
   decision in this file.
3. No commit ships a TODO that crosses a Stage boundary. TODOs inside a
   Stage must be resolved before the Stage is closed.
4. No commit ships a feature flag whose only consumer is the flag itself.
5. Every deletion is paired with: (a) imports removed, (b) tests deleted,
   (c) dashboard pages deleted, (d) admin handler files deleted, (e)
   `internal/app/app.go` reference removed. A grep across the repo for
   the old package name must return zero hits before the commit is
   pushed.
6. Every new table has `ON DELETE CASCADE` for every foreign key that
   can be deleted by the operator. The vacuum job reclaims the space.
7. Every endpoint that the SPA calls must have a hand-written test that
   calls the endpoint with a real request and asserts the response shape.
   No "this is just a thin proxy" excuses.

---

## 13. Build, Test, and Release

### 13.1 Local development loop

```text
1. edit source locally
2. go test ./...                 # local, only unit tests
3. git commit
4. git push
5. CI runs lint, vet, all tests, dashboard build consistency, e2e
6. on tag push, CI builds the Windows amd64 artifact and uploads it
```

The local machine never builds the release binary. The local machine
never starts the gateway against the real providers. Local `go test`
covers only the unit-test subset that is safe to run without secrets.

### 13.2 CI workflows

```text
.github/workflows/ci.yml
  on: push, pull_request
  jobs:
    - lint          (golangci-lint v2.13.x)
    - vet           (go vet ./...)
    - test          (go test ./... with sqlite in tmpdir)
    - dashboard     (pnpm install && pnpm build && diff vs internal/admin/dashboard/static/dist)
    - race          (go test -race ./internal/provider/...)

.github/workflows/release-windows.yml
  on: push tags matching v*
  jobs:
    - build-windows-amd64
        env: { GOOS: windows, GOARCH: amd64, CGO_ENABLED: 0 }
        run:  go build -trimpath -ldflags "..." -o pgw.exe ./cmd/pgw
    - package
        copy: pgw.exe + config/pgw.example.yaml + README.txt + LICENSE
        zip:  pgw-windows-amd64.zip
    - sha256
        sha256sum > SHA256SUMS.txt
    - smoke-windows
        runs-on: windows-latest
        run:   ./pgw.exe --version
```

### 13.3 Removed workflows

```text
.github/workflows/codeql.yml
.github/workflows/pr-title.yml
.github/workflows/release.yml         (upstream goreleaser)
```

### 13.4 .goreleaser.yaml

Deleted. The release is built directly by GitHub Actions.

### 13.5 Build flags

```text
-tags=netgo,osusergo,sqlite_omit_load_extension
-ldflags="-s -w -X main.version=... -X main.commit=... -X main.date=..."
-trimpath
```

`sqlite_omit_load_extension` ensures the SQLite driver cannot be asked to
load arbitrary `.so`/`.dll` files — important for a personal binary that
runs as the user's user.

---

## 14. Verification Plan

CI is the only execution environment that produces claims. The README,
CHANGELOG, and dashboard Settings page never claim performance numbers
that are not in a CI artifact.

### 14.1 Required CI checks

- `go test ./...` exits 0
- `golangci-lint run` exits 0
- `go vet ./...` exits 0
- `pnpm build` in `web/dashboard/` exits 0 and produces byte-identical
  output to `internal/admin/dashboard/static/dist/*`
- `pgw.exe --version` exits 0 with correct metadata
- `pgw.exe --help` exits 0 and lists every CLI flag
- `pgw.exe` boots on a clean `data/`, prints the master
  key, serves `/healthz`, serves `/v1/models`, exits 0 on SIGINT

### 14.2 Endpoint contract tests

A new `tests/contract/` package contains:

- `TestV1ChatCompletions_OpenAIShape`: POST `/v1/chat/completions` with
  an OpenAI-shaped body returns an OpenAI-shaped response.
- `TestV1ChatCompletions_Streaming`: streaming SSE events match the
  OpenAI shape (`data: {json}\n\n`, terminated by `data: [DONE]\n\n`).
- `TestV1Models_OnlyEnabled`: `GET /v1/models` returns only models with
  `enabled=1` from `provider_models`.
- `TestAdminProviders_CRUD`: add, list, edit, delete via the JSON API.
- `TestAdminProviders_Discover_Parses`: with a fake upstream that
  returns an OpenAI-shaped `/v1/models`, discover populates
  `provider_models` and extracts metadata heuristics correctly.
- `TestAdminProviders_Discover_401`: a fake upstream that returns 401
  causes discover to return an error and leaves `provider_models` empty.
- `TestAdminModels_Toggle`: PATCH `enabled` flips `/v1/models`
  visibility.
- `TestAdminStop_GracefulShutdown`: POST `/admin/stop` causes the
  server to exit within the grace period.
- `TestSecrets_EncryptedAtRest`: the secrets table stores ciphertext,
  not plaintext; deleting a provider requires deleting its secrets row
  first.

### 14.3 Dashboard contract test

`web/dashboard/src/lib/stores/runtimeConfig.svelte.js` exposes a
`CONFIG_KEYS` allowlist. A test (`TestDashboardConfigContract_*`) pins
this list to the backend's `DashboardConfigResponse` struct, mirroring
the upstream pattern.

---

## 15. Performance and Footprint Measurement

### 15.1 What CI measures

On every release build, CI records:

- binary size in bytes
- ZIP size in bytes
- number of files in the ZIP
- cold start time: `time ./pgw.exe --version`
- idle memory: boot, sleep 5s, sample `wmic process ... WorkingSetSize`
- smoke response time: POST `/v1/chat/completions` against a mock
  upstream, record TTFB and total duration

### 15.2 What we never claim

- "Fastest" without a comparison against named competitors on the same
  hardware.
- "Lowest memory" without a measurement under a stated load.
- "Smallest" without comparing compressed sizes of equivalent
  releases.

The README, CHANGELOG, and dashboard Settings page never repeat a
performance number that is not present in a CI artifact for the current
release tag.

---

## 16. Implementation Stages

Each stage ends in a green CI build and a clean commit. Stages are not
optional; later stages depend on earlier ones.

### Stage 0 — Foundation (1 commit)

- Remove `upstream` remote.
- Rename module path from `github.com/enterpilot/gomodel` to
  `github.com/<owner>/pgw`.
- Update every internal import.
- `go.mod` tidy.
- CI: lint + vet + test must pass before this commit merges.

### Stage 1 — Cut to one provider package (3-5 commits)

1.1 Add `internal/provider` with the generic adapter and tests.
1.2 Switch `run/providers.go` to register only the generic adapter.
1.3 Delete `internal/providers/openai/` after the generic adapter is
    wired and tested.
1.4 Delete the remaining 31 provider packages in one commit per package.
1.5 Delete `internal/providers/googlecommon/`,
    `internal/providers/auth_headers.go`,
    `chat_chunk_sse.go`, `responses_*`, `cache_control.go`,
    `cache_planner.go`, and every shared helper file in
    `internal/providers/*.go`.
1.6 Delete `internal/anthropicapi/`.

    Implementation note 2 (recorded after dependency analysis):
    `cache_control.go`, `cache_planner.go`, and `passthrough.go` in
    internal/providers are still referenced by `router.go` and
    `internal/server/passthrough_*`, which survive Stage 1. They are
    vendor-era Anthropic-cache/OpenAI-passthrough plumbing; their
    deletion is deferred to the router/server simplification stage so
    the build stays green at each commit. `internal/anthropicapi/` is
    imported by internal/server's Anthropic `/v1/messages` surface; it
    is deferred to the same server-surface cut (plan §4.3 deletes that
    surface).

    Implementation note (recorded after dependency analysis): openai is the
    shared base of 22 other vendor packages, so it cannot be deleted on its
    own without breaking them, and the only non-vendor consumers of the
    vendor set are the contract/perf replay suites (plan §4.3 deletes
    those). Stages 1.3+1.4 therefore land as one atomic cut: delete the
    whole vendor set plus the vendor-only contract/perf tests in a single
    commit, keeping the build green. `internal/providers/health/` is kept:
    the admin Providers page (plan §5.1) reads its Tracker snapshots and it
    imports nothing vendor-specific. `tests/contract/` keeps a placeholder
    doc.go; the new §14.2 contract suite repopulates it in Stage 14.

### Stage 2 — Cut Codex subsystems (2 commits)

2.1 Delete `internal/codeximport/` and `internal/admin/handler_codex_import.go`.
2.2 Delete `internal/codexoauth/`, `internal/providers/chatgpt/`, and
    `internal/admin/handler_codex_oauth.go`.

### Stage 3 — Cut observability and metrics (1 commit)

- Delete `internal/observability/`, `internal/versioncheck/`,
  `internal/runtimesettings/`, `internal/live/`.

### Stage 4 — Cut auth, conversation, files (2 commits)

4.1 Delete `internal/authkeys/`, `internal/conversationstore/`,
    `internal/responsestore/`.
4.2 Delete `internal/filestore/`, `internal/batch/`,
    `internal/batchrewrite/`, `internal/embedding/`, `internal/realtime/`.

### Stage 5 — Cut governance subsystems (2 commits)

5.1 Delete `internal/mcpgateway/`, `internal/guardrails/`,
    `internal/ratelimit/`, `internal/budget/`.
5.2 Delete `internal/workflows/`, `internal/session/`,
    `internal/tagging/`, `internal/pricingoverrides/`, `internal/usage/`,
    `internal/auditlog/`.

### Stage 6 — Cut infra artifacts (1 commit)

- Delete `ext/`, `cmd/recordapi/`, `helm/`, `Dockerfile`,
  `docker-compose.yaml`, `prometheus.yml`, `tools/`,
  `.goreleaser.yaml`, `.env.template`, `docs/install/`.

### Stage 7 — Dashboard rebuild (3 commits)

7.1 Delete `web/dashboard/src/pages/{audit-logs,usage,auth-keys,budgets,rate-limits,workflows,guardrails,mcp-servers}/`.
7.2 Rewrite `web/dashboard/src/App.svelte` and
    `web/dashboard/src/lib/components/organisms/navigation.js` to the
    five-page inventory.
7.3 Add the Providers page (add/edit/delete/refresh, per-row model
    count, edit-metadata modal).
7.4 Add the Models page (table, tick column, edit-metadata action,
    playground link).
7.5 Add the Stop/Restart actions to the Settings page.
7.6 Rebuild `internal/admin/dashboard/static/dist/*` from source and
    commit.

### Stage 8 — Storage migration (2 commits)

8.1 Add the new schema migration (drop obsolete tables, rename
    `model_preferences` -> `provider_models`, fold `hidden` into
    `enabled`).
8.2 Add KEK generation, secret encryption, vacuum goroutine.

### Stage 9 — Configuration and runtime defaults (1 commit)

- Rewrite `config/pgw.example.yaml` to the minimal defaults.
- Rewrite `config/config.go`'s `buildDefaultConfig` to the new shape.
- Replace `config/flow.yaml` with a one-line `flow: {}` or delete it.
- Remove `cmd/gomodel/docs/`, `cmd/gomodel/swagger_disabled.go`,
  `cmd/gomodel/swagger_enabled.go`. The only entry point becomes
  `cmd/pgw/main.go`.

### Stage 10 — CLI flags and lifecycle (2 commits)

10.1 Add `--help`, `--version`, `--config`, `--data-dir`, `--port`,
     `--tray`, `--stop` to `run/run.go`.
10.2 Add `internal/tray/` (Windows-only build tag) that talks to a
     single running gateway via a local named pipe
     (`\.\pipe\pgw-ctl`).

### Stage 11 — Windows release pipeline (1 commit)

- Rewrite `.github/workflows/release-windows.yml` to the new build
  recipe.
- Delete `.github/workflows/{release.yml,codeql.yml,pr-title.yml}`.
- Add `.github/workflows/ci.yml` with lint, vet, test, dashboard
  build, race.
- Add `.github/workflows/dashboard-consistency.yml` that fails when
  the source build does not match the committed dist.

### Stage 12 — Branding cleanup (1 commit)

- Rename `cmd/gomodel/` to `cmd/pgw/`.
- Update `ProductName`, `--version` output, dashboard header, README.
- Update `go.mod` module path (if not done in Stage 0).
- Update every reference in `docs/`.

### Stage 13 — Documentation pass (1 commit)

- Rewrite `README.md` to the new architecture.
- Rewrite `AGENTS.md` to remove upstream references.
- Update `docs/` to drop upstream links.

---

## 17. Acceptance Criteria

This plan is complete when all of the following are true:

1. `go test ./...` exits 0 on the personal-edition branch.
2. `golangci-lint run` exits 0.
3. `pnpm build` in `web/dashboard/` produces output byte-identical to
   `internal/admin/dashboard/static/dist/`.
4. `pgw.exe --version` prints version, commit, date.
5. `pgw.exe --help` lists every CLI flag.
6. Booting a fresh `data/` directory produces a usable dashboard and
   prints a one-time master key.
7. Adding a provider with a real OpenAI-compatible `base_url` and
   `api_key` causes discover to populate `provider_models` and
   `/v1/models`.
8. Toggling a model on the Models page causes it to appear in
   `/v1/models`; toggling off causes it to disappear.
9. Editing a model's metadata and saving survives a "Refresh models"
   click (heuristic re-parse does not overwrite manual values).
10. POST `/admin/stop` causes the server to exit within the grace
    period, releasing the SQLite handle cleanly.
11. Deleting a provider cascades to its secrets and provider_models.
12. The CI workflow on a `v*` tag produces a `pgw-windows-amd64.zip`
    with `pgw.exe`, `config/pgw.example.yaml`,
    `README.txt`, `LICENSE`, `SHA256SUMS.txt`, and the smoke job on
    `windows-latest` exits 0.
13. The local binary size is below 25 MB (target, not yet measured).
14. No `internal/providers/<vendor>/` directory exists.
15. No file under `web/dashboard/src/pages/` matches the removed-page
    inventory in §8.2.
16. No `TODO` from any Stage appears in `git grep TODO` on `main`.


---

## 18. Non-Goals

These are deliberately out of scope and will not be added unless the
project scope changes.

- Multi-user, multi-tenant, or team-billing features.
- A web-based marketplace of "verified providers".
- OAuth, login flows, or any protocol that requires a browser callback
  loop in the binary itself.
- Helm, Kubernetes, Docker, Compose, or any container deployment.
- Helm-style values, schema validation, or chart packaging.
- Voice, audio, image generation, embeddings, or any `/v1/audio`,
  `/v1/embeddings`, `/v1/images`, or `/v1/realtime` surface.
- Files API, Batches API, Conversation API, MCP gateway.
- Auditing, alerting, observability beyond a recent-requests log.
- Per-user rate limits, budgets, or quotas.
- Provider-specific quirks beyond what the OpenAI standard already
  exposes.
- Local compilation, local testing, or local release builds as a
  substitute for CI.

---

## 19. Working Principle

Use the smallest maintainable codebase that still satisfies the §1 goals.

```text
native Go gateway
  + single OpenAI-compatible adapter
  + SQLite (pure Go driver)
  + virtual model routing
  + recent-request log
  + five-page dashboard
  + per-model metadata overrides
  + Windows amd64 single binary via GitHub Actions
```

Prefer **deletion** over **configuration gating**. If a feature can be
removed without harming §1, it must be removed. If a feature is kept, it
must be wired all the way through (server -> admin handler -> dashboard
page -> dashboard test). Half-built features are forbidden.

---

## 20. Glossary

| Term              | Meaning                                                |
|-------------------|--------------------------------------------------------|
| Provider          | A user-configured OpenAI-compatible upstream identified by display_name, base_url, api_key |
| Provider model    | One model id under one provider, with metadata and an enabled flag |
| Discovery         | The act of calling GET {base_url}/v1/models and parsing the response |
| Tick / untick     | Toggle the enabled flag on a provider model from the Models page |
| Heuristic field   | A non-standard field name from /v1/models that the gateway recognizes (see §2.3 table) |
| Manual override   | A user-edited metadata value stored in `provider_models.user_overrides` JSON; wins over heuristics |
| KEK               | Key-Encryption Key; AES-GCM key in `data/secret.key` that wraps each `secrets.ciphertext` row |
| Master key        | Single bearer token guarding every `/admin/*` endpoint; random on first boot, persisted in `data/master.key` |
| Vacuum            | `PRAGMA incremental_vacuum` to release free pages in the SQLite file |
| Stage             | One unit of work in §16; ends in a green CI build      |

---

## 21. Open Questions

1. Should the dashboard read raw SSE chunks and render them incrementally,
   or buffer full responses? (Personal playground UX prefers incremental;
   simpler implementation prefers buffer.)
2. Should `/v1/models` projection include virtual models, provider models,
   or both? Default proposal: both, with virtual models listed first.
3. Should the binary name be `pgw.exe` or something shorter
   like `pgw.exe`? Default proposal: `pgw.exe`.
4. Should the tray mode use a Win32 message-only window or a regular
   hidden window? Default proposal: regular hidden window so the icon
   shows in the system tray.
5. Should the gateway refuse to boot if `data/secret.key` is world-readable
   on POSIX? Default proposal: yes, fail closed with a clear message.

These are answered during Stage 7 (dashboard) and Stage 10 (lifecycle).
The plan is not blocked on them.

---

## 22. References

- The OpenAI Chat Completions API:
  https://platform.openai.com/docs/api-reference/chat
- The OpenAI Models API:
  https://platform.openai.com/docs/api-reference/models
- SQLite foreign key support:
  https://www.sqlite.org/foreignkeys.html
- SQLite WAL mode:
  https://www.sqlite.org/wal.html
- modernc.org/sqlite (pure-Go driver):
  https://pkg.go.dev/modernc.org/sqlite
- Go `embed` package:
  https://pkg.go.dev/embed
- golangci-lint v2.13:
  https://golangci-lint.run/
- GitHub Actions windows-latest runner:
  https://docs.github.com/en/actions/using-github-hosted-runners/about-github-hosted-runners

There are no references to upstream GoModel in this file by design.
