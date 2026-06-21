package t4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/t4db/t4/internal/metrics"
	"github.com/t4db/t4/pkg/object"
)

func TestMakeUploaderClosesLocalFileAfterUpload(t *testing.T) {
	metrics.Register(prometheus.NewRegistry())

	dir := t.TempDir()
	localPath := filepath.Join(dir, "segment.wal")
	if err := os.WriteFile(localPath, []byte("wal-data"), 0o600); err != nil {
		t.Fatalf("write local segment: %v", err)
	}

	store := object.NewMem()
	uploader := makeUploader(store, NoopLogger)
	if err := uploader(context.Background(), localPath, "wal/test-segment"); err != nil {
		t.Fatalf("upload local segment: %v", err)
	}

	if err := os.Remove(localPath); err != nil {
		t.Fatalf("local segment should be closed after upload: %v", err)
	}

	rc, err := store.Get(context.Background(), "wal/test-segment")
	if err != nil {
		t.Fatalf("uploaded object missing: %v", err)
	}
	_ = rc.Close()
}
