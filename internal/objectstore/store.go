package objectstore

import (
	"context"
	"io"
)

type Store interface {
	Put(ctx context.Context, key string, contents io.Reader, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, string, error)
	Delete(ctx context.Context, key string) error
}
