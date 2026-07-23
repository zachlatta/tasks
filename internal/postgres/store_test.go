package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/pgtest"
	"github.com/zachlatta/tasks/internal/task"
)

func TestServiceMutationsRecordCompleteTaskRevisions(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	service := task.NewService(store, func() time.Time {
		current := now
		now = now.Add(time.Minute)
		return current
	}, func() string { return "audit-me" })

	created, err := service.Create(ctx, task.CreateInput{Title: "Keep history", Description: "Initial description"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.AddAttachment(ctx, created.ID, task.Attachment{
		Key: "audit-me/evidence.png", Name: "evidence.png", ContentType: "image/png",
	}); err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if _, err := service.Complete(ctx, created.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	result, err := store.Query(ctx, `
		SELECT version, action, before_state, after_state
		FROM task_revisions
		WHERE task_id = 'audit-me'
		ORDER BY version
	`)
	if err != nil {
		t.Fatalf("query revisions: %v", err)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("revision count = %d, want 3; rows = %#v", len(result.Rows), result.Rows)
	}
	wantActions := []string{"create", "add_attachment", "complete"}
	for i, want := range wantActions {
		if got := fmt.Sprint(result.Rows[i]["action"]); got != want {
			t.Fatalf("revision %d action = %q, want %q", i, got, want)
		}
		if got := fmt.Sprint(result.Rows[i]["version"]); got != fmt.Sprint(i+1) {
			t.Fatalf("revision %d version = %q, want %d", i, got, i+1)
		}
	}
	if result.Rows[0]["before_state"] != nil {
		t.Fatalf("create before_state = %#v, want nil", result.Rows[0]["before_state"])
	}

	var createdState task.Task
	if err := decodeSnapshot(result.Rows[0]["after_state"], &createdState); err != nil {
		t.Fatalf("decode create snapshot: %v", err)
	}
	if createdState.Title != "Keep history" || createdState.Description != "Initial description" || createdState.Status != task.StatusTodo {
		t.Fatalf("create snapshot = %#v", createdState)
	}
	var attachmentState task.Task
	if err := decodeSnapshot(result.Rows[1]["after_state"], &attachmentState); err != nil {
		t.Fatalf("decode attachment snapshot: %v", err)
	}
	if len(attachmentState.Attachments) != 1 || attachmentState.Attachments[0].Key != "audit-me/evidence.png" {
		t.Fatalf("attachment snapshot = %#v", attachmentState)
	}
	var completedState task.Task
	if err := decodeSnapshot(result.Rows[2]["after_state"], &completedState); err != nil {
		t.Fatalf("decode complete snapshot: %v", err)
	}
	if completedState.Status != task.StatusDone {
		t.Fatalf("complete snapshot status = %q, want done", completedState.Status)
	}
}

func decodeSnapshot(value any, destination any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, destination)
}

func TestRejectedMutationDoesNotRecordRevision(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ids := []string{"prerequisite", "blocked"}
	service := task.NewService(store, func() time.Time { return now }, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	prerequisite, err := service.Create(ctx, task.CreateInput{Title: "Prerequisite"})
	if err != nil {
		t.Fatalf("create prerequisite: %v", err)
	}
	blocked, err := service.Create(ctx, task.CreateInput{
		Title: "Blocked", Dependencies: []string{prerequisite.ID},
	})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}

	if _, err := service.Complete(ctx, blocked.ID); !errors.Is(err, task.ErrBlocked) {
		t.Fatalf("Complete error = %v, want ErrBlocked", err)
	}
	result, err := store.Query(ctx, `SELECT action FROM task_revisions WHERE task_id = 'blocked'`)
	if err != nil {
		t.Fatalf("query revisions: %v", err)
	}
	if len(result.Rows) != 1 || fmt.Sprint(result.Rows[0]["action"]) != "create" {
		t.Fatalf("blocked revisions = %#v, want only create", result.Rows)
	}
}

func TestStoreRollsBackTaskAndRevisionTogether(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	item := task.Task{
		ID: "atomic", Title: "Before", Status: task.StatusTodo, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Create(ctx, item); err != nil {
		t.Fatalf("Create: %v", err)
	}
	item.Title = "After"
	item.Dependencies = []string{"missing"}
	item.UpdatedAt = now.Add(time.Minute)
	item.Version = 2
	if err := store.Update(ctx, item); err == nil {
		t.Fatal("Update succeeded with missing dependency")
	}

	got, err := store.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Before" || len(got.Dependencies) != 0 {
		t.Fatalf("task after failed update = %#v", got)
	}
	result, err := store.Query(ctx, `SELECT action FROM task_revisions WHERE task_id = 'atomic'`)
	if err != nil {
		t.Fatalf("query revisions: %v", err)
	}
	if len(result.Rows) != 1 || fmt.Sprint(result.Rows[0]["action"]) != "create" {
		t.Fatalf("revisions after failed update = %#v, want only create", result.Rows)
	}
}

func TestCompletingDoneTaskDoesNotCreateEmptyRevision(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	service := task.NewService(store, func() time.Time {
		now = now.Add(time.Minute)
		return now
	}, func() string { return "once" })
	created, err := service.Create(ctx, task.CreateInput{Title: "Complete once"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := service.Complete(ctx, created.ID)
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	second, err := service.Complete(ctx, created.ID)
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("second completion updated_at = %v, want unchanged %v", second.UpdatedAt, first.UpdatedAt)
	}
	result, err := store.Query(ctx, `SELECT action FROM task_revisions WHERE task_id = 'once' ORDER BY version`)
	if err != nil {
		t.Fatalf("query revisions: %v", err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("revision count = %d, want 2; rows = %#v", len(result.Rows), result.Rows)
	}
}

func TestRevisionRecordsAuditAttribution(t *testing.T) {
	store := newStore(t)
	ctx := task.WithAuditMetadata(context.Background(), task.AuditMetadata{
		ActorKind: "oauth_client",
		ActorID:   "client-123",
		Source:    "mcp",
		RequestID: "request-456",
	})
	service := task.NewService(store, time.Now, func() string { return "attributed" })
	if _, err := service.Create(ctx, task.CreateInput{Title: "Attributed"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	result, err := store.Query(context.Background(), `
		SELECT actor_kind, actor_id, source, request_id
		FROM task_revisions
		WHERE task_id = 'attributed'
	`)
	if err != nil {
		t.Fatalf("query revision: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("revision rows = %#v", result.Rows)
	}
	row := result.Rows[0]
	if fmt.Sprint(row["actor_kind"]) != "oauth_client" ||
		fmt.Sprint(row["actor_id"]) != "client-123" ||
		fmt.Sprint(row["source"]) != "mcp" ||
		fmt.Sprint(row["request_id"]) != "request-456" {
		t.Fatalf("revision attribution = %#v", row)
	}
}

func TestOpenBackfillsBaselineRevisionForExistingTask(t *testing.T) {
	databaseURL := pgtest.URL(t)
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect legacy database: %v", err)
	}
	_, err = connection.Exec(ctx, `
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE dependencies (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			depends_on_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			PRIMARY KEY (task_id, depends_on_id)
		);
		CREATE TABLE images (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			object_key TEXT NOT NULL,
			name TEXT NOT NULL,
			content_type TEXT NOT NULL,
			PRIMARY KEY (task_id, object_key)
		);
		CREATE VIEW task_overview AS
		SELECT
			t.*,
			0 AS blocked,
			(SELECT count(*) FROM dependencies d WHERE d.task_id = t.id) AS dependency_count,
			(SELECT count(*) FROM images i WHERE i.task_id = t.id) AS image_count
		FROM tasks t;
		INSERT INTO tasks (id, title, description, status, created_at, updated_at)
		VALUES ('legacy', 'Existing task', 'Predates revisions', 'todo', now(), now());
	`)
	connection.Close(ctx)
	if err != nil {
		t.Fatalf("seed legacy task: %v", err)
	}

	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	result, err := store.Query(ctx, `
		SELECT version, action, actor_kind, source, before_state, after_state
		FROM task_revisions
		WHERE task_id = 'legacy'
	`)
	if err != nil {
		t.Fatalf("query baseline revision: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("baseline revisions = %#v, want one", result.Rows)
	}
	row := result.Rows[0]
	if fmt.Sprint(row["version"]) != "1" ||
		fmt.Sprint(row["action"]) != "import" ||
		fmt.Sprint(row["actor_kind"]) != "system" ||
		fmt.Sprint(row["source"]) != "migration" ||
		row["before_state"] != nil {
		t.Fatalf("baseline revision = %#v", row)
	}
	var snapshot task.Task
	if err := decodeSnapshot(row["after_state"], &snapshot); err != nil {
		t.Fatalf("decode baseline snapshot: %v", err)
	}
	if snapshot.ID != "legacy" || snapshot.Title != "Existing task" || snapshot.Description != "Predates revisions" {
		t.Fatalf("baseline snapshot = %#v", snapshot)
	}
}

func TestTaskRevisionsRejectMutation(t *testing.T) {
	for name, statement := range map[string]string{
		"update":   `UPDATE task_revisions SET action = 'tampered'`,
		"delete":   `DELETE FROM task_revisions`,
		"truncate": `TRUNCATE task_revisions`,
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			service := task.NewService(store, time.Now, func() string { return "immutable" })
			if _, err := service.Create(context.Background(), task.CreateInput{Title: "Immutable"}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := store.pool.Exec(context.Background(), statement); err == nil {
				t.Fatalf("%s succeeded, want immutable revision rejection", statement)
			}
		})
	}
}

func TestStoreRejectsStaleUpdateWithoutRecordingRevision(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	service := task.NewService(store, func() time.Time {
		now = now.Add(time.Minute)
		return now
	}, func() string { return "concurrent" })
	created, err := service.Create(ctx, task.CreateInput{Title: "Original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("created version = %d, want 1", created.Version)
	}

	first, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	stale := first
	first.Title = "First writer"
	first.UpdatedAt = first.UpdatedAt.Add(time.Minute)
	first.Version++
	if err := store.Update(ctx, first); err != nil {
		t.Fatalf("Update first: %v", err)
	}
	stale.Title = "Stale writer"
	stale.UpdatedAt = stale.UpdatedAt.Add(2 * time.Minute)
	stale.Version++
	if err := store.Update(ctx, stale); !errors.Is(err, task.ErrConflict) {
		t.Fatalf("stale Update error = %v, want ErrConflict", err)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get final: %v", err)
	}
	if got.Title != "First writer" || got.Version != 2 {
		t.Fatalf("task after stale update = %#v", got)
	}
	result, err := store.Query(ctx, `SELECT version FROM task_revisions WHERE task_id = 'concurrent' ORDER BY version`)
	if err != nil {
		t.Fatalf("query revisions: %v", err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("revision count = %d, want 2; rows = %#v", len(result.Rows), result.Rows)
	}
}

func TestStoreCreateRejectsNonInitialVersion(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	err := store.Create(context.Background(), task.Task{
		ID: "bad-version", Title: "Bad version", Status: task.StatusTodo,
		CreatedAt: now, UpdatedAt: now, Version: 7,
	})
	if !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("Create error = %v, want ErrInvalid", err)
	}
	result, queryErr := store.Query(context.Background(), `
		SELECT task_id FROM task_revisions WHERE task_id = 'bad-version'
	`)
	if queryErr != nil {
		t.Fatalf("query revisions: %v", queryErr)
	}
	if len(result.Rows) != 0 {
		t.Fatalf("revisions = %#v, want none", result.Rows)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestStoreRoundTripLoadsChildren(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)

	for _, id := range []string{"write-tests", "write-docs"} {
		if err := store.Create(ctx, task.Task{ID: id, Title: id, Description: "", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	want := task.Task{
		ID:           "ship-feature",
		Title:        "Ship feature",
		Description:  "Release it",
		Status:       task.StatusTodo,
		Dependencies: []string{"write-tests", "write-docs"},
		Attachments: []task.Attachment{
			{Key: "ship-feature/a.png", Name: "a.png", ContentType: "image/png"},
			{Key: "ship-feature/b.png", Name: "b.png", ContentType: "image/png"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := store.Get(ctx, "ship-feature")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != want.Title || got.Description != want.Description || got.Status != want.Status {
		t.Fatalf("scalar fields = %#v", got)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %v / %v", got.CreatedAt, got.UpdatedAt)
	}
	// Dependencies come back ordered by depends_on_id.
	if len(got.Dependencies) != 2 || got.Dependencies[0] != "write-docs" || got.Dependencies[1] != "write-tests" {
		t.Fatalf("dependencies = %v", got.Dependencies)
	}
	if len(got.Attachments) != 2 || got.Attachments[0].Key != "ship-feature/a.png" || got.Attachments[1].Key != "ship-feature/b.png" {
		t.Fatalf("attachments = %#v", got.Attachments)
	}
}

func TestGetAndListAttachOnlyOwnChildren(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)

	mustCreate(t, store, task.Task{ID: "dep-a", Title: "Dep A", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now})
	mustCreate(t, store, task.Task{ID: "dep-b", Title: "Dep B", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now})
	mustCreate(t, store, task.Task{
		ID: "first", Title: "First", Status: task.StatusTodo,
		Dependencies: []string{"dep-a"},
		Attachments:  []task.Attachment{{Key: "first/x.png", Name: "x.png", ContentType: "image/png"}},
		CreatedAt:    now, UpdatedAt: now,
	})
	mustCreate(t, store, task.Task{
		ID: "second", Title: "Second", Status: task.StatusTodo,
		Dependencies: []string{"dep-b"},
		Attachments:  []task.Attachment{{Key: "second/y.png", Name: "y.png", ContentType: "image/png"}},
		CreatedAt:    now, UpdatedAt: now,
	})

	// Get must attach only the requested task's children, not a sibling's.
	first, err := store.Get(ctx, "first")
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if len(first.Dependencies) != 1 || first.Dependencies[0] != "dep-a" {
		t.Fatalf("first dependencies = %v, want [dep-a]", first.Dependencies)
	}
	if len(first.Attachments) != 1 || first.Attachments[0].Key != "first/x.png" {
		t.Fatalf("first attachments = %#v, want only first/x.png", first.Attachments)
	}

	// List must attach each task's own children across the whole set.
	items, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := make(map[string]task.Task, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	if deps := byID["second"].Dependencies; len(deps) != 1 || deps[0] != "dep-b" {
		t.Fatalf("second dependencies = %v, want [dep-b]", deps)
	}
	if att := byID["second"].Attachments; len(att) != 1 || att[0].Key != "second/y.png" {
		t.Fatalf("second attachments = %#v, want only second/y.png", att)
	}
	if deps := byID["dep-a"].Dependencies; len(deps) != 0 {
		t.Fatalf("dep-a dependencies = %v, want none", deps)
	}
}

func TestStoreCreateDuplicateReturnsAlreadyExists(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	item := task.Task{ID: "dup", Title: "Dup", Status: task.StatusTodo, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(ctx, item); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := store.Create(ctx, item); !errors.Is(err, task.ErrAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrAlreadyExists", err)
	}
}

func TestStoreGetAndUpdateMissingReturnNotFound(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.Get(ctx, "nope"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("get err = %v, want ErrNotFound", err)
	}
	missing := task.Task{ID: "nope", Title: "Nope", Status: task.StatusTodo, UpdatedAt: time.Now()}
	if err := store.Update(ctx, missing); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("update err = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdateReplacesStatusAndChildren(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, task.Task{ID: "dep", Title: "Dep", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create dep: %v", err)
	}
	if err := store.Create(ctx, task.Task{
		ID: "main", Title: "Main", Status: task.StatusTodo,
		Dependencies: []string{"dep"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create main: %v", err)
	}
	updated := task.Task{
		ID: "main", Title: "Main", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now.Add(time.Hour), Version: 2,
	}
	if err := store.Update(ctx, updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.Get(ctx, "main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if len(got.Dependencies) != 0 {
		t.Fatalf("dependencies = %v, want cleared", got.Dependencies)
	}
}

func TestTasksOrdersTodoFirstThenNewest(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	mustCreate(t, store, task.Task{ID: "old-todo", Title: "Old todo", Status: task.StatusTodo, CreatedAt: base, UpdatedAt: base})
	mustCreate(t, store, task.Task{ID: "new-todo", Title: "New todo", Status: task.StatusTodo, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)})
	mustCreate(t, store, task.Task{ID: "done-task", Title: "Done", Status: task.StatusDone, CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)})

	items, err := store.Tasks(ctx)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	order := []string{items[0].ID, items[1].ID, items[2].ID}
	want := []string{"new-todo", "old-todo", "done-task"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("Tasks order = %v, want %v", order, want)
		}
	}
}

func TestQueryOverviewRejectsWritesAndCaps(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	mustCreate(t, store, task.Task{ID: "a", Title: "A", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now})
	mustCreate(t, store, task.Task{
		ID: "b", Title: "B", Status: task.StatusTodo, Dependencies: []string{"a"},
		Attachments: []task.Attachment{{Key: "b/x.png", Name: "x.png", ContentType: "image/png"}},
		CreatedAt:   now, UpdatedAt: now,
	})

	// task_overview exposes computed columns; b's only dependency is done, so
	// b is not blocked.
	overview, err := store.Query(ctx, "SELECT blocked, dependency_count, image_count FROM task_overview WHERE id = 'b'")
	if err != nil {
		t.Fatalf("query task_overview: %v", err)
	}
	row := overview.Rows[0]
	if fmt.Sprint(row["blocked"]) != "0" || fmt.Sprint(row["dependency_count"]) != "1" || fmt.Sprint(row["image_count"]) != "1" {
		t.Fatalf("task_overview row = %#v", row)
	}

	if _, err := store.Query(ctx, "DELETE FROM tasks"); err == nil {
		t.Fatal("DELETE accepted, want rejection")
	}
	if _, err := store.Query(ctx, "WITH gone AS (DELETE FROM tasks RETURNING id) SELECT * FROM gone"); err == nil {
		t.Fatal("CTE write accepted, want read-only transaction to block it")
	}

	store.SetMaxRows(1)
	capped, err := store.Query(ctx, "SELECT id FROM tasks")
	if err != nil {
		t.Fatalf("capped select: %v", err)
	}
	if !capped.Truncated || len(capped.Rows) != 1 {
		t.Fatalf("capped result = %#v, want 1 row truncated", capped)
	}
}

func mustCreate(t *testing.T, store *Store, item task.Task) {
	t.Helper()
	if err := store.Create(context.Background(), item); err != nil {
		t.Fatalf("create %s: %v", item.ID, err)
	}
}

func TestOAuthClientRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, ok, err := store.Client(ctx, "missing"); err != nil || ok {
		t.Fatalf("Client(missing) = ok %v, err %v, want (false, nil)", ok, err)
	}
	client := auth.Client{
		ID:           "client-1",
		Name:         "Claude",
		RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1/callback"},
	}
	if err := store.SaveClient(ctx, client); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	got, ok, err := store.Client(ctx, "client-1")
	if err != nil || !ok {
		t.Fatalf("Client = ok %v, err %v", ok, err)
	}
	if got.Name != client.Name || len(got.RedirectURIs) != 2 || got.RedirectURIs[0] != client.RedirectURIs[0] {
		t.Fatalf("client = %#v", got)
	}
}

func TestOAuthCodeSingleUse(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	future := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	code := auth.Code{ClientID: "c", RedirectURI: "https://x/cb", Challenge: "abc", Resource: "https://x/mcp", Scope: "tasks", ExpiresAt: future}
	if err := store.SaveCode(ctx, "codehash", code); err != nil {
		t.Fatalf("SaveCode: %v", err)
	}
	got, ok, err := store.TakeCode(ctx, "codehash")
	if err != nil || !ok {
		t.Fatalf("TakeCode first = ok %v, err %v", ok, err)
	}
	if got.ClientID != "c" || got.Scope != "tasks" || !got.ExpiresAt.Equal(future) {
		t.Fatalf("code = %#v", got)
	}
	if _, ok, err := store.TakeCode(ctx, "codehash"); err != nil || ok {
		t.Fatalf("TakeCode second = ok %v, err %v, want consumed", ok, err)
	}
}

func TestOAuthTokenRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	future := time.Date(2026, time.July, 20, 13, 0, 0, 0, time.UTC)
	if err := store.SaveToken(ctx, "tokhash", auth.Token{ClientID: "c", Resource: "https://x/mcp", Scope: "tasks", ExpiresAt: future}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	got, ok, err := store.Token(ctx, "tokhash")
	if err != nil || !ok {
		t.Fatalf("Token = ok %v, err %v", ok, err)
	}
	if got.Resource != "https://x/mcp" || !got.ExpiresAt.Equal(future) {
		t.Fatalf("token = %#v", got)
	}
	if _, ok, _ := store.Token(ctx, "nope"); ok {
		t.Fatal("Token(nope) ok = true, want false")
	}
}

func TestOAuthRefreshTokenRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	future := time.Date(2036, time.July, 20, 13, 0, 0, 0, time.UTC)
	if err := store.SaveRefreshToken(ctx, "refhash", auth.Token{ClientID: "c", Resource: "https://x/mcp", Scope: "tasks", ExpiresAt: future}); err != nil {
		t.Fatalf("SaveRefreshToken: %v", err)
	}
	got, ok, err := store.RefreshToken(ctx, "refhash")
	if err != nil || !ok {
		t.Fatalf("RefreshToken = ok %v, err %v", ok, err)
	}
	if got.ClientID != "c" || got.Resource != "https://x/mcp" || !got.ExpiresAt.Equal(future) {
		t.Fatalf("refresh token = %#v", got)
	}
	// Refresh tokens live in their own table, separate from access tokens.
	if _, ok, _ := store.Token(ctx, "refhash"); ok {
		t.Fatal("refresh token is also readable as an access token")
	}
	if _, ok, _ := store.RefreshToken(ctx, "nope"); ok {
		t.Fatal("RefreshToken(nope) ok = true, want false")
	}
}

func TestSessionRoundTripAndDelete(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	future := time.Date(2026, time.July, 20, 14, 0, 0, 0, time.UTC)
	if err := store.SaveSession(ctx, "sess", "csrf-1", future); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	csrf, expiresAt, ok, err := store.Session(ctx, "sess")
	if err != nil || !ok || csrf != "csrf-1" || !expiresAt.Equal(future) {
		t.Fatalf("Session = %q %v %v %v", csrf, expiresAt, ok, err)
	}
	if err := store.DeleteSession(ctx, "sess"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, ok, _ := store.Session(ctx, "sess"); ok {
		t.Fatal("session still present after delete")
	}
}

func TestDeleteExpiredAuthState(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	if err := store.SaveCode(ctx, "old-code", auth.Code{ClientID: "c", RedirectURI: "u", Challenge: "x", Resource: "r", Scope: "tasks", ExpiresAt: past}); err != nil {
		t.Fatalf("SaveCode: %v", err)
	}
	if err := store.SaveToken(ctx, "old-tok", auth.Token{ClientID: "c", Resource: "r", Scope: "tasks", ExpiresAt: past}); err != nil {
		t.Fatalf("SaveToken(old): %v", err)
	}
	if err := store.SaveSession(ctx, "old-sess", "csrf", past); err != nil {
		t.Fatalf("SaveSession(old): %v", err)
	}
	if err := store.SaveRefreshToken(ctx, "old-refresh", auth.Token{ClientID: "c", Resource: "r", Scope: "tasks", ExpiresAt: past}); err != nil {
		t.Fatalf("SaveRefreshToken(old): %v", err)
	}
	if err := store.SaveToken(ctx, "new-tok", auth.Token{ClientID: "c", Resource: "r", Scope: "tasks", ExpiresAt: future}); err != nil {
		t.Fatalf("SaveToken(new): %v", err)
	}
	if err := store.SaveRefreshToken(ctx, "new-refresh", auth.Token{ClientID: "c", Resource: "r", Scope: "tasks", ExpiresAt: future}); err != nil {
		t.Fatalf("SaveRefreshToken(new): %v", err)
	}
	if err := store.SaveSession(ctx, "new-sess", "csrf", future); err != nil {
		t.Fatalf("SaveSession(new): %v", err)
	}

	if err := store.DeleteExpiredAuthState(ctx, now); err != nil {
		t.Fatalf("DeleteExpiredAuthState: %v", err)
	}
	if _, ok, _ := store.Token(ctx, "old-tok"); ok {
		t.Fatal("expired token survived cleanup")
	}
	if _, _, ok, _ := store.Session(ctx, "old-sess"); ok {
		t.Fatal("expired session survived cleanup")
	}
	if _, ok, _ := store.RefreshToken(ctx, "old-refresh"); ok {
		t.Fatal("expired refresh token survived cleanup")
	}
	if _, ok, _ := store.Token(ctx, "new-tok"); !ok {
		t.Fatal("valid token removed by cleanup")
	}
	if _, ok, _ := store.RefreshToken(ctx, "new-refresh"); !ok {
		t.Fatal("valid refresh token removed by cleanup")
	}
	if _, _, ok, _ := store.Session(ctx, "new-sess"); !ok {
		t.Fatal("valid session removed by cleanup")
	}
}
