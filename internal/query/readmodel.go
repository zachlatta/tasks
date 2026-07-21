package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zachlatta/task-tracker/internal/task"
	_ "modernc.org/sqlite"
)

const defaultMaxRows = 500

type ReadModel struct {
	database *sql.DB
	maxRows  atomic.Int64
}

type Result struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Truncated bool             `json:"truncated"`
}

func Open(path string) (*ReadModel, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Keep PRAGMA settings on one physical connection. MCP queries are short and
	// the read model is only a local projection of the Markdown source of truth.
	database.SetMaxOpenConns(1)
	model := &ReadModel{database: database}
	model.maxRows.Store(defaultMaxRows)
	if _, err := database.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
		DROP VIEW IF EXISTS task_overview;
		DROP TABLE IF EXISTS task_attachments;
		DROP TABLE IF EXISTS task_dependencies;
		CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('todo', 'done')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
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
		CREATE VIEW task_overview AS
		SELECT
			t.*,
			CASE WHEN EXISTS (
				SELECT 1
				FROM dependencies d
				JOIN tasks prerequisite ON prerequisite.id = d.depends_on_id
				WHERE d.task_id = t.id AND prerequisite.status != 'done'
			) THEN 1 ELSE 0 END AS blocked,
			(SELECT count(*) FROM dependencies d WHERE d.task_id = t.id) AS dependency_count,
			(SELECT count(*) FROM images i WHERE i.task_id = t.id) AS image_count
		FROM tasks t;
	`); err != nil {
		database.Close()
		return nil, fmt.Errorf("initialize task read model: %w", err)
	}
	return model, nil
}

func (m *ReadModel) Close() error {
	return m.database.Close()
}

func (m *ReadModel) SetMaxRows(maximum int) {
	if maximum > 0 {
		m.maxRows.Store(int64(maximum))
	}
}

func (m *ReadModel) Sync(ctx context.Context, items []task.Task) error {
	transaction, err := m.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for _, statement := range []string{"DELETE FROM images", "DELETE FROM dependencies", "DELETE FROM tasks"} {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("reset task read model: %w", err)
		}
	}
	for _, item := range items {
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO tasks (id, title, description, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, item.ID, item.Title, item.Description, item.Status, item.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), item.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")); err != nil {
			return fmt.Errorf("index task %s: %w", item.ID, err)
		}
	}
	for _, item := range items {
		for _, dependency := range item.Dependencies {
			if _, err := transaction.ExecContext(ctx, `INSERT INTO dependencies (task_id, depends_on_id) VALUES (?, ?)`, item.ID, dependency); err != nil {
				return fmt.Errorf("index dependency for %s: %w", item.ID, err)
			}
		}
		for _, attachment := range item.Attachments {
			if _, err := transaction.ExecContext(ctx, `
				INSERT INTO images (task_id, object_key, name, content_type) VALUES (?, ?, ?, ?)
			`, item.ID, attachment.Key, attachment.Name, attachment.ContentType); err != nil {
				return fmt.Errorf("index attachment for %s: %w", item.ID, err)
			}
		}
	}
	return transaction.Commit()
}

func (m *ReadModel) Tasks(ctx context.Context) ([]task.Task, error) {
	result, err := m.query(ctx, `
		SELECT id, title, description, status, created_at, updated_at
		FROM tasks
		ORDER BY CASE status WHEN 'todo' THEN 0 ELSE 1 END, created_at DESC
	`, 0)
	if err != nil {
		return nil, err
	}
	items := make([]task.Task, 0, len(result.Rows))
	byID := make(map[string]*task.Task, len(result.Rows))
	for _, row := range result.Rows {
		createdAt, err := time.Parse(time.RFC3339Nano, row["created_at"].(string))
		if err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, row["updated_at"].(string))
		if err != nil {
			return nil, fmt.Errorf("parse updated_at: %w", err)
		}
		items = append(items, task.Task{
			ID: row["id"].(string), Title: row["title"].(string), Description: row["description"].(string),
			Status: task.Status(row["status"].(string)), CreatedAt: createdAt, UpdatedAt: updatedAt,
		})
		byID[items[len(items)-1].ID] = &items[len(items)-1]
	}
	dependencies, err := m.query(ctx, "SELECT task_id, depends_on_id FROM dependencies ORDER BY task_id, depends_on_id", 0)
	if err != nil {
		return nil, err
	}
	for _, row := range dependencies.Rows {
		if item := byID[row["task_id"].(string)]; item != nil {
			item.Dependencies = append(item.Dependencies, row["depends_on_id"].(string))
		}
	}
	images, err := m.query(ctx, "SELECT task_id, object_key, name, content_type FROM images ORDER BY task_id, object_key", 0)
	if err != nil {
		return nil, err
	}
	for _, row := range images.Rows {
		if item := byID[row["task_id"].(string)]; item != nil {
			item.Attachments = append(item.Attachments, task.Attachment{
				Key: row["object_key"].(string), Name: row["name"].(string), ContentType: row["content_type"].(string),
			})
		}
	}
	return items, nil
}

func (m *ReadModel) Query(ctx context.Context, statement string) (Result, error) {
	return m.query(ctx, statement, int(m.maxRows.Load()))
}

func (m *ReadModel) query(ctx context.Context, statement string, maximum int) (Result, error) {
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
	connection, err := m.database.Conn(ctx)
	if err != nil {
		return Result{}, err
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return Result{}, fmt.Errorf("enable SQLite query-only mode: %w", err)
	}
	defer connection.ExecContext(context.WithoutCancel(ctx), "PRAGMA query_only = OFF")
	rows, err := connection.QueryContext(ctx, statement)
	if err != nil {
		return Result{}, fmt.Errorf("execute read-only SQL: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return Result{}, err
	}
	result := Result{Columns: columns, Rows: make([]map[string]any, 0)}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return Result{}, err
		}
		if maximum > 0 && len(result.Rows) == maximum {
			result.Truncated = true
			break
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			if bytes, ok := values[i].([]byte); ok {
				row[column] = string(bytes)
			} else {
				row[column] = values[i]
			}
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}
