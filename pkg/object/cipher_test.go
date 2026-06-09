package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func testKeyProvider(t *testing.T, b byte) *StaticKeyProvider {
	t.Helper()
	kp, err := NewStaticKeyProvider(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

func encryptedMem(t *testing.T) (Store, *Mem) {
	t.Helper()
	mem := NewMem()
	return NewEncryptedStore(mem, testKeyProvider(t, 0x42)), mem
}

func rawObject(t *testing.T, mem *Mem, key string) []byte {
	t.Helper()
	rc, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNewStaticKeyProviderRejectsBadLength(t *testing.T) {
	if _, err := NewStaticKeyProvider(make([]byte, 16)); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestEncryptedStoreRoundTrip(t *testing.T) {
	es, mem := encryptedMem(t)
	ctx := context.Background()
	plain := bytes.Repeat([]byte("secret data "), 7000)

	if err := es.Put(ctx, "wal/0001.wal", bytes.NewReader(plain)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	raw := rawObject(t, mem, "wal/0001.wal")
	if bytes.Contains(raw, plain[:32]) {
		t.Fatal("plaintext fragment found in stored ciphertext")
	}
	rc, err := es.Get(ctx, "wal/0001.wal")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestEncryptedStoreEmptyObject(t *testing.T) {
	es, _ := encryptedMem(t)
	ctx := context.Background()
	if err := es.Put(ctx, "empty", bytes.NewReader(nil)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := es.Get(ctx, "empty")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bytes, want empty object", len(got))
	}
}

func TestEncryptedStoreWrongKeyFails(t *testing.T) {
	mem := NewMem()
	ctx := context.Background()
	if err := NewEncryptedStore(mem, testKeyProvider(t, 0x01)).Put(ctx, "k", strings.NewReader("value")); err != nil {
		t.Fatal(err)
	}
	rc, err := NewEncryptedStore(mem, testKeyProvider(t, 0x02)).Get(ctx, "k")
	if err != nil {
		return
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("expected authentication failure")
	}
}

func TestEncryptedStoreTruncatedFinalFrameFails(t *testing.T) {
	es, mem := encryptedMem(t)
	ctx := context.Background()
	if err := es.Put(ctx, "k", strings.NewReader("value")); err != nil {
		t.Fatal(err)
	}
	raw := rawObject(t, mem, "k")
	if len(raw) <= encHeaderLen+4+encTagSize {
		t.Fatalf("encrypted object unexpectedly short: %d", len(raw))
	}
	if err := mem.Put(ctx, "k", bytes.NewReader(raw[:len(raw)-encTagSize])); err != nil {
		t.Fatal(err)
	}
	rc, err := es.Get(ctx, "k")
	if err != nil {
		return
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("expected truncated final frame to fail")
	}
}

func TestEncryptedStoreAuthenticatesObjectKey(t *testing.T) {
	es, mem := encryptedMem(t)
	ctx := context.Background()
	if err := es.Put(ctx, "a", strings.NewReader("value")); err != nil {
		t.Fatal(err)
	}
	raw := rawObject(t, mem, "a")
	if err := mem.Put(ctx, "b", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	rc, err := es.Get(ctx, "b")
	if err != nil {
		return
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("expected object-key substitution to fail")
	}
}

func TestEncryptedStoreConditionalOperations(t *testing.T) {
	es, ok := NewEncryptedStore(NewMem(), testKeyProvider(t, 0x42)).(ConditionalStore)
	if !ok {
		t.Fatal("encrypted Mem should preserve ConditionalStore")
	}
	ctx := context.Background()
	if err := es.PutIfAbsent(ctx, "k", strings.NewReader("v1")); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if err := es.PutIfAbsent(ctx, "k", strings.NewReader("v2")); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("got %v, want ErrPreconditionFailed", err)
	}
	got, err := es.GetETag(ctx, "k")
	if err != nil {
		t.Fatalf("GetETag: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1" || got.ETag == "" {
		t.Fatalf("unexpected GetETag result body=%q etag=%q", body, got.ETag)
	}
	if err := es.PutIfMatch(ctx, "k", strings.NewReader("v3"), got.ETag); err != nil {
		t.Fatalf("PutIfMatch: %v", err)
	}
}

func TestEncryptedStoreDeleteMany(t *testing.T) {
	es, _ := encryptedMem(t)
	ctx := context.Background()
	for _, key := range []string{"a", "b"} {
		if err := es.Put(ctx, key, strings.NewReader("v")); err != nil {
			t.Fatal(err)
		}
	}
	if err := es.DeleteMany(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	keys, err := es.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("got keys %v, want none", keys)
	}
}

func TestEncryptedStoreOptionalInterfaces(t *testing.T) {
	if _, ok := NewEncryptedStore(noopStore{}, testKeyProvider(t, 0x42)).(ConditionalStore); ok {
		t.Fatal("noop store should not grow ConditionalStore")
	}
	if _, ok := NewEncryptedStore(noopStore{}, testKeyProvider(t, 0x42)).(VersionedStore); ok {
		t.Fatal("noop store should not grow VersionedStore")
	}

	var inner versionedMem
	wrapped := NewEncryptedStore(&inner, testKeyProvider(t, 0x42))
	if _, ok := wrapped.(ConditionalStore); !ok {
		t.Fatal("encrypted versionedMem should preserve ConditionalStore")
	}
	if _, ok := wrapped.(VersionedStore); !ok {
		t.Fatal("encrypted versionedMem should preserve VersionedStore")
	}
	if _, ok := NewInstrumentedStore(&inner).(VersionedStore); !ok {
		t.Fatal("instrumented versionedMem should preserve VersionedStore")
	}
}

type noopStore struct{}

func (noopStore) Put(context.Context, string, io.Reader) error       { return nil }
func (noopStore) Get(context.Context, string) (io.ReadCloser, error) { return nil, ErrNotFound }
func (noopStore) Delete(context.Context, string) error               { return nil }
func (noopStore) DeleteMany(context.Context, []string) error         { return nil }
func (noopStore) List(context.Context, string) ([]string, error)     { return nil, nil }

type versionedMem struct {
	*Mem
}

func (m *versionedMem) ensure() {
	if m.Mem == nil {
		m.Mem = NewMem()
	}
}

func (m *versionedMem) Put(ctx context.Context, key string, r io.Reader) error {
	m.ensure()
	return m.Mem.Put(ctx, key, r)
}

func (m *versionedMem) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.ensure()
	return m.Mem.Get(ctx, key)
}

func (m *versionedMem) GetETag(ctx context.Context, key string) (*GetWithETag, error) {
	m.ensure()
	return m.Mem.GetETag(ctx, key)
}

func (m *versionedMem) PutIfAbsent(ctx context.Context, key string, r io.Reader) error {
	m.ensure()
	return m.Mem.PutIfAbsent(ctx, key, r)
}

func (m *versionedMem) PutIfMatch(ctx context.Context, key string, r io.Reader, etag string) error {
	m.ensure()
	return m.Mem.PutIfMatch(ctx, key, r, etag)
}

func (m *versionedMem) Delete(ctx context.Context, key string) error {
	m.ensure()
	return m.Mem.Delete(ctx, key)
}

func (m *versionedMem) DeleteMany(ctx context.Context, keys []string) error {
	m.ensure()
	return m.Mem.DeleteMany(ctx, keys)
}

func (m *versionedMem) List(ctx context.Context, prefix string) ([]string, error) {
	m.ensure()
	return m.Mem.List(ctx, prefix)
}

func (m *versionedMem) GetVersioned(ctx context.Context, key, versionID string) (io.ReadCloser, error) {
	m.ensure()
	return m.Mem.Get(ctx, key)
}
