package markdown

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zachlatta/task-tracker/internal/task"
	"gopkg.in/yaml.v3"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type Store struct {
	directory string
}

func NewStore(directory string) *Store {
	return &Store{directory: directory}
}

func (s *Store) Create(ctx context.Context, item task.Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(item.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return task.ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.write(path, item)
}

func (s *Store) Update(ctx context.Context, item task.Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(item.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return task.ErrNotFound
	} else if err != nil {
		return err
	}
	return s.write(path, item)
}

func (s *Store) Get(ctx context.Context, id string) (task.Task, error) {
	if err := ctx.Err(); err != nil {
		return task.Task{}, err
	}
	path, err := s.path(id)
	if err != nil {
		return task.Task{}, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return task.Task{}, task.ErrNotFound
	}
	if err != nil {
		return task.Task{}, err
	}
	return decode(contents)
}

func (s *Store) List(ctx context.Context) ([]task.Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.directory)
	if errors.Is(err, os.ErrNotExist) {
		return []task.Task{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]task.Task, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(s.directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		item, err := decode(contents)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) path(id string) (string, error) {
	if !safeID.MatchString(id) {
		return "", fmt.Errorf("%w: unsafe task ID %q", task.ErrInvalid, id)
	}
	return filepath.Join(s.directory, id+".md"), nil
}

func (s *Store) write(path string, item task.Task) error {
	if err := os.MkdirAll(s.directory, 0o750); err != nil {
		return err
	}
	header, err := yaml.Marshal(item)
	if err != nil {
		return err
	}
	var contents bytes.Buffer
	contents.WriteString("---\n")
	contents.Write(header)
	contents.WriteString("---\n")
	contents.WriteString(item.Description)
	if item.Description != "" && !strings.HasSuffix(item.Description, "\n") {
		contents.WriteByte('\n')
	}
	temporary, err := os.CreateTemp(s.directory, ".task-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o640); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents.Bytes()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func decode(contents []byte) (task.Task, error) {
	const opening = "---\n"
	if !bytes.HasPrefix(contents, []byte(opening)) {
		return task.Task{}, errors.New("missing YAML front matter")
	}
	parts := bytes.SplitN(contents[len(opening):], []byte("\n---\n"), 2)
	if len(parts) != 2 {
		return task.Task{}, errors.New("unterminated YAML front matter")
	}
	var item task.Task
	if err := yaml.Unmarshal(parts[0], &item); err != nil {
		return task.Task{}, err
	}
	if !safeID.MatchString(item.ID) || strings.TrimSpace(item.Title) == "" {
		return task.Task{}, fmt.Errorf("%w: invalid task metadata", task.ErrInvalid)
	}
	item.Description = string(parts[1])
	return item, nil
}
