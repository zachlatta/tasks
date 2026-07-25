# Tasks

A small, self-hosted task manager with one shared Go backend and three interfaces:

- a `tasks` CLI;
- a secret-protected, drag-and-drop kanban web UI; and
- an OAuth-protected MCP server over Streamable HTTP.

Tasks live in PostgreSQL as the single source of truth. Every user-facing read goes through read-only SQL against those tables, while create/edit/start/move/complete/delete operations go through the shared task service. Every successful mutation also appends an immutable before/after revision in the same database transaction. Deletion is always soft: a deleted task keeps its row and its whole history and can be restored unchanged.

## Quick start

Requires Go 1.26.5 or newer and a PostgreSQL 13+ server.

```sh
cp .env.example .env
# Set TASKS_SECRET and TASKS_DATABASE_URL in .env.

# For local development you can start Postgres with Docker:
docker run -d --name tasks-pg -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=tasks -p 5432:5432 postgres:16-alpine

# Start the API, web UI, and MCP server:
go run ./cmd/tasks serve

# In another shell, the CLI uses TASKS_API_URL and TASKS_SECRET:
go run ./cmd/tasks add "Write the first task"
go run ./cmd/tasks query 'SELECT id, status, title FROM task_overview'
```

Open <http://127.0.0.1:8080> and enter the same `TASKS_SECRET` from `.env`.

The server creates the schema (`tasks`, `dependencies`, `images`, and the `task_overview` view) automatically on first connection. Only `tasks serve` connects to PostgreSQL. Other CLI commands call `TASKS_API_URL` with the same `TASKS_SECRET` used by the web and MCP authorization flow.

## CLI

```text
tasks add [--description text] [--depends-on id,id] <title>
tasks edit [--title text] [--description text | --description-file path|-] [--depends-on id,id] [--expected-version n] <task-id>
tasks query <read-only-sql>
tasks done <task-id>
tasks delete <task-id>
tasks restore <task-id>
tasks serve
tasks version
```

The CLI has no `list` or `show` shortcut. Every user-facing read goes through read-only SQL on the server and is returned as structured JSON. The web UI uses fixed SQL against the same projection, while mutations from every interface still go through `internal/task.Service`. The CLI never receives or opens `TASKS_DATABASE_URL`.

`tasks edit` replaces only the fields named by flags. Use `--description-file` for longer Markdown; `-` reads it from stdin. Passing an empty `--description` clears the description, and an empty `--depends-on` clears all dependencies. `--expected-version` is optional optimistic concurrency protection for scripts that first read a task.

```sh
tasks edit --title "Research primary sources" <task-id>
tasks edit --description-file notes.md --expected-version 3 <task-id>
cat notes.md | tasks edit --description-file - <task-id>
tasks edit --depends-on prerequisite-id,other-id <task-id>
```

`tasks delete` soft-deletes a task and `tasks restore` brings it back. Deleted tasks are absent from `task_overview` and every other read; list them straight from the `tasks` table:

```sh
tasks query 'SELECT id, title, deleted_at FROM tasks WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC'
```

## HTTP API

The CLI calls `POST /api/tools/{name}` with `Authorization: Bearer <TASKS_SECRET>`. The available names mirror MCP: `query_tasks_sql`, `create_task`, `edit_task_text`, `update_task`, `complete_task`, `delete_task`, and `restore_task`. Inputs and successful `data` values use the same JSON types as the corresponding MCP tools:

```sh
curl -sS https://your-host.example/api/tools/create_task \
  -H "Authorization: Bearer $TASKS_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"title":"Created through the API"}'
```

Successful responses use `{"data": ...}`. Errors use `{"error":{"code":"...","message":"..."}}`. The API and MCP transports share one transport-neutral implementation, so validation and task behavior stay identical.

## Web board

The homepage is a three-column board of **To do**, **In progress**, and **Done**.

