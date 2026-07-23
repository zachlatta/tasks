# Tasks

A small, self-hosted task manager with one shared Go backend and three interfaces:

- a `tasks` CLI;
- a secret-protected web UI; and
- an OAuth-protected MCP server over Streamable HTTP.

Tasks live in PostgreSQL as the single source of truth. Every user-facing read goes through read-only SQL against those tables, while create/complete operations go through the shared task service. Every successful mutation also appends an immutable before/after revision in the same database transaction.

## Quick start

Requires Go 1.26.5 or newer and a PostgreSQL 13+ server.

```sh
cp .env.example .env
# Set TASKS_SECRET and TASKS_DATABASE_URL in .env.

# For local development you can start Postgres with Docker:
docker run -d --name tasks-pg -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=tasks -p 5432:5432 postgres:16-alpine

go run ./cmd/tasks add "Write the first task"
go run ./cmd/tasks query 'SELECT id, status, title FROM task_overview'
go run ./cmd/tasks serve
```

Open <http://127.0.0.1:8080> and enter the same `TASKS_SECRET` from `.env`.

The schema (`tasks`, `dependencies`, `images`, and the `task_overview` view) is created automatically on first connection. `TASKS_DATABASE_URL` is required by every command, not just `serve`.

## CLI

```text
tasks add [--description text] [--depends-on id,id] <title>
tasks query <read-only-sql>
tasks done <task-id>
tasks serve
tasks version
```

The CLI has no `list` or `show` shortcut. Every user-facing read goes through read-only SQL and is returned as structured JSON. The web UI uses fixed SQL against the same projection, while mutations from every interface still go through `internal/task.Service`.

```sh
tasks query 'SELECT id, status, blocked, title FROM task_overview ORDER BY created_at DESC'
tasks query "SELECT version, action, actor_kind, source, occurred_at, before_state, after_state FROM task_revisions WHERE task_id = '<task-id>' ORDER BY version"
```

## MCP

The MCP endpoint is `https://your-host.example/mcp`. It implements Streamable HTTP plus OAuth authorization-code flow with S256 PKCE, dynamic client registration, authorization-server metadata, and protected-resource metadata. The authorization page asks the user for `TASKS_SECRET`.

Available tools:

- `query_tasks_sql`: arbitrary read-only PostgreSQL `SELECT`, `WITH`, or `EXPLAIN` queries, capped at 500 rows, including task revision history;
- `create_task`: create a todo, optionally with dependency IDs; and
- `complete_task`: mark a task done once its dependencies are done.

There is deliberately no MCP `list_tasks` tool. Trusted agents can inspect the schema with:

```sql
SELECT table_name, column_name, data_type
FROM information_schema.columns
WHERE table_schema = 'public'
ORDER BY table_name, ordinal_position;
```

Each read runs inside a PostgreSQL `READ ONLY` transaction; the CLI and MCP layers also reject statements that do not begin with `SELECT`, `WITH`, or `EXPLAIN`. The intentionally small schema is:

- `tasks(id, title, description, status, created_at, updated_at, version)`
- `dependencies(task_id, depends_on_id)`
- `images(task_id, object_key, name, content_type)`
- `task_revisions(revision_id, task_id, version, action, actor_kind, actor_id, source, request_id, occurred_at, before_state, after_state, metadata)`
- `task_overview`: task columns plus `blocked`, `dependency_count`, and `image_count`

`blocked` is `1` when at least one dependency is not done and `0` otherwise. Agents can discover the schema directly through `information_schema`; there are no non-SQL read tools.

## Revision history

`task_revisions` is an append-only, Git-like history of successful task changes. Creating, completing, or attaching an image updates the current task and records one revision atomically. A failed or blocked operation records nothing, and repeating completion on an already-done task is a no-op.

Each revision contains:

- a per-task version and semantic action;
- its interface (`cli`, `web`, `mcp`, or `migration`) and the best actor identity currently available;
- complete JSONB snapshots before and after the mutation; and
- a database timestamp, plus reserved request and metadata fields.

