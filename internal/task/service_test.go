package task

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCreateAndCompleteTaskWithDependencies(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	ids := []string{"write-tests", "ship-feature"}
	service := NewService(repo, func() time.Time { return now }, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})

	first, err := service.Create(context.Background(), CreateInput{Title: "Write tests"})
	if err != nil {
		t.Fatalf("Create first task: %v", err)
	}
	second, err := service.Create(context.Background(), CreateInput{
		Title:        "Ship feature",
		Dependencies: []string{first.ID},
	})
	if err != nil {
		t.Fatalf("Create dependent task: %v", err)
	}

	if _, err := service.Complete(context.Background(), second.ID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Complete blocked task error = %v, want ErrBlocked", err)
	}
	if _, err := service.Complete(context.Background(), first.ID); err != nil {
		t.Fatalf("Complete dependency: %v", err)
	}
	completed, err := service.Complete(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("Complete dependent task: %v", err)
	}
	if completed.Status != StatusDone {
		t.Fatalf("status = %q, want %q", completed.Status, StatusDone)
	}
}

func TestStartTaskMovesItIntoProgress(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "build-board" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Build the board"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	now = now.Add(time.Hour)
	started, err := service.Start(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status != StatusInProgress {
		t.Fatalf("status = %q, want %q", started.Status, StatusInProgress)
	}
	if started.Version != 2 || !started.UpdatedAt.Equal(now) {
		t.Fatalf("started task version/time = %d/%v, want 2/%v", started.Version, started.UpdatedAt, now)
	}

	startedAgain, err := service.Start(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Start again: %v", err)
	}
	if startedAgain.Version != started.Version {
		t.Fatalf("repeated Start version = %d, want unchanged %d", startedAgain.Version, started.Version)
	}
}

func TestAgentSessionURLCanBeCreatedUpdatedAndCleared(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "linked" })
	firstURL := "cmux://workspace/00000000-0000-4000-8000-000000000001"
	created, err := service.Create(context.Background(), CreateInput{
		Title:           "Review the agent's work",
		AgentSessionURL: firstURL,
	})
	if err != nil {
		t.Fatalf("Create with agent session URL: %v", err)
	}
	if created.AgentSessionURL != firstURL {
		t.Fatalf("created agent session URL = %q, want %q", created.AgentSessionURL, firstURL)
	}

	secondURL := "cmux://workspace/00000000-0000-4000-8000-000000000002"
	now = now.Add(time.Hour)
	updated, err := service.Edit(context.Background(), created.ID, EditInput{AgentSessionURL: &secondURL})
	if err != nil {
		t.Fatalf("Edit agent session URL: %v", err)
	}
	if updated.AgentSessionURL != secondURL || updated.Version != 2 {
		t.Fatalf("updated task = %#v", updated)
	}

	empty := ""
	cleared, err := service.Edit(context.Background(), created.ID, EditInput{AgentSessionURL: &empty})
	if err != nil {
		t.Fatalf("Clear agent session URL: %v", err)
	}
	if cleared.AgentSessionURL != "" || cleared.Version != 3 {
		t.Fatalf("cleared task = %#v", cleared)
	}
}