- Drag a card to another column to change its status, or within a column to set its order by hand. Both are saved immediately, and a card dropped into **Done** ahead of its dependencies snaps back with the reason.
- Cards are previews: title, a plain-text slice of the description, dependency and file counts, and a cover thumbnail of the first image. Click one for the full task in a slide-over panel; the URL follows, so the panel is shareable and the back button closes it.
- In task detail, double-click the title or description to edit it in place. Press `Enter` to save a title, `⌘`/`Ctrl` + `Enter` to save a description, or `Escape` to cancel.
- **Delete task** at the bottom of the detail takes a card off the board without a confirmation prompt, because it is reversible: the toast that follows offers an undo, and the board's `n deleted` link opens `/deleted`, where every deleted task can be read and restored to exactly where it was.
- Every drag has a pointer-free equivalent. The `⋯` menu on each card moves it between columns, and focusing a card and holding `⌘`/`Ctrl` with the arrow keys moves it left, right, up, or down.
- Without JavaScript the same board renders, the `⋯` menu posts an ordinary form, and cards open a full detail page. File attachments live on that detail view, images previewed inline and everything else as a download.

Column order lives in `tasks.position`, a float that is halved between neighbors on each move so a drag writes one row. Tasks stored before the board could be reordered are spread out once, newest first, the first time the upgraded server opens the database.

```sh
tasks query 'SELECT id, status, blocked, title FROM task_overview ORDER BY created_at DESC'
tasks query "SELECT version, action, actor_kind, source, occurred_at, before_state, after_state FROM task_revisions WHERE task_id = '<task-id>' ORDER BY version"
```

## MCP

The MCP endpoint is `https://your-host.example/mcp`. It implements Streamable HTTP plus OAuth authorization-code flow with S256 PKCE, dynamic client registration, authorization-server metadata, and protected-resource metadata. The authorization page asks the user for `TASKS_SECRET`.

Available tools:

- `query_tasks_sql`: arbitrary read-only PostgreSQL `SELECT`, `WITH`, or `EXPLAIN` queries, capped at 500 rows, including task revision history;
- `create_task`: create a todo, optionally with dependency IDs;
- `edit_task_text`: atomically apply one or more exact `old_text`/`new_text` replacements to a task title or description;
- `update_task`: replace any supplied title, description, or complete dependency list;
- `complete_task`: mark a task done once its dependencies are done;
- `delete_task`: soft-delete a task; and
- `restore_task`: return a soft-deleted task to the board.

`delete_task` never removes data. It stamps `tasks.deleted_at`, which takes the task off the board and out of `task_overview`, the domain list, and every other operation; `restore_task` clears the stamp and the task comes back with its status, board position, text, dependencies, and files intact. Two guardrails keep live dependencies resolvable: a task other live tasks depend on cannot be deleted, and a task whose own dependencies are still deleted cannot be restored until they are. Both tools are idempotent.

`edit_task_text` is intended for agent-authored contextual edits. Replacements run in order in one transaction. By default each `old_text` must occur exactly once; missing or ambiguous text fails the whole call, while `replace_all: true` explicitly replaces every occurrence. `update_task` is the whole-field equivalent: omitted fields remain unchanged, while empty description text or an empty dependency list clears the field. Both tools accept an optional `expected_version` from a prior query so a stale agent cannot overwrite a newer task. Dependency edits reject missing tasks and cycles.

There is deliberately no MCP `list_tasks` tool. Trusted agents can inspect the schema with:

```sql
SELECT table_name, column_name, data_type
FROM information_schema.columns
WHERE table_schema = 'public'
ORDER BY table_name, ordinal_position;
```

Each read runs inside a PostgreSQL `READ ONLY` transaction; the HTTP API and MCP layers also reject statements that do not begin with `SELECT`, `WITH`, or `EXPLAIN`. The intentionally small schema is:

- `tasks(id, title, description, status, position, created_at, updated_at, version, deleted_at)`, where status is `todo`, `in_progress`, or `done`, `position` orders a column top to bottom, and a non-null `deleted_at` marks a soft-deleted task
- `dependencies(task_id, depends_on_id)`
- `images(task_id, object_key, name, content_type)`
- `task_revisions(revision_id, task_id, version, action, actor_kind, actor_id, source, request_id, occurred_at, before_state, after_state, metadata)`
- `task_overview`: task columns plus `blocked`, `dependency_count`, and `image_count`, for live tasks only

