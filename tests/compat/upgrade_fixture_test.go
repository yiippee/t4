package compat_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/t4db/t4"
	"github.com/t4db/t4/internal/checkpoint"
	"github.com/t4db/t4/pkg/object"
)

const baseline = "v0.19.1"

type fixtureMetadata struct {
	Baseline         string `json:"baseline"`
	BranchID         string `json:"branch_id"`
	BranchCheckpoint string `json:"branch_checkpoint"`
}

func TestPreviousReleaseLocalDataDirOpens(t *testing.T) {
	dir := extractFixture(t, baseline, "local-data.tar.gz")
	node, err := t4.Open(t4.Config{
		DataDir:            dir,
		CheckpointInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("open old local data dir: %v", err)
	}
	defer func() { _ = node.Close() }()

	assertValue(t, node, "/compat/local/a", "old-local-a")
	assertValue(t, node, "/compat/local/b", "old-local-b")
}

func TestPreviousReleaseObjectStoreRestoresAndReplays(t *testing.T) {
	meta := readMetadata(t, baseline)
	store := loadObjectFixture(t, baseline)

	entries, err := checkpoint.New(nil).ReadBranchEntries(context.Background(), store)
	if err != nil {
		t.Fatalf("read old branch registry: %v", err)
	}
	if got := entries[meta.BranchID].AncestorCheckpointKey; got != meta.BranchCheckpoint {
		t.Fatalf("branch checkpoint: got %q, want %q", got, meta.BranchCheckpoint)
	}

	node, err := t4.Open(t4.Config{
		DataDir:            t.TempDir(),
		ObjectStore:        store,
		CheckpointInterval: time.Hour,
		SegmentMaxAge:      time.Hour,
	})
	if err != nil {
		t.Fatalf("open from old object store: %v", err)
	}
	defer func() { _ = node.Close() }()

	assertValue(t, node, "/compat/checkpoint/a", "old-checkpoint-a")
	assertValue(t, node, "/compat/checkpoint/b", "old-checkpoint-b")
	assertValue(t, node, "/compat/wal/after", "old-wal-after")
}

func TestPreviousReleaseBranchCheckpointRestores(t *testing.T) {
	meta := readMetadata(t, baseline)
	source := loadObjectFixture(t, baseline)
	branchStore := object.NewMem()

	node, err := t4.Open(t4.Config{
		DataDir:            t.TempDir(),
		ObjectStore:        branchStore,
		BranchPoint:        &t4.BranchPoint{SourceStore: source, CheckpointKey: meta.BranchCheckpoint},
		AncestorStore:      source,
		CheckpointInterval: time.Hour,
		SegmentMaxAge:      time.Hour,
	})
	if err != nil {
		t.Fatalf("open branch from old checkpoint: %v", err)
	}
	defer func() { _ = node.Close() }()

	assertValue(t, node, "/compat/checkpoint/a", "old-checkpoint-a")
	assertValue(t, node, "/compat/checkpoint/b", "old-checkpoint-b")
}

func assertValue(t *testing.T, node *t4.Node, key, want string) {
	t.Helper()
	got, err := node.Get(key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if string(got.Value) != want {
		t.Fatalf("get %q: got %q, want %q", key, got.Value, want)
	}
}

func readMetadata(t *testing.T, version string) fixtureMetadata {
	t.Helper()
	path := filepath.Join("testdata", version, "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var meta fixtureMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if meta.Baseline != version {
		t.Fatalf("fixture baseline: got %q, want %q", meta.Baseline, version)
	}
	return meta
}

func extractFixture(t *testing.T, version, name string) string {
	t.Helper()
	dst := t.TempDir()
	path := filepath.Join("testdata", version, name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", path, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar %s: %v", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(dst, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, dst+string(os.PathSeparator)) {
			t.Fatalf("unsafe fixture path %q", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir fixture target: %v", err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("create fixture target: %v", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			t.Fatalf("copy fixture target: %v", err)
		}
		if err := out.Close(); err != nil {
			t.Fatalf("close fixture target: %v", err)
		}
	}
	return dst
}

func loadObjectFixture(t *testing.T, version string) *fileStore {
	t.Helper()
	return &fileStore{root: extractFixture(t, version, "object-store.tar.gz")}
}

type fileStore struct {
	root string
}

func (s *fileStore) Put(_ context.Context, key string, r io.Reader) error {
	path := filepath.Join(s.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *fileStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, object.ErrNotFound
	}
	return f, err
}

func (s *fileStore) Delete(_ context.Context, key string) error {
	if err := os.Remove(filepath.Join(s.root, filepath.FromSlash(key))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *fileStore) DeleteMany(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if err := s.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *fileStore) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}