func TestAgentSessionURLRejectsAnythingExceptAnExactCmuxWorkspaceLink(t *testing.T) {
	t.Parallel()

	for _, invalid := range []string{
		"https://example.com",
		"cmux://ssh?host=example.com",
		"cmux://prompt?text=run%20something",
		"cmux://workspace/not-a-uuid",
		"cmux://workspace/00000000-0000-4000-8000-000000000001?command=id",
		"cmux://workspace/00000000-0000-4000-8000-000000000001#pane",
		" cmux://workspace/00000000-0000-4000-8000-000000000001 ",
	} {
		t.Run(invalid, func(t *testing.T) {
			repo := newMemoryRepository()
			service := NewService(repo, time.Now, func() string { return "invalid" })
			if _, err := service.Create(context.Background(), CreateInput{
				Title: "Unsafe link", AgentSessionURL: invalid,
			}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Create error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestListOrdersTasksByWorkflowStateThenNewest(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	base := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	for _, item := range []Task{
		{ID: "done", Title: "Done", Status: StatusDone, CreatedAt: base.Add(4 * time.Hour)},
		{ID: "active", Title: "Active", Status: StatusInProgress, CreatedAt: base.Add(3 * time.Hour)},
		{ID: "older-todo", Title: "Older todo", Status: StatusTodo, CreatedAt: base},
		{ID: "newer-todo", Title: "Newer todo", Status: StatusTodo, CreatedAt: base.Add(time.Hour)},
	} {
		if err := repo.Create(context.Background(), item); err != nil {
			t.Fatalf("seed task %q: %v", item.ID, err)
		}
	}
	service := NewService(repo, time.Now, func() string { return "unused" })

	items, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(items))
	for index, item := range items {
		got[index] = item.ID
	}
	want := []string{"newer-todo", "older-todo", "active", "done"}
	if !slices.Equal(got, want) {
		t.Fatalf("task order = %v, want %v", got, want)
	}
}

func TestCreatePlacesNewTasksAtTheTopOfTodo(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "first", "second", "third")
	for _, title := range []string{"First", "Second", "Third"} {
		if _, err := service.Create(context.Background(), CreateInput{Title: title}); err != nil {
			t.Fatalf("Create %q: %v", title, err)
		}
	}

	if got, want := columnIDs(t, service, StatusTodo), []string{"third", "second", "first"}; !slices.Equal(got, want) {
		t.Fatalf("todo column = %v, want %v", got, want)
	}
}

func TestMoveReordersTasksWithinAColumn(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "first", "second", "third")
	seed(t, service, "First", "Second", "Third")
	// Newest first, so the column starts as third, second, first.

	if _, err := service.Move(context.Background(), "first", StatusTodo, 0); err != nil {
		t.Fatalf("Move to the top: %v", err)
	}
	if got, want := columnIDs(t, service, StatusTodo), []string{"first", "third", "second"}; !slices.Equal(got, want) {
		t.Fatalf("column after moving to the top = %v, want %v", got, want)
	}

	if _, err := service.Move(context.Background(), "first", StatusTodo, 1); err != nil {
		t.Fatalf("Move to the middle: %v", err)
	}
	if got, want := columnIDs(t, service, StatusTodo), []string{"third", "first", "second"}; !slices.Equal(got, want) {
		t.Fatalf("column after moving to the middle = %v, want %v", got, want)
	}

	// An index past the end lands at the bottom rather than failing.
	if _, err := service.Move(context.Background(), "third", StatusTodo, 99); err != nil {
		t.Fatalf("Move past the end: %v", err)
	}
	if got, want := columnIDs(t, service, StatusTodo), []string{"first", "second", "third"}; !slices.Equal(got, want) {
		t.Fatalf("column after moving past the end = %v, want %v", got, want)
	}
}

func TestMovePlacesTaskAtIndexInTargetColumn(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "first", "second", "third")
	seed(t, service, "First", "Second", "Third")

	for _, move := range []struct {
		id    string
		index int
	}{{"first", 0}, {"third", 1}, {"second", 1}} {
		if _, err := service.Move(context.Background(), move.id, StatusInProgress, move.index); err != nil {
			t.Fatalf("Move %q: %v", move.id, err)
		}
	}

	if got, want := columnIDs(t, service, StatusInProgress), []string{"first", "second", "third"}; !slices.Equal(got, want) {
		t.Fatalf("in-progress column = %v, want %v", got, want)
	}
	if got := columnIDs(t, service, StatusTodo); len(got) != 0 {
		t.Fatalf("todo column = %v, want empty", got)
	}
}

func TestMoveReopensCompletedTasks(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "revive")
	seed(t, service, "Revive me")
	if _, err := service.Complete(context.Background(), "revive"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	reopened, err := service.Move(context.Background(), "revive", StatusTodo, 0)
	if err != nil {
		t.Fatalf("Move back to todo: %v", err)
	}
	if reopened.Status != StatusTodo {
		t.Fatalf("status = %q, want %q", reopened.Status, StatusTodo)
	}
}

func TestMoveToDoneStillRespectsDependencies(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "prerequisite", "blocked")
	if _, err := service.Create(context.Background(), CreateInput{Title: "Prerequisite"}); err != nil {
		t.Fatalf("Create prerequisite: %v", err)
	}
	if _, err := service.Create(context.Background(), CreateInput{Title: "Blocked", Dependencies: []string{"prerequisite"}}); err != nil {
		t.Fatalf("Create blocked: %v", err)
	}

	if _, err := service.Move(context.Background(), "blocked", StatusDone, 0); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Move error = %v, want ErrBlocked", err)
	}
	stayed, err := service.Get(context.Background(), "blocked")
	if err != nil || stayed.Status != StatusTodo || stayed.Version != 1 {
		t.Fatalf("blocked task after rejected move = %#v, %v", stayed, err)
	}
}

func TestMoveRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "only")
	seed(t, service, "Only task")

	if _, err := service.Move(context.Background(), "only", Status("archived"), 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Move error = %v, want ErrInvalid", err)
	}
}

func TestMoveToTheSamePlaceRecordsNothing(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "first", "second")
	seed(t, service, "First", "Second")
	// The column reads second, first.

	before, err := service.Get(context.Background(), "first")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	after, err := service.Move(context.Background(), "first", StatusTodo, 1)
	if err != nil {
		t.Fatalf("Move in place: %v", err)
	}
	if after.Version != before.Version || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("in-place move = version %d at %v, want unchanged version %d at %v",
			after.Version, after.UpdatedAt, before.Version, before.UpdatedAt)
	}
}

func TestMoveRebalancesColumnWhenPositionsRunOut(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	base := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	// Adjacent float positions leave no value in between, and tasks imported
	// before positions existed all share position zero.
	for _, item := range []Task{
		{ID: "top", Title: "Top", Status: StatusTodo, Position: 1, CreatedAt: base, Version: 1},
		{ID: "bottom", Title: "Bottom", Status: StatusTodo, Position: math.Nextafter(1, 2), CreatedAt: base, Version: 1},
		{ID: "mover", Title: "Mover", Status: StatusInProgress, CreatedAt: base, Version: 1},
	} {
		if err := repo.Create(context.Background(), item); err != nil {
			t.Fatalf("seed %q: %v", item.ID, err)
		}
	}
	service := NewService(repo, func() time.Time { return base }, func() string { return "unused" })

	if _, err := service.Move(context.Background(), "mover", StatusTodo, 1); err != nil {
		t.Fatalf("Move between exhausted positions: %v", err)
	}
	if got, want := columnIDs(t, service, StatusTodo), []string{"top", "mover", "bottom"}; !slices.Equal(got, want) {
		t.Fatalf("column after rebalance = %v, want %v", got, want)
	}
	positions := map[string]float64{}
	items, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range items {
		positions[item.ID] = item.Position
	}
	if positions["top"] >= positions["mover"] || positions["mover"] >= positions["bottom"] {
		t.Fatalf("rebalanced positions are not strictly increasing: %#v", positions)
	}
}

func TestCreateRejectsUnknownDependency(t *testing.T) {
	t.Parallel()

	service := NewService(newMemoryRepository(), time.Now, func() string { return "new-task" })
	_, err := service.Create(context.Background(), CreateInput{
		Title:        "Blocked task",
		Dependencies: []string{"missing"},
	})
	if !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("Create error = %v, want ErrDependencyNotFound", err)
	}
}

func TestCreateRejectsBlankTitle(t *testing.T) {
	t.Parallel()

	service := NewService(newMemoryRepository(), time.Now, func() string { return "new-task" })
	if _, err := service.Create(context.Background(), CreateInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create error = %v, want ErrInvalid", err)
	}
}

