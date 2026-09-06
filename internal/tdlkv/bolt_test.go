package tdlkv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/iyear/tdl/core/storage"
)

// newFixture writes a bolt file laid out the way tdl's driver lays one out:
// file named after the namespace, single bucket of the same name.
func newFixture(t *testing.T, dir, ns string, pairs map[string]string) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(dir, ns), 0o600, nil)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(ns))
		if err != nil {
			return err
		}
		for k, v := range pairs {
			if err := b.Put([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
}

func TestOpenReadsTdlLayout(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, "default", map[string]string{"app": "desktop"})

	s, err := Open(dir, "default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.Get(context.Background(), "app")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "desktop" {
		t.Errorf("Get(app) = %q, want %q", got, "desktop")
	}
}

// A missing key must be distinguishable from an error, because core/storage
// consumers branch on ErrNotFound to fall back to defaults.
func TestGetMissingKeyReturnsErrNotFound(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, "default", nil)

	s, err := Open(dir, "default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Get(context.Background(), "absent"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get(absent) error = %v, want storage.ErrNotFound", err)
	}
}

func TestSetAndDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, "default", nil)

	s, err := Open(dir, "default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	if err := s.Set(ctx, "session", []byte("blob")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "session")
	if err != nil || string(got) != "blob" {
		t.Fatalf("Get after Set = %q, %v", got, err)
	}
	if err := s.Delete(ctx, "session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "session"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get after Delete error = %v, want storage.ErrNotFound", err)
	}
}

// The three failure modes must stay distinguishable in the message, because
// each one tells the operator to do something different.
func TestOpenFailureMessages(t *testing.T) {
	t.Run("missing namespace file", func(t *testing.T) {
		_, err := Open(t.TempDir(), "default")
		if err == nil {
			t.Fatal("expected an error for a missing session file")
		}
		if !strings.Contains(err.Error(), "tdl login") {
			t.Errorf("error should point at `tdl login`, got: %v", err)
		}
	})

	t.Run("non-default namespace names the flag", func(t *testing.T) {
		_, err := Open(t.TempDir(), "second")
		if err == nil {
			t.Fatal("expected an error for a missing session file")
		}
		if !strings.Contains(err.Error(), "-n second") {
			t.Errorf("error should name the namespace flag, got: %v", err)
		}
	})

	t.Run("file present but bucket missing", func(t *testing.T) {
		dir := t.TempDir()
		db, err := bbolt.Open(filepath.Join(dir, "default"), 0o600, nil)
		if err != nil {
			t.Fatalf("create empty db: %v", err)
		}
		_ = db.Close()

		if _, err := Open(dir, "default"); err == nil {
			t.Fatal("expected an error for a bucketless store")
		} else if !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("error should call the session incomplete, got: %v", err)
		}
	})

	t.Run("empty namespace rejected", func(t *testing.T) {
		if _, err := Open(t.TempDir(), ""); err == nil {
			t.Fatal("expected an error for an empty namespace")
		}
	})
}

// Open must not create a session that does not exist: silently handing back an
// empty store would surface much later as a confusing auth failure.
func TestOpenDoesNotCreateMissingStore(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, "default"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "default")); !os.IsNotExist(err) {
		t.Errorf("Open created %s; it must never create a session file", filepath.Join(dir, "default"))
	}
}

// A second opener must be told the store is locked rather than hanging.
func TestOpenReportsLockContention(t *testing.T) {
	dir := t.TempDir()
	newFixture(t, dir, "default", nil)

	first, err := Open(dir, "default")
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	_, err = Open(dir, "default")
	if err == nil {
		t.Fatal("expected the second Open to fail while the first holds the lock")
	}
	if !strings.Contains(err.Error(), "locked by another process") {
		t.Errorf("error should name lock contention, got: %v", err)
	}
}
