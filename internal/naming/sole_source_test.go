package naming

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// buildsAName matches the ways a stored filename would plausibly be assembled by
// hand: the format verbs ("%d_%d_%s" and relatives) and string concatenation
// around a bare "_" separator.
//
// This is a lint for known shapes, not a proof. A determined reimplementation —
// a strings.Builder copy of For, say — still slips through. It catches the
// realistic accident, which is someone reaching for Sprintf in a new file.
var buildsAName = regexp.MustCompile(`%[ds]_%[ds]|_%[ds]_|\+ *"_" *\+`)

// The whole point of this package is that it is the only answer to "what is this
// file called". Nothing in the type system prevents a second place from
// formatting the same string, so the common ways of doing so are linted here.
//
// If this test fails, the fix is to call For (or MessageID) rather than to widen
// the pattern. The shell pipeline's bug was two independent name derivations that
// nothing forced to agree; a second one here would reintroduce it exactly.
func TestNamingIsTheSoleSourceOfFilenames(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS and anything vendored; only our own source counts.
			switch d.Name() {
			case ".git", "vendor", "plans", "staging":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// This package is the one place allowed to build the name.
		if filepath.Dir(path) == filepath.Join(root, "internal", "naming") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if buildsAName.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("filenames must only be built by naming.For; found %d other place(s):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