func TestEditUpdatesFieldsAndAppliesGuardedTextReplacements(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	ids := []string{"dependency", "editable"}
	service := NewService(repo, func() time.Time { return now }, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	dependency, err := service.Create(context.Background(), CreateInput{Title: "Dependency"})
	if err != nil {
		t.Fatalf("Create dependency: %v", err)
	}
	created, err := service.Create(context.Background(), CreateInput{
		Title:       "Research sources",
		Description: "Find source. Summarize source.",
	})
	if err != nil {
		t.Fatalf("Create editable task: %v", err)
	}

	now = now.Add(time.Hour)
	title := "Research primary sources"
	description := "Find primary source. Summarize primary source."
	dependencies := []string{dependency.ID, dependency.ID, ""}
	expectedVersion := created.Version
	updated, err := service.Edit(context.Background(), created.ID, EditInput{
		Title:           &title,
		Description:     &description,
		Dependencies:    &dependencies,
		ExpectedVersion: &expectedVersion,
	})
	if err != nil {
		t.Fatalf("Edit fields: %v", err)
	}
	if updated.Title != title || updated.Description != description {
		t.Fatalf("updated text = %q / %q", updated.Title, updated.Description)
	}
	if !slices.Equal(updated.Dependencies, []string{dependency.ID}) {
		t.Fatalf("updated dependencies = %v", updated.Dependencies)
	}
	if updated.Version != 2 || !updated.UpdatedAt.Equal(now) {
		t.Fatalf("updated version/time = %d/%v, want 2/%v", updated.Version, updated.UpdatedAt, now)
	}

	now = now.Add(time.Hour)
	updated, err = service.Edit(context.Background(), created.ID, EditInput{
		Replacements: []TextReplacement{
			{Field: TextFieldDescription, OldText: "primary source", NewText: "primary source document", ReplaceAll: true},
			{Field: TextFieldTitle, OldText: "primary sources", NewText: "primary source documents"},
		},
	})
	if err != nil {
		t.Fatalf("Edit replacements: %v", err)
	}
	if updated.Title != "Research primary source documents" {
		t.Fatalf("replacement title = %q", updated.Title)
	}
	if updated.Description != "Find primary source document. Summarize primary source document." {
		t.Fatalf("replacement description = %q", updated.Description)
	}
	if updated.Version != 3 || !updated.UpdatedAt.Equal(now) {
		t.Fatalf("replacement version/time = %d/%v, want 3/%v", updated.Version, updated.UpdatedAt, now)
	}
}

func TestEditRejectsAmbiguousOrStaleChangesAtomically(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	service := NewService(repo, time.Now, func() string { return "editable" })
	created, err := service.Create(context.Background(), CreateInput{
		Title:       "Review sources",
		Description: "source and source",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = service.Edit(context.Background(), created.ID, EditInput{
		Replacements: []TextReplacement{
			{Field: TextFieldTitle, OldText: "Review", NewText: "Inspect"},
			{Field: TextFieldDescription, OldText: "source", NewText: "document"},
		},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous Edit error = %v, want ErrInvalid", err)
	}
	afterAmbiguous, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous edit: %v", err)
	}
	if afterAmbiguous.Title != created.Title || afterAmbiguous.Description != created.Description || afterAmbiguous.Version != created.Version {
		t.Fatalf("task changed after ambiguous edit: %#v", afterAmbiguous)
	}

	staleVersion := created.Version + 1
	title := "Stale title"
	_, err = service.Edit(context.Background(), created.ID, EditInput{
		Title:           &title,
		ExpectedVersion: &staleVersion,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Edit error = %v, want ErrConflict", err)
	}
}

func TestEditRejectsInvalidTitleAndDependencyCycles(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	ids := []string{"first", "second"}
	service := NewService(repo, time.Now, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	first, err := service.Create(context.Background(), CreateInput{Title: "First"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := service.Create(context.Background(), CreateInput{
		Title:        "Second",
		Dependencies: []string{first.ID},
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	blank := "   "
	if _, err := service.Edit(context.Background(), first.ID, EditInput{Title: &blank}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank title Edit error = %v, want ErrInvalid", err)
	}
	unknown := []string{"missing"}
	if _, err := service.Edit(context.Background(), first.ID, EditInput{Dependencies: &unknown}); !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("unknown dependency Edit error = %v, want ErrDependencyNotFound", err)
	}
	cycle := []string{second.ID}
	if _, err := service.Edit(context.Background(), first.ID, EditInput{Dependencies: &cycle}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cyclic dependency Edit error = %v, want ErrInvalid", err)
	}
}

func TestEditRequiresARequestedChangeAndSkipsNoOpWrites(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	service := NewService(repo, time.Now, func() string { return "editable" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Same"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.Edit(context.Background(), created.ID, EditInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty Edit error = %v, want ErrInvalid", err)
	}
	sameTitle := created.Title
	unchanged, err := service.Edit(context.Background(), created.ID, EditInput{Title: &sameTitle})
	if err != nil {
		t.Fatalf("no-op Edit: %v", err)
	}
	if unchanged.Version != created.Version || !unchanged.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("no-op Edit changed version/time: %#v", unchanged)
	}
}

func TestDeleteHidesTaskAndRestoreBringsItBack(t *testing.T) {
	t.Parallel()

	service, repo := boardService(t, "keeper", "gone")
	seed(t, service, "Keeper", "Gone")
	// Newest first, so the todo column reads gone, keeper.
	if _, err := service.Move(context.Background(), "gone", StatusInProgress, 0); err != nil {
		t.Fatalf("Move: %v", err)
	}
	before, err := service.Get(context.Background(), "gone")
	if err != nil {
		t.Fatalf("Get before delete: %v", err)
	}

	deleted, err := service.Delete(context.Background(), "gone")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.DeletedAt == nil {
		t.Fatalf("deleted task = %#v, want a deletion timestamp", deleted)
	}
	if deleted.Version != before.Version+1 {
		t.Fatalf("deleted version = %d, want %d", deleted.Version, before.Version+1)
	}
	if _, err := service.Get(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
	if got, want := columnIDs(t, service, StatusInProgress), []string{}; !slices.Equal(got, want) {
		t.Fatalf("in-progress column after delete = %v, want %v", got, want)
	}
	// Soft delete keeps the row, so the history and the task itself survive.
	stored, err := repo.Get(context.Background(), "gone")
	if err != nil {
		t.Fatalf("stored task after delete: %v", err)
	}
	if stored.Title != "Gone" || stored.Status != StatusInProgress {
		t.Fatalf("stored task = %#v, want the task preserved", stored)
	}

	restored, err := service.Restore(context.Background(), "gone")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.DeletedAt != nil {
		t.Fatalf("restored task = %#v, want no deletion timestamp", restored)
	}
	if restored.Status != before.Status || restored.Position != before.Position {
		t.Fatalf("restored place = %q/%v, want %q/%v", restored.Status, restored.Position, before.Status, before.Position)
	}
	if restored.Version != deleted.Version+1 {
		t.Fatalf("restored version = %d, want %d", restored.Version, deleted.Version+1)
	}
	if got, want := columnIDs(t, service, StatusInProgress), []string{"gone"}; !slices.Equal(got, want) {
		t.Fatalf("in-progress column after restore = %v, want %v", got, want)
	}
}

func TestDeleteAndRestoreAreIdempotent(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "only")
	seed(t, service, "Only task")

	deleted, err := service.Delete(context.Background(), "only")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	again, err := service.Delete(context.Background(), "only")
	if err != nil {
		t.Fatalf("Delete already deleted: %v", err)
	}
	if again.Version != deleted.Version {
		t.Fatalf("repeated Delete version = %d, want unchanged %d", again.Version, deleted.Version)
	}
	restored, err := service.Restore(context.Background(), "only")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	stillThere, err := service.Restore(context.Background(), "only")
	if err != nil {
		t.Fatalf("Restore a live task: %v", err)
	}
	if stillThere.Version != restored.Version {
		t.Fatalf("repeated Restore version = %d, want unchanged %d", stillThere.Version, restored.Version)
	}
	if _, err := service.Delete(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing task error = %v, want ErrNotFound", err)
	}
	if _, err := service.Restore(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Restore missing task error = %v, want ErrNotFound", err)
	}
}

func TestDeleteRejectsTaskLiveTasksDependOn(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "prerequisite", "dependent")
	if _, err := service.Create(context.Background(), CreateInput{Title: "Prerequisite"}); err != nil {
		t.Fatalf("Create prerequisite: %v", err)
	}
	if _, err := service.Create(context.Background(), CreateInput{Title: "Dependent", Dependencies: []string{"prerequisite"}}); err != nil {
		t.Fatalf("Create dependent: %v", err)
	}

	if _, err := service.Delete(context.Background(), "prerequisite"); !errors.Is(err, ErrHasDependents) {
		t.Fatalf("Delete error = %v, want ErrHasDependents", err)
	}
	untouched, err := service.Get(context.Background(), "prerequisite")
	if err != nil || untouched.Version != 1 {
		t.Fatalf("prerequisite after rejected delete = %#v, %v", untouched, err)
	}
	// Deleting the dependent first frees the prerequisite.
	if _, err := service.Delete(context.Background(), "dependent"); err != nil {
		t.Fatalf("Delete dependent: %v", err)
	}
	if _, err := service.Delete(context.Background(), "prerequisite"); err != nil {
		t.Fatalf("Delete freed prerequisite: %v", err)
	}
}

func TestRestoreRequiresLiveDependencies(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "prerequisite", "dependent")
	if _, err := service.Create(context.Background(), CreateInput{Title: "Prerequisite"}); err != nil {
		t.Fatalf("Create prerequisite: %v", err)
	}
	if _, err := service.Create(context.Background(), CreateInput{Title: "Dependent", Dependencies: []string{"prerequisite"}}); err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	for _, id := range []string{"dependent", "prerequisite"} {
		if _, err := service.Delete(context.Background(), id); err != nil {
			t.Fatalf("Delete %q: %v", id, err)
		}
	}

	if _, err := service.Restore(context.Background(), "dependent"); !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("Restore error = %v, want ErrDependencyNotFound", err)
	}
	if _, err := service.Restore(context.Background(), "prerequisite"); err != nil {
		t.Fatalf("Restore prerequisite: %v", err)
	}
	if _, err := service.Restore(context.Background(), "dependent"); err != nil {
		t.Fatalf("Restore dependent after its dependency: %v", err)
	}
}

func TestDeletedTasksAreInvisibleToEveryOtherOperation(t *testing.T) {
	t.Parallel()

	service, _ := boardService(t, "gone", "live")
	seed(t, service, "Gone", "Live")
	if _, err := service.Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	title := "Renamed"
	for name, operation := range map[string]func() error{
		"get":      func() error { _, err := service.Get(context.Background(), "gone"); return err },
		"start":    func() error { _, err := service.Start(context.Background(), "gone"); return err },
		"complete": func() error { _, err := service.Complete(context.Background(), "gone"); return err },
		"move":     func() error { _, err := service.Move(context.Background(), "gone", StatusDone, 0); return err },
		"edit": func() error {
			_, err := service.Edit(context.Background(), "gone", EditInput{Title: &title})
			return err
		},
		"add attachment": func() error {
			_, err := service.AddAttachment(context.Background(), "gone", Attachment{Key: "k", Name: "n"})
			return err
		},
	} {
		if err := operation(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s on a deleted task error = %v, want ErrNotFound", name, err)
		}
	}

	// A deleted task can no longer be named as a dependency.
	if _, err := service.Create(context.Background(), CreateInput{Title: "New", Dependencies: []string{"gone"}}); !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("Create depending on a deleted task error = %v, want ErrDependencyNotFound", err)
	}
	dependencies := []string{"gone"}
	if _, err := service.Edit(context.Background(), "live", EditInput{Dependencies: &dependencies}); !errors.Is(err, ErrDependencyNotFound) {
		t.Fatalf("Edit depending on a deleted task error = %v, want ErrDependencyNotFound", err)
	}
}

// boardService returns a service whose clock advances a minute per call and
// whose IDs come from ids in order, so board ordering assertions stay readable.
func boardService(t *testing.T, ids ...string) (*Service, *memoryRepository) {
	t.Helper()
	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	next := 0
	service := NewService(repo, func() time.Time {
		now = now.Add(time.Minute)
		return now
	}, func() string {
		if next >= len(ids) {
			t.Fatalf("test requested more than %d task IDs", len(ids))
		}
		id := ids[next]
		next++
		return id
	})
	return service, repo
}

func seed(t *testing.T, service *Service, titles ...string) {
	t.Helper()
	for _, title := range titles {
		if _, err := service.Create(context.Background(), CreateInput{Title: title}); err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
	}
}

func columnIDs(t *testing.T, service *Service, status Status) []string {
	t.Helper()
	items, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Status == status {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

type memoryRepository struct {
	tasks map[string]Task
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{tasks: make(map[string]Task)}
}

func (r *memoryRepository) Create(_ context.Context, task Task) error {
	if _, ok := r.tasks[task.ID]; ok {
		return ErrAlreadyExists
	}
	r.tasks[task.ID] = task
	return nil
}

func (r *memoryRepository) Update(_ context.Context, task Task) error {
	if _, ok := r.tasks[task.ID]; !ok {
		return ErrNotFound
	}
	r.tasks[task.ID] = task
	return nil
}

func (r *memoryRepository) Get(_ context.Context, id string) (Task, error) {
	task, ok := r.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return task, nil
}

func (r *memoryRepository) List(_ context.Context) ([]Task, error) {
	tasks := make([]Task, 0, len(r.tasks))
	for _, task := range r.tasks {
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func TestSnoozedReportsWhetherTheWakeDateHasArrived(t *testing.T) {
	t.Parallel()

	wake := time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)
	item := Task{WakeAt: &wake}
	if !item.Snoozed(wake.Add(-time.Minute)) {
		t.Fatal("a task with a future wake date should be snoozed")
	}
	if item.Snoozed(wake) {
		t.Fatal("a task should wake exactly at its wake date")
	}
	if item.Snoozed(wake.Add(time.Hour)) {
		t.Fatal("a task past its wake date should be awake")
	}
	if (Task{}).Snoozed(wake) {
		t.Fatal("a task with no wake date should never be snoozed")
	}
}

func TestEditSetsAndClearsTheWait(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "licence-signoff" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Priya, Tom & Rae sign off"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.WakeAt != nil || created.WaitingOn != "" || created.SnoozeCount != 0 {
		t.Fatalf("new task should start awake, got %#v", created)
	}

	wake := time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)
	waitingOn := "Priya, Tom & Rae (asked Mar 4)"
	onTimeout := "Send one final reminder, then proceed without approval"
	slept, err := service.Edit(context.Background(), created.ID, EditInput{
		WakeAt: &wake, WaitingOn: &waitingOn, OnTimeout: &onTimeout,
	})
	if err != nil {
		t.Fatalf("Edit to sleep: %v", err)
	}
	if slept.WakeAt == nil || !slept.WakeAt.Equal(wake) {
		t.Fatalf("wake date = %v, want %v", slept.WakeAt, wake)
	}
	if slept.WaitingOn != waitingOn {
		t.Fatalf("waiting on = %q, want %q", slept.WaitingOn, waitingOn)
	}
	if slept.OnTimeout != onTimeout {
		t.Fatalf("on timeout = %q, want %q", slept.OnTimeout, onTimeout)
	}
	if !slept.Snoozed(now) {
		t.Fatal("task should be snoozed right after it is put to sleep")
	}
	if slept.SnoozeCount != 1 {
		t.Fatalf("snooze count = %d, want 1", slept.SnoozeCount)
	}

	// Re-supplying the same wait changes nothing and must not count as a snooze.
	same, err := service.Edit(context.Background(), created.ID, EditInput{
		WakeAt: &wake, WaitingOn: &waitingOn, OnTimeout: &onTimeout,
	})
	if err != nil {
		t.Fatalf("Edit with an unchanged wait: %v", err)
	}
	if same.Version != slept.Version || same.SnoozeCount != 1 {
		t.Fatalf("unchanged wait bumped the task: version %d, snoozes %d", same.Version, same.SnoozeCount)
	}

	var cleared time.Time
	empty := ""
	awake, err := service.Edit(context.Background(), created.ID, EditInput{
		WakeAt: &cleared, WaitingOn: &empty, OnTimeout: &empty,
	})
	if err != nil {
		t.Fatalf("Edit to wake: %v", err)
	}
	if awake.WakeAt != nil || awake.WaitingOn != "" || awake.OnTimeout != "" {
		t.Fatalf("clearing the wait left %#v", awake)
	}
	if awake.Snoozed(now) {
		t.Fatal("a cleared wait should leave the task awake")
	}
	if awake.SnoozeCount != 0 {
		t.Fatalf("snooze count = %d, want 0 after the wait is cleared", awake.SnoozeCount)
	}
}

func TestRepeatedSnoozesAccumulate(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "nudge-vendor" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Nudge the vendor"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		wake := now.AddDate(0, 0, attempt)
		edited, err := service.Edit(context.Background(), created.ID, EditInput{WakeAt: &wake})
		if err != nil {
			t.Fatalf("Edit snooze %d: %v", attempt, err)
		}
		if edited.SnoozeCount != attempt {
			t.Fatalf("snooze count after %d snoozes = %d", attempt, edited.SnoozeCount)
		}
	}
}

func TestEditTrimsWaitingOn(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "trim-wait" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Trim"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	padded := "  Priya Raman  "
	wake := now.AddDate(0, 0, 3)
	edited, err := service.Edit(context.Background(), created.ID, EditInput{WakeAt: &wake, WaitingOn: &padded})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if edited.WaitingOn != "Priya Raman" {
		t.Fatalf("waiting on = %q, want trimmed", edited.WaitingOn)
	}
}

func TestWaitOnSomeoneRequiresAReviewDateOrDependency(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	ids := []string{"reviewer", "follow-up"}
	service := NewService(repo, func() time.Time { return now }, func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	reviewer, err := service.Create(context.Background(), CreateInput{Title: "Review the draft"})
	if err != nil {
		t.Fatalf("Create reviewer: %v", err)
	}

	waitingOn := "the reviewer"
	if _, err := service.Create(context.Background(), CreateInput{
		Title: "Follow up", WaitingOn: waitingOn,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create error = %v, want ErrInvalid", err)
	}

	followUp, err := service.Create(context.Background(), CreateInput{
		Title: "Follow up", Dependencies: []string{reviewer.ID}, WaitingOn: waitingOn,
	})
	if err != nil {
		t.Fatalf("Create dependency-backed wait: %v", err)
	}
	if followUp.WaitingOn != waitingOn {
		t.Fatalf("waiting on = %q", followUp.WaitingOn)
	}

	emptyDependencies := []string{}
	if _, err := service.Edit(context.Background(), followUp.ID, EditInput{
		Dependencies: &emptyDependencies,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("removing the only review trigger error = %v, want ErrInvalid", err)
	}
}

func TestTaskContextIsNormalizedAndCheckedIndependently(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "home-task" })
	checkedInOffset := time.Date(2026, time.July, 23, 9, 30, 0, 0, time.FixedZone("UTC-4", -4*60*60))
	created, err := service.Create(context.Background(), CreateInput{
		Title:            "Replace the filter",
		Context:          "  @Home  ",
		ContextCheckedAt: &checkedInOffset,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Context != "home" {
		t.Fatalf("context = %q, want home", created.Context)
	}
	if created.ContextCheckedAt == nil ||
		created.ContextCheckedAt.Location() != time.UTC ||
		!created.ContextCheckedAt.Equal(checkedInOffset) {
		t.Fatalf("context checked at = %v, want %v in UTC", created.ContextCheckedAt, checkedInOffset)
	}

	title := "Replace the air filter"
	edited, err := service.Edit(context.Background(), created.ID, EditInput{Title: &title})
	if err != nil {
		t.Fatalf("Edit title: %v", err)
	}
	if edited.ContextCheckedAt == nil || !edited.ContextCheckedAt.Equal(checkedInOffset) {
		t.Fatalf("an unrelated edit changed context checked at to %v", edited.ContextCheckedAt)
	}

	emptyContext := ""
	var cleared time.Time
	edited, err = service.Edit(context.Background(), created.ID, EditInput{
		Context: &emptyContext, ContextCheckedAt: &cleared,
	})
	if err != nil {
		t.Fatalf("clear context metadata: %v", err)
	}
	if edited.Context != "" || edited.ContextCheckedAt != nil {
		t.Fatalf("clearing context metadata left %#v", edited)
	}
}

func TestTaskContextRejectsMultilineOrOverlongValues(t *testing.T) {
	t.Parallel()

	service := NewService(newMemoryRepository(), time.Now, func() string { return "invalid-context" })
	for name, contextName := range map[string]string{
		"multiline": "home\nshop",
		"overlong":  strings.Repeat("x", 65),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Create(context.Background(), CreateInput{
				Title: "Scoped task", Context: contextName,
			}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Create error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestEditStoresTheWakeDateInUTC(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "utc-wait" })
	created, err := service.Create(context.Background(), CreateInput{Title: "UTC"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	zone := time.FixedZone("UTC-4", -4*60*60)
	wake := time.Date(2026, time.July, 27, 9, 0, 0, 0, zone)
	edited, err := service.Edit(context.Background(), created.ID, EditInput{WakeAt: &wake})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if edited.WakeAt.Location() != time.UTC || !edited.WakeAt.Equal(wake) {
		t.Fatalf("wake date = %v, want the same instant in UTC", edited.WakeAt)
	}
}

func TestEditClonesTheWakeDate(t *testing.T) {
	t.Parallel()

	repo := newMemoryRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := NewService(repo, func() time.Time { return now }, func() string { return "clone-wait" })
	created, err := service.Create(context.Background(), CreateInput{Title: "Clone"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wake := time.Date(2026, time.July, 27, 9, 0, 0, 0, time.UTC)
	edited, err := service.Edit(context.Background(), created.ID, EditInput{WakeAt: &wake})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	*edited.WakeAt = edited.WakeAt.AddDate(0, 0, 10)
	reread, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reread.WakeAt.Equal(wake) {
		t.Fatalf("stored wake date = %v, want %v; the pointer is shared", reread.WakeAt, wake)
	}
}
