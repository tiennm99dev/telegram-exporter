package remote

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
)

// object is what the index remembers about one stored file.
//
// Path is kept alongside the basename because acting on an object — deleting a
// stale copy, say — needs the path rclone knows it by, while matching needs the
// basename. Conflating the two makes a delete address the wrong file, or no file
// at all, whenever a remote has any directory structure.
type object struct {
	path string
	size int64
}

// Index is a snapshot of what a remote holds, keyed by filename.
//
// It answers one question — "is this exact name present, and how big is it?" —
// and it answers it from the whole name, never from a message id. That is the
// point: a file whose id matches but whose name does not is a different file,
// and treating it as present is precisely the bug this rewrite exists to remove.
//
// The id-keyed map below exists only to tell "absent" apart from "absent, but
// something else is stored under this message's id" when reporting. It is
// unexported, and the only exported route from an id to a name is
// StoredUnderOtherNames, which by construction excludes the wanted name — so no
// caller can get "the file you asked for is present" out of an id.
type Index struct {
	byName     map[string]object
	byID       map[int][]string
	collisions []string
}

// BuildIndex lists a remote once and indexes it.
//
// Object paths are reduced to their basename, so a remote written with a
// subdirectory layout matches the same way a flat one does — the shell verifier
// did this too, and existing archives rely on it.
//
// Listing runs with depth and filters neutralised. rclone's ListFn otherwise
// inherits whatever RCLONE_MAX_DEPTH or RCLONE_EXCLUDE happen to be set to, and
// a narrowed listing here does not fail — it silently reports archived files as
// absent and re-downloads every one of them. The transfer tunables in Init are
// deliberately env-overridable; this is not.
func BuildIndex(ctx context.Context, f fs.Fs, dialogID int64) (*Index, error) {
	ctx, ci := fs.AddConfig(ctx)
	ci.MaxDepth = -1

	unfiltered, err := filter.NewFilter(nil)
	if err != nil {
		return nil, fmt.Errorf("build an empty filter: %w", err)
	}
	ctx = filter.ReplaceConfig(ctx, unfiltered)

	idx := &Index{
		byName: make(map[string]object),
		byID:   make(map[int][]string),
	}

	// ListFn is documented not to call fn concurrently, so the maps need no lock.
	if err := operations.ListFn(ctx, f, func(o fs.Object) {
		name := path.Base(o.Remote())

		if _, seen := idx.byName[name]; seen {
			// Two objects in different directories sharing a basename. Listing
			// order is not guaranteed, so silently keeping one would make the
			// verdict flip between runs — a zero-byte copy and a complete one
			// would alternate. Keep the first and report the ambiguity instead.
			if !slices.Contains(idx.collisions, name) {
				idx.collisions = append(idx.collisions, name)
			}
			return
		}

		idx.byName[name] = object{path: o.Remote(), size: o.Size()}
		if id, ok := naming.SplitStored(dialogID, name); ok {
			idx.byID[id] = append(idx.byID[id], name)
		}
	}); err != nil {
		// A destination that does not exist yet holds nothing. That is an empty
		// index, not a failure — it is what a first run against a new path looks
		// like, and treating it as an error would make verify unusable there.
		if errors.Is(err, fs.ErrorDirNotFound) {
			return idx, nil
		}
		return nil, fmt.Errorf("list %s: %w", f.String(), err)
	}
	return idx, nil
}

// Lookup reports the size stored under an exact name.
func (i *Index) Lookup(name string) (size int64, ok bool) {
	o, ok := i.byName[name]
	return o.size, ok
}

// PathOf returns the remote path an indexed name was found at, which is what
// rclone needs to act on the object. It differs from the name whenever the
// remote has directory structure.
func (i *Index) PathOf(name string) (string, bool) {
	o, ok := i.byName[name]
	return o.path, ok
}

// Len reports how many distinct names the remote held when the snapshot was
// taken. Objects dropped as basename collisions are not counted.
func (i *Index) Len() int { return len(i.byName) }

// Collisions lists basenames that appeared at more than one path. A non-empty
// result means the snapshot is ambiguous and any verdict about those names is
// unreliable, so callers should surface it rather than ignore it.
func (i *Index) Collisions() []string { return i.collisions }

// StoredUnderOtherNames lists names present for a message id that are not the
// wanted name.
//
// Diagnostics only. A non-empty result never means the file is archived — it
// means a stale copy from an earlier naming scheme is sitting there and will
// still be sitting there after the re-download, which is why the report has to
// surface it rather than quietly counting it.
func (i *Index) StoredUnderOtherNames(messageID int, wanted string) []string {
	var others []string
	for _, n := range i.byID[messageID] {
		if n != wanted {
			others = append(others, n)
		}
	}
	return others
}
