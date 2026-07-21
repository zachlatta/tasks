package objectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Local struct {
	directory string
}

type metadata struct {
	ContentType string `json:"content_type"`
}

func NewLocal(directory string) *Local {
	return &Local{directory: directory}
}

func (s *Local) Put(ctx context.Context, key string, contents io.Reader, size int64, contentType string) error {
	objectPath, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(objectPath), ".object-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, err := io.Copy(temporary, &contextReader{ctx: ctx, reader: contents})
	if err != nil {
		temporary.Close()
		return err
	}
	if size >= 0 && written != size {
		temporary.Close()
		return fmt.Errorf("object size is %d bytes, expected %d", written, size)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, objectPath); err != nil {
		return err
	}
	encoded, err := json.Marshal(metadata{ContentType: contentType})
	if err != nil {
		return err
	}
	if err := os.WriteFile(objectPath+".metadata.json", encoded, 0o640); err != nil {
		_ = os.Remove(objectPath)
		return err
	}
	return nil
}

func (s *Local) Open(ctx context.Context, key string) (io.ReadCloser, string, error) {
	objectPath, err := s.objectPath(key)
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	encoded, err := os.ReadFile(objectPath + ".metadata.json")
	if err != nil {
		return nil, "", err
	}
	var details metadata
	if err := json.Unmarshal(encoded, &details); err != nil {
		return nil, "", err
	}
	reader, err := os.Open(objectPath)
	if err != nil {
		return nil, "", err
	}
	return reader, details.ContentType, nil
}

func (s *Local) Delete(ctx context.Context, key string) error {
	objectPath, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, target := range []string{objectPath, objectPath + ".metadata.json"} {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Local) objectPath(key string) (string, error) {
	if !validKey(key) {
		return "", fmt.Errorf("unsafe object key %q", key)
	}
	return filepath.Join(s.directory, filepath.FromSlash(key)), nil
}

func validKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && !strings.HasPrefix(key, "../") && !strings.Contains(key, "/../") && !strings.Contains(key, `\`) && path.Clean(key) == key && key != "."
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
