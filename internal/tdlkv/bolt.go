// Package tdlkv opens the key-value store the tdl CLI writes, so this binary can
// reuse an existing `tdl login` session instead of introducing a second auth path.
//
// Layout, as implemented by tdl's bolt driver: the configured storage path is a
// directory, each namespace is a file inside it named after the namespace, and
// inside that file every value lives in a single bucket, also named after the
// namespace. So the default session is `<data-dir>/default`, bucket `default`.
// (The `data.kv` file some installs also carry belongs to tdl's older single-file
// driver and is not read here.)
//
// The store is opened read-write, not read-only, and that is not incidental:
// gotd rewrites the session blob whenever it establishes or re-establishes a
// connection, so an ordinary run does write here. Two consequences follow.
// First, bolt holds an exclusive file lock for as long as the store is open, so
// this binary and the `tdl` CLI cannot run against the same namespace at once —
// for a `doctor` that is a moment, for a long archive run it is the whole run.
// Second, the file is shared with a separately built binary: bbolt's on-disk
// format is stable across the versions involved (tdl pins v1.3.10, this module
// v1.5.0) and the default freelist type matches, so the sharing is safe, but a
// future bbolt major would need checking rather than assuming.
package tdlkv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"

	"github.com/iyear/tdl/core/storage"
)

// lockTimeout bounds how long we wait for bolt's exclusive file lock. tdl holds
// that lock for its whole run, so without a timeout a concurrent `tdl` process
// makes this binary hang with no explanation. Three seconds is long enough to
// ride out a lock being handed over and short enough to fail fast.
const lockTimeout = 3 * time.Second

// DefaultDir returns tdl's default storage directory.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tdl/data"
	}
	return filepath.Join(home, ".tdl", "data")
}

// Store is a storage.Storage backed by one namespace of tdl's bolt store.
type Store struct {
	db     *bbolt.DB
	bucket []byte
}

// Open opens the namespace `ns` under the bolt directory `dir`.
//
// The namespace file must already exist: this binary never creates a session,
// it only reads the one `tdl login` produced. Creating it here would silently
// hand back an empty store and surface later as a confusing auth failure.
func Open(dir, ns string) (*Store, error) {
	if ns == "" {
		return nil, fmt.Errorf("namespace is required")
	}

	path := filepath.Join(dir, ns)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no tdl session at %s — run `tdl login%s` first",
				path, nsFlag(ns))
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: lockTimeout})
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return nil, fmt.Errorf("tdl session %s is locked by another process "+
				"(a running `tdl` or `tgexport`); stop it and retry", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	s := &Store{db: db, bucket: []byte(ns)}

	// Fail here rather than on the first Get: a namespace file without its
	// bucket is a store tdl never finished writing.
	if err := db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(s.bucket) == nil {
			return fmt.Errorf("no bucket %q in %s — session looks incomplete, re-run `tdl login%s`",
				ns, path, nsFlag(ns))
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

func nsFlag(ns string) string {
	if ns == "default" {
		return ""
	}
	return " -n " + ns
}

func (s *Store) Get(_ context.Context, key string) ([]byte, error) {
	var val []byte
	if err := s.db.View(func(tx *bbolt.Tx) error {
		// bbolt only guarantees a value is valid for the life of its
		// transaction, so copy before returning it.
		if v := tx.Bucket(s.bucket).Get([]byte(key)); v != nil {
			val = append([]byte(nil), v...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if val == nil {
		return nil, storage.ErrNotFound
	}
	return val, nil
}

func (s *Store) Set(_ context.Context, key string, value []byte) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(s.bucket).Put([]byte(key), value)
	})
}

func (s *Store) Delete(_ context.Context, key string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(s.bucket).Delete([]byte(key))
	})
}

func (s *Store) Close() error { return s.db.Close() }
