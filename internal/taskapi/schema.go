package taskapi

// SchemaColumns is the complete column list of every relation the agent SQL
// surface can read. It feeds the query_tasks_sql tool description so agents
// can write a correct query without a discovery round-trip, and a
// Postgres-backed test asserts it exactly matches the live schema.
var SchemaColumns = map[string][]string{
	"tasks": {
		"id", "title", "description", "agent_session_url", "status",
		"created_at", "updated_at", "version", "position", "deleted_at",
		"wake_at", "waiting_on", "on_timeout", "snooze_count",
		"context", "context_checked_at",
	},
	"task_overview": {
		"id", "title", "description", "agent_session_url", "status",
		"created_at", "updated_at", "blocked", "snoozed", "sleeping",
		"wake_at", "waiting_on", "on_timeout", "snooze_count",
		"context", "context_checked_at", "depends_on",
		"dependency_count", "image_count", "version", "position",
	},
	"dependencies": {"task_id", "depends_on_id"},
	"images":       {"task_id", "object_key", "name", "content_type"},
	"task_revisions": {
		"revision_id", "task_id", "version", "action", "actor_kind", "actor_id",
		"source", "request_id", "occurred_at", "before_state", "after_state",
		"metadata",
	},
}

// SchemaReference renders the exact schema and the semantics an agent needs
// to query it correctly. It is embedded in the query_tasks_sql tool
// description.
func SchemaReference() string {
	return `Schema (complete; these are the only readable relations and their exact columns):
- task_overview (live tasks only, one row per task; deleted tasks are absent, so it has no deleted_at): ` + columnList("task_overview") + `.
  blocked, snoozed, and sleeping are integer 0/1: blocked = an unfinished dependency, snoozed = wake_at in the future, sleeping = either, meaning the task is held off the board. depends_on is a text[] of prerequisite task IDs. status is todo, in_progress, or done. position is the user's hand-set priority order within a status column.
- tasks (live and deleted; deleted rows have deleted_at IS NOT NULL): ` + columnList("tasks") + `.
- dependencies: ` + columnList("dependencies") + `. images: ` + columnList("images") + `.
- task_revisions (append-only audit history; before_state/after_state/metadata are JSONB task snapshots): ` + columnList("task_revisions") + `.
The actionable board is: SELECT * FROM task_overview WHERE status <> 'done' AND sleeping = 0 ORDER BY position. Read a task's version here and pass it to mutating tools as expected_version.`
}

func columnList(relation string) string {
	list := ""
	for i, column := range SchemaColumns[relation] {
		if i > 0 {
			list += ", "
		}
		list += column
	}
	return list
}
