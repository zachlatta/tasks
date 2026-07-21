# Task Tracker

A small, self-hosted task tracker with one shared Go backend and three interfaces:

- a `task-tracker` CLI;
- a secret-protected web UI; and
- an OAuth-protected MCP server over Streamable HTTP.

Tasks are canonical Markdown files. A synchronized SQLite read model lets trusted MCP agents query them with arbitrary read-only SQL, while create/complete operations still go through the shared task service.

## Quick start

Requires Go 1.26.5 or newer.

```sh
cp .env.example .env
# Set TASK_TRACKER_SECRET in .env.
go run ./cmd/task-tracker add "Write the first task"
go run ./cmd/task-tracker list
go run ./cmd/task-tracker serve
```

Open <http://127.0.0.1:8080> and enter the same `TASK_TRACKER_SECRET` from `.env`.

By default, application data lives in the operating system's user config directory. Set `TASK_TRACKER_DATA_DIR=./data` in `.env` if you want a visible project-local directory during development.

## CLI

```text
task-tracker add [--description text] [--depends-on id,id] <title>
task-tracker list [--json]
task-tracker done <task-id>
task-tracker serve
task-tracker version
```

The CLI, web UI, and MCP tools all call `internal/task.Service`; there are no interface-specific task implementations.

## MCP

The MCP endpoint is `https://your-host.example/mcp`. It implements Streamable HTTP plus OAuth authorization-code flow with S256 PKCE, dynamic client registration, authorization-server metadata, and protected-resource metadata. The authorization page asks the user for `TASK_TRACKER_SECRET`.

Available tools:

- `query_tasks_sql`: arbitrary read-only SQLite `SELECT`, `WITH`, or `EXPLAIN` queries, capped at 500 rows;
- `create_task`: create a todo, optionally with dependency IDs; and
- `complete_task`: mark a task done once its dependencies are done.

There is deliberately no MCP `list_tasks` tool. Trusted agents can inspect the schema with:

```sql
SELECT sql
FROM sqlite_schema
WHERE type IN ('table', 'view')
ORDER BY name;
```

The query connection uses SQLite `PRAGMA query_only = ON`; the MCP layer also rejects statements that do not begin with `SELECT`, `WITH`, or `EXPLAIN`. The exposed tables are:

- `tasks(id, title, description, status, created_at, updated_at)`
- `task_dependencies(task_id, dependency_id)`
- `task_attachments(task_id, object_key, name, content_type)`

OAuth clients, authorization codes, access tokens, and browser sessions are currently in memory. Restarting the server signs everyone out. This is suitable for a basic single-instance deployment; a durable/distributed token store is the next step before horizontal scaling.

## Configuration

The process reads `.env` when it starts. Existing environment variables take precedence.

| Variable | Default | Purpose |
| --- | --- | --- |
| `TASK_TRACKER_SECRET` | required for `serve` | Shared secret used by web login and OAuth authorization |
| `TASK_TRACKER_ADDR` | `127.0.0.1:8080` | HTTP listen address |
| `TASK_TRACKER_PUBLIC_URL` | derived from listen address | Public OAuth issuer origin; HTTPS required off loopback |
| `TASK_TRACKER_DATA_DIR` | OS user config directory | Markdown files and SQLite read model |
| `TASK_TRACKER_OBJECT_STORE` | `local` | `local` or `s3` |
| `TASK_TRACKER_LOCAL_OBJECT_DIR` | `<data-dir>/images` | Local development image storage |
| `TASK_TRACKER_S3_ENDPOINT` | none | S3-compatible endpoint without scheme |
| `TASK_TRACKER_S3_ACCESS_KEY` | none | S3 access key |
| `TASK_TRACKER_S3_SECRET_KEY` | none | S3 secret key |
| `TASK_TRACKER_S3_BUCKET` | none | Existing image bucket |
| `TASK_TRACKER_S3_REGION` | none | Optional bucket region |
| `TASK_TRACKER_S3_USE_SSL` | `true` | Use TLS for object storage |

The S3 credentials belong in deployment secrets, never in a committed `.env` file.

## Development

```sh
make test
make build
```

Tests cover the domain service, Markdown persistence, read-only SQL enforcement, OAuth/PKCE, HTTP origin protection, MCP tools, CLI behavior, browser sessions, CSRF checks, and image uploads.

## Releases and Homebrew

Every commit pushed to `main` runs the full test suite and replaces the rolling `edge` GitHub prerelease with cross-platform archives for that commit. Tags matching `v*` create immutable stable releases through GoReleaser.

Stable tagged releases also publish a Homebrew cask into this repository. After the first tagged release:

```sh
brew tap zachlatta/task-tracker https://github.com/zachlatta/task-tracker
brew install --cask task-tracker
```

The release workflows use only the repository-scoped `GITHUB_TOKEN`; no package or object-storage credentials are embedded in builds.

## Current boundaries

- One process should own the Markdown directory. Multi-instance deployments need shared locking and durable OAuth/session state.
- The shared secret grants full task access. There are not yet per-user identities or separate read/write grants.
- Public deployments should add reverse-proxy request throttling for the login, registration, and authorization endpoints.
- Attachments are images up to 10 MiB. Local storage is for development; production can use an existing S3-compatible bucket.
- The project does not yet declare an open-source license.