`blocked` is `1` when at least one dependency is not done and `0` otherwise. Deleted tasks are absent from `task_overview`; read them from `tasks` with `deleted_at IS NOT NULL`. Agents can discover the schema directly through `information_schema`; there are no non-SQL read tools.

## Revision history

`task_revisions` is an append-only, Git-like history of successful task changes. Creating, editing, starting, completing, reopening, reordering, attaching a file, deleting, or restoring updates the current task and records one revision atomically, under the action `create`, `edit`, `start`, `complete`, `reopen`, `reorder`, `add_attachment`, `delete`, or `restore`. A failed or blocked operation records nothing; edits that produce no change, repeated completion on an already-done task, deleting an already-deleted task, restoring a live one, and dropping a card back where it came from are all no-ops. The rare `rebalance` action appears when a column's positions can no longer be split and are spread back out.

Each revision contains:

- a per-task version and semantic action;
- its interface (`cli`, `web`, `mcp`, or `migration`) and the best actor identity currently available;
- complete JSONB snapshots before and after the mutation; and
- a database timestamp, plus reserved request and metadata fields.

The first revision has a null `before_state`. Existing tasks receive a version-one `import` baseline when the upgraded server first opens their database; changes made before that baseline cannot be reconstructed. Revision rows reject update, delete, and truncate operations. Task versions also prevent a stale writer from silently overwriting a newer mutation.

The web UI and CLI use one shared secret, so their revisions identify the corresponding web or CLI shared-secret surface rather than an individual person. MCP revisions include the authenticated OAuth client ID.

Revision snapshots retain old task text and attachment metadata intentionally. They do not copy file bytes; preserving deleted or replaced file content requires object-store versioning or retention.

OAuth clients, authorization codes, access and refresh tokens, and browser sessions are persisted in PostgreSQL. Secret token and session values are stored only as hashes, and authorized clients remain connected across server restarts.

## Configuration

The process reads `.env` when it starts. Existing environment variables take precedence.

| Variable | Default | Purpose |
| --- | --- | --- |
| `TASKS_SECRET` | required for server and CLI | Shared secret used by CLI API auth, web login, and OAuth authorization |
| `TASKS_API_URL` | required for CLI commands | Base URL of the task server; remote URLs must use HTTPS |
| `TASKS_DATABASE_URL` | required for `serve` | PostgreSQL connection string for task and auth storage |
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

The Postgres-backed tests (storage and MCP integration) are skipped unless `TASKS_TEST_DATABASE_URL` points at a reachable server. Each test provisions and drops its own database, so point it at a throwaway instance:

```sh
docker run -d --name tasks-test-pg -e POSTGRES_PASSWORD=postgres \
  -p 5432:5432 postgres:16-alpine
TASKS_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
  make test
```

Tests cover the domain service, PostgreSQL persistence and migrations, soft delete and restore, read-only SQL enforcement, shared-secret API auth, the CLI HTTP client, OAuth/PKCE, HTTP origin protection, MCP tools, CLI behavior, browser sessions, CSRF checks, and file uploads.

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

- PostgreSQL is the source of truth for task and authentication state, so multiple instances can share it. Production instances must also share the configured S3-compatible object store; local file storage is single-instance only.
- The shared secret grants full task access. There are not yet per-user identities or separate read/write grants.
- Public deployments should add reverse-proxy request throttling for the shared-secret API, login, registration, and authorization endpoints.
- Attachments can be any file type up to 50 MiB. Local storage is for development; production can use an existing S3-compatible bucket.
- Deletion is soft and has no purge. A deleted task keeps its row, attachments, and history until it is removed directly in PostgreSQL and the object store, which is deliberate: nothing an interface can do destroys task data.
- Task revisions cover successful domain mutations, not reads, failed login attempts, or database-administrator activity. Deployments needing forensic change capture should stream PostgreSQL changes to an external immutable destination in addition to this application history.
- The project does not yet declare an open-source license.
