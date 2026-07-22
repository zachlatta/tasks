// Package postgres implements the canonical task storage on PostgreSQL. It
// satisfies task.Repository for writes and exposes read-only SQL plus a fixed
// task projection for the CLI, web UI, and trusted MCP agents against the same
// tables.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zachlatta/task-tracker/internal/task"
)

const defaultMaxRows = 500

// Store is the PostgreSQL-backed source of truth for tasks.
type Store struct {
	pool    *pgxpool.Pool
	maxRows int
}

// Result is a read-only query result returned to the CLI and MCP agents.
type Result struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Truncated bool             `json:"truncated"`
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	description TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('todo', 'done')),
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS dependencies (
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	depends_on_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	PRIMARY KEY (task_id, depends_on_id)
);
CREATE TABLE IF NOT EXISTS images (
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
	object_key TEXT NOT NULL,
	name TEXT NOT NULL,
	content_type TEXT NOT NULL,
	PRIMARY KEY (task_id, object_key)
);
CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks(status);
CREATE INDEX IF NOT EXISTS dependencies_depends_on_idx ON dependencies(depends_on_id);
CREATE OR REPLACE VIEW task_overview AS
SELECT
	t.*,
	CASE WHEN EXISTS (
		SELECT 1
		FROM dependencies d
		JOIN tasks prerequisite ON prerequisite.id = d.depends_on_id
		WHERE d.task_id = t.id AND prerequisite.status <> 'done'
	) THEN 1 ELSE 0 END AS blocked,
	(SELECT count(*) FROM dependencies d WHERE d.task_id = t.id) AS dependency_count,
	(SELECT count(*) FROM images i WHERE i.task_id = t.id) AS image_count
FROM tasks t;
`

// Open connects to PostgreSQL, ensures the task schema exists, and returns a
// ready Store. Callers must Close it.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	store := &Store{pool: pool, maxRows: defaultMaxRows}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("initialize task schema: %w", err)
	}
	return store, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// SetMaxRows caps how many rows a read-only query returns before truncating.
func (s *Store) SetMaxRows(maximum int) {
	if maximum > 0 {
		s.maxRows = maximum
	}
}

// Create inserts a new task and its dependencies and images atomically.
func (s *Store) Create(ctx context.Context, item task.Task) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tasks (id, title, description, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, item.ID, item.Title, item.Description, string(item.Status), item.CreatedAt, item.UpdatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				return task.ErrAlreadyExists
			}
			return err
		}
		return insertChildren(ctx, tx, item)
	})
}

// Update overwrites an existing task's mutable fields, dependencies, and images.
// It reports task.ErrNotFound when the task does not exist.
func (s *Store) Update(ctx context.Context, item task.Task) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks
			SET title = $2, description = $3, status = $4, updated_at = $5
			WHERE id = $1
		`, item.ID, item.Title, item.Description, string(item.Status), item.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return task.ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dependencies WHERE task_id = $1`, item.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM images WHERE task_id = $1`, item.ID); err != nil {
			return err
		}
		return insertChildren(ctx, tx, item)
	})
}

