package objectstore

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestLocalStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := NewLocal(t.TempDir())
	if err := store.Put(context.Background(), "task/image.png", strings.NewReader("png"), 3, "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reader, contentType, err := store.Open(context.Background(), "task/image.png")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()
	contents, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(contents) != "png" || contentType != "image/png" {
		t.Fatalf("contents = %q, content type = %q", contents, contentType)
	}
	if err := store.Delete(context.Background(), "task/image.png"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestLocalStoreRejectsUnsafeKey(t *testing.T) {
	t.Parallel()

	store := NewLocal(t.TempDir())
	if err := store.Put(context.Background(), "../escape", strings.NewReader("bad"), 3, "text/plain"); err == nil {
		t.Fatal("Put unsafe key succeeded, want error")
	}
}
