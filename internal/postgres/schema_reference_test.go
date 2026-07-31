// The schema reference published to agents through the MCP tool description
// must never drift from the real database schema, or it becomes worse than no
// documentation. This test lives in an external test package because taskapi
// imports postgres.
package postgres_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/zachlatta/tasks/internal/pgtest"
	"github.com/zachlatta/tasks/internal/postgres"
	"github.com/zachlatta/tasks/internal/taskapi"
)

func TestSchemaReferenceMatchesDatabase(t *testing.T) {
	store, err := postgres.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	for relation, documented := range taskapi.SchemaColumns {
		result, err := store.Query(context.Background(), fmt.Sprintf(
			"SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = '%s'",
			relation,
		))
		if err != nil {
			t.Fatalf("list columns of %s: %v", relation, err)
		}
		actual := make([]string, 0, len(result.Rows))
		for _, row := range result.Rows {
			actual = append(actual, fmt.Sprint(row["column_name"]))
		}
		// Existing production databases grew columns through ALTERs, so
		// ordinal positions differ between fresh and migrated schemas.
		// Compare membership, not order.
		wantSorted := slices.Clone(documented)
		slices.Sort(wantSorted)
		slices.Sort(actual)
		if !slices.Equal(actual, wantSorted) {
			t.Errorf("%s columns = %v, documented as %v", relation, actual, wantSorted)
		}
	}

	// The reference must cover every relation the reader role can see, so a
	// new table cannot ship undocumented.
	listing, err := store.Query(context.Background(),
		"SELECT DISTINCT table_name FROM information_schema.columns WHERE table_schema = 'public'")
	if err != nil {
		t.Fatalf("list relations: %v", err)
	}
	for _, row := range listing.Rows {
		name := fmt.Sprint(row["table_name"])
		if _, ok := taskapi.SchemaColumns[name]; !ok {
			t.Errorf("relation %s is readable by agents but undocumented in taskapi.SchemaColumns", name)
		}
	}
}