// Get loads a task by ID, including its dependencies and images. It reports
// task.ErrNotFound when the task does not exist.
func (s *Store) Get(ctx context.Context, id string) (task.Task, error) {
	var item task.Task
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, description, status, created_at, updated_at
		FROM tasks WHERE id = $1
	`, id).Scan(&item.ID, &item.Title, &item.Description, &status, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return task.Task{}, task.ErrNotFound
	}
	if err != nil {
		return task.Task{}, err
	}
	item.Status = task.Status(status)
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	items := []task.Task{item}
	if err := s.attachChildren(ctx, items, map[string]int{id: 0}); err != nil {
		return task.Task{}, err
	}
	return items[0], nil
}

// List returns every task with its dependencies and images. Ordering of the
// tasks themselves is left to the caller.
func (s *Store) List(ctx context.Context) ([]task.Task, error) {
	return s.projection(ctx, `
		SELECT id, title, description, status, created_at, updated_at FROM tasks
	`)
}

// Tasks returns every task ordered todo-first then newest-first, matching the
// fixed projection the web UI renders.
func (s *Store) Tasks(ctx context.Context) ([]task.Task, error) {
	return s.projection(ctx, `
		SELECT id, title, description, status, created_at, updated_at
		FROM tasks
		ORDER BY CASE status WHEN 'todo' THEN 0 ELSE 1 END, created_at DESC
	`)
}

func (s *Store) projection(ctx context.Context, taskQuery string) ([]task.Task, error) {
	rows, err := s.pool.Query(ctx, taskQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]task.Task, 0)
	index := make(map[string]int)
	for rows.Next() {
		var item task.Task
		var status string
		if err := rows.Scan(&item.ID, &item.Title, &item.Description, &status, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Status = task.Status(status)
		item.CreatedAt = item.CreatedAt.UTC()
		item.UpdatedAt = item.UpdatedAt.UTC()
		index[item.ID] = len(items)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachChildren(ctx, items, index); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) attachChildren(ctx context.Context, items []task.Task, index map[string]int) error {
	if len(index) == 0 {
		return nil
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	dependencyRows, err := s.pool.Query(ctx, `
		SELECT task_id, depends_on_id FROM dependencies
		WHERE task_id = ANY($1)
		ORDER BY task_id, depends_on_id
	`, ids)
	if err != nil {
		return err
	}
	defer dependencyRows.Close()
	for dependencyRows.Next() {
		var taskID, dependencyID string
		if err := dependencyRows.Scan(&taskID, &dependencyID); err != nil {
			return err
		}
		if position, ok := index[taskID]; ok {
			items[position].Dependencies = append(items[position].Dependencies, dependencyID)
		}
	}
	if err := dependencyRows.Err(); err != nil {
		return err
	}
	imageRows, err := s.pool.Query(ctx, `
		SELECT task_id, object_key, name, content_type FROM images
		WHERE task_id = ANY($1)
		ORDER BY task_id, object_key
	`, ids)
	if err != nil {
		return err
	}
	defer imageRows.Close()
	for imageRows.Next() {
		var taskID string
		var attachment task.Attachment
		if err := imageRows.Scan(&taskID, &attachment.Key, &attachment.Name, &attachment.ContentType); err != nil {
			return err
		}
		if position, ok := index[taskID]; ok {
			items[position].Attachments = append(items[position].Attachments, attachment)
		}
	}
	return imageRows.Err()
}

// Query runs a trusted, read-only SQL statement against the task tables. It
// enforces both a statement-prefix allowlist and a read-only transaction so a
// write can never slip through a CTE.
func (s *Store) Query(ctx context.Context, statement string) (Result, error) {
	statement = strings.TrimSpace(statement)
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return Result{}, errors.New("SQL query is required")
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "WITH", "EXPLAIN":
	default:
		return Result{}, errors.New("only read-only SELECT, WITH, and EXPLAIN statements are allowed")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 5000"); err != nil {
		return Result{}, err
	}
	rows, err := tx.Query(ctx, statement)
	if err != nil {
		return Result{}, fmt.Errorf("execute read-only SQL: %w", err)
	}
	defer rows.Close()
	descriptions := rows.FieldDescriptions()
	columns := make([]string, len(descriptions))
	for i, description := range descriptions {
		columns[i] = description.Name
	}
	result := Result{Columns: columns, Rows: make([]map[string]any, 0)}
	for rows.Next() {
		if len(result.Rows) == s.maxRows {
			result.Truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return Result{}, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = normalize(values[i])
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertChildren(ctx context.Context, tx pgx.Tx, item task.Task) error {
	for _, dependency := range item.Dependencies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO dependencies (task_id, depends_on_id) VALUES ($1, $2)
		`, item.ID, dependency); err != nil {
			return fmt.Errorf("store dependency for %s: %w", item.ID, err)
		}
	}
	for _, attachment := range item.Attachments {
		if _, err := tx.Exec(ctx, `
			INSERT INTO images (task_id, object_key, name, content_type) VALUES ($1, $2, $3, $4)
		`, item.ID, attachment.Key, attachment.Name, attachment.ContentType); err != nil {
			return fmt.Errorf("store image for %s: %w", item.ID, err)
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func normalize(value any) any {
	if bytes, ok := value.([]byte); ok {
		return string(bytes)
	}
	return value
}
