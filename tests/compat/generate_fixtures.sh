#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/tests/compat/testdata"
BASELINE="${1:-v0.19.1}"
WORK_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/t4-compat-fixtures.XXXXXX")"

cleanup() {
  if [[ -d "$WORK_ROOT/src" ]]; then
    git -C "$ROOT" worktree remove --force "$WORK_ROOT/src" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_ROOT"
}
trap cleanup EXIT

mkdir -p "$OUT/$BASELINE"
git -C "$ROOT" worktree add --detach "$WORK_ROOT/src" "$BASELINE"

mkdir -p "$WORK_ROOT/src/.compatgen"
cat >"$WORK_ROOT/src/.compatgen/main.go" <<'GO'
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/t4db/t4"
	"github.com/t4db/t4/pkg/object"
)

type fileStore struct {
	root string
}

func (s fileStore) Put(_ context.Context, key string, r io.Reader) error {
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

func (s fileStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, object.ErrNotFound
	}
	return f, err
}

func (s fileStore) Delete(_ context.Context, key string) error {
	if err := os.Remove(filepath.Join(s.root, filepath.FromSlash(key))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s fileStore) DeleteMany(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if err := s.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (s fileStore) List(_ context.Context, prefix string) ([]string, error) {
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

func main() {
	if len(os.Args) != 2 {
		panic("usage: compatgen <output-dir>")
	}
	out := os.Args[1]
	ctx := context.Background()
	work, err := os.MkdirTemp("", "t4-compat-generate-*")
	must(err)
	defer os.RemoveAll(work)

	localDir := filepath.Join(work, "local-data")
	local, err := t4.Open(t4.Config{DataDir: localDir})
	must(err)
	_, err = local.Put(ctx, "/compat/local/a", []byte("old-local-a"), 0)
	must(err)
	_, err = local.Put(ctx, "/compat/local/b", []byte("old-local-b"), 0)
	must(err)
	must(local.Close())

	objectDir := filepath.Join(work, "object-store")
	must(os.MkdirAll(objectDir, 0o755))
	store := fileStore{root: objectDir}
	objNode, err := t4.Open(t4.Config{
		DataDir:            filepath.Join(work, "object-node"),
		ObjectStore:        store,
		CheckpointInterval: 25 * time.Millisecond,
		CheckpointEntries:  2,
		SegmentMaxAge:      25 * time.Millisecond,
	})
	must(err)
	_, err = objNode.Put(ctx, "/compat/checkpoint/a", []byte("old-checkpoint-a"), 0)
	must(err)
	_, err = objNode.Put(ctx, "/compat/checkpoint/b", []byte("old-checkpoint-b"), 0)
	must(err)
	must(waitManifestAtLeast(ctx, store, 2))
	branchKey, err := t4.Fork(ctx, store, "compat-branch")
	must(err)
	_, err = objNode.Put(ctx, "/compat/wal/after", []byte("old-wal-after"), 0)
	must(err)
	must(objNode.Close())

	meta := map[string]string{
		"baseline":            os.Getenv("T4_COMPAT_BASELINE"),
		"branch_id":           "compat-branch",
		"branch_checkpoint":   branchKey,
		"local_key_a":         "old-local-a",
		"local_key_b":         "old-local-b",
		"checkpoint_key_a":    "old-checkpoint-a",
		"checkpoint_key_b":    "old-checkpoint-b",
		"wal_replay_key":      "old-wal-after",
		"checkpoint_prefix":   "/compat/checkpoint/",
		"wal_replay_key_path": "/compat/wal/after",
	}

	must(os.MkdirAll(out, 0o755))
	must(writeJSON(filepath.Join(out, "metadata.json"), meta))
	must(tarGz(filepath.Join(out, "local-data.tar.gz"), localDir))
	must(tarGz(filepath.Join(out, "object-store.tar.gz"), objectDir))
}

func waitManifestAtLeast(ctx context.Context, store fileStore, rev int64) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rc, err := store.Get(ctx, "manifest/latest")
		if err == nil {
			var m struct {
				Revision int64 `json:"revision"`
			}
			decodeErr := json.NewDecoder(rc).Decode(&m)
			_ = rc.Close()
			if decodeErr == nil && m.Revision >= rev {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for manifest revision >= %d", rev)
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func tarGz(dst, src string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Mode = 0o644
		hdr.ModTime = time.Unix(0, 0)
		hdr.AccessTime = time.Unix(0, 0)
		hdr.ChangeTime = time.Unix(0, 0)
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, in); err != nil {
			_ = in.Close()
			return err
		}
		return in.Close()
	})
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
GO

(
  cd "$WORK_ROOT/src"
  T4_COMPAT_BASELINE="$BASELINE" go run ./.compatgen "$OUT/$BASELINE"
)