The first revision has a null `before_state`. Existing tasks receive a version-one `import` baseline when the upgraded server first opens their database; changes made before that baseline cannot be reconstructed. Revision rows reject update, delete, and truncate operations. Task versions also prevent a stale writer from silently overwriting a newer mutation.

The web UI uses one shared secret, so its revisions identify the web/shared-secret surface rather than an individual person. MCP revisions include the authenticated OAuth client ID. CLI revisions identify the local CLI but do not collect the operating-system username.

Revision snapshots retain old task text and attachment metadata intentionally. They do not copy image bytes; preserving deleted or replaced image content requires object-store versioning or retention.

OAuth clients, authorization codes, access and refresh tokens, and browser sessions are persisted in PostgreSQL. Secret token and session values are stored only as hashes, and authorized clients remain connected across server restarts.

## Configuration

The process reads `.env` when it starts. Existing environment variables take precedence.

| Variable | Default | Purpose |
| --- | --- | --- |
| `TASKS_SECRET` | required for `serve` | Shared secret used by web login and OAuth authorization |
| `TASKS_DATABASE_URL` | required for all commands | PostgreSQL connection string for task storage |
| `TASKS_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `TASKS_PUBLIC_URL` | `https://tasks.hackclub.com` | Public OAuth issuer origin; HTTPS required off loopback |
| `TASKS_DATA_DIR` | OS user config directory | Default parent directory for local image storage |
| `TASKS_OBJECT_STORE` | `local` | `local` or `s3` |
| `TASKS_LOCAL_OBJECT_DIR` | `<data-dir>/images` | Local development image storage |
| `TASKS_S3_ENDPOINT` | none | S3-compatible endpoint without scheme |
| `TASKS_S3_ACCESS_KEY` | none | S3 access key |
| `TASKS_S3_SECRET_KEY` | none | S3 secret key |
| `TASKS_S3_BUCKET` | none | Existing image bucket |
| `TASKS_S3_REGION` | none | Optional bucket region |
| `TASKS_S3_USE_SSL` | `true` | Use TLS for object storage |

The S3 credentials belong in deployment secrets, never in a committed `.env` file.

## Development

```sh
make test
make build
```

The Postgres-backed tests (storage, MCP tools, CLI) are skipped unless `TASKS_TEST_DATABASE_URL` points at a reachable server. Each test provisions and drops its own database, so point it at a throwaway instance:

```sh
docker run -d --name tasks-test-pg -e POSTGRES_PASSWORD=postgres \
  -p 5432:5432 postgres:16-alpine
TASKS_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
  make test
```

Tests cover the domain service, PostgreSQL persistence, read-only SQL enforcement, OAuth/PKCE, HTTP origin protection, MCP tools, CLI behavior, browser sessions, CSRF checks, and image uploads.

## Releases and Homebrew

Every source commit pushed to `main` runs the full test suite and replaces the rolling `edge` GitHub prerelease with cross-platform archives for that commit. The edge workflow also updates the checksummed formula in this repository, so the tap follows `main`. Tags matching `v*` create immutable stable releases through GoReleaser.

Install the latest `main` build through the repository's Homebrew tap:

```sh
brew tap zachlatta/tasks https://github.com/zachlatta/tasks
brew trust --tap zachlatta/tasks
brew install tasks
```

After later commits reach `main`, update it with `brew update && brew upgrade tasks`.

The release workflows use only the repository-scoped `GITHUB_TOKEN`; no package or object-storage credentials are embedded in builds.

## Current boundaries

- PostgreSQL is the source of truth for task and authentication state, so multiple instances can share it. Production instances must also share the configured S3-compatible image store; local image storage is single-instance only.
- The shared secret grants full task access. There are not yet per-user identities or separate read/write grants.
- Public deployments should add reverse-proxy request throttling for the login, registration, and authorization endpoints.
- Attachments are images up to 10 MiB. Local storage is for development; production can use an existing S3-compatible bucket.
- Task revisions cover successful domain mutations, not reads, failed login attempts, or database-administrator activity. Deployments needing forensic change capture should stream PostgreSQL changes to an external immutable destination in addition to this application history.
- The project does not yet declare an open-source license.
