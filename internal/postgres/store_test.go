package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/pgtest"
	"github.com/zachlatta/tasks/internal/task"
)

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
	updated := task.Task{ID: "main", Title: "Main", Status: task.StatusDone, CreatedAt: now, UpdatedAt: now.Add(time.Hour)}
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
