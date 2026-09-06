package naming

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// maxNameBytes is NAME_MAX on Linux: the longest single path component ext4 and
// friends accept. It is a byte count, not a rune count.
const maxNameBytes = 255

// Safe reports whether a stored name can be joined onto a directory path.
//
// Names are kept exactly as Telegram reports them, and the filename part comes
// from DocumentAttributeFilename — an unconstrained UTF-8 string chosen by
// whoever uploaded the file. So a name may contain '/' or be "..", and
// filepath.Join would happily resolve either outside the staging directory. tdl
// never had to think about this because its default template runs the name
// through filenamify, which rewrites separators and leading dots away; storing
// names verbatim moves that responsibility here.
//
// The rule is deliberately strict rather than corrective: a name that is not a
// single path element is rejected, not rewritten. Rewriting is what produced two
// disagreeing derivations in the first place, and a rejected file is a visible
// problem where a silently renamed one is not.
func Safe(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("empty filename")
	case len(name) > maxNameBytes:
		// The one rejection that is about a limit rather than an escape, and the
		// one that matters most: os.Create returns ENAMETOOLONG past this, so an
		// over-long name would download-fail forever while verify kept reporting
		// it absent — a loop that never terminates. tdl could not hit this
		// because filenamify truncates to 100 runes; storing names verbatim
		// removes that cap, so the limit has to be checked instead.
		return fmt.Errorf("filename is %d bytes, over the %d-byte limit: %q",
			len(name), maxNameBytes, name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("filename contains a NUL byte: %q", name)
	case name == "." || name == "..":
		return fmt.Errorf("filename is a directory reference: %q", name)
	case filepath.IsAbs(name):
		return fmt.Errorf("filename is an absolute path: %q", name)
	case name != filepath.Base(name):
		// Catches embedded separators, trailing slashes, and any ".." segment,
		// since Base of all of those differs from the original.
		return fmt.Errorf("filename is not a single path element: %q", name)
	}
	return nil
}

// SplitStored recovers the message id from a stored name, reporting false when
// the name does not have the expected shape or belongs to another dialog.
//
// Diagnostics only — telling "absent" apart from "present under a different
// name" when reporting on a remote. It must never decide that the wanted file is
// present: a file whose id matches but whose name does not is a different file.
// remote.Index keeps its id-keyed map unexported and its only id-to-name route
// excludes the wanted name, so an id cannot yield "the file you asked for is
// here" — though a caller that deliberately looks up a different name will of
// course get an answer about that name.
func SplitStored(dialogID int64, name string) (messageID int, ok bool) {
	prefix := strconv.FormatInt(dialogID, 10) + sep
	rest, found := strings.CutPrefix(name, prefix)
	if !found {
		return 0, false
	}
	idStr, _, found := strings.Cut(rest, sep)
	if !found {
		return 0, false
	}
	// Reject anything Atoi would accept but For would never emit: a sign, or
	// leading zeros. The id has to be the exact text For wrote.
	if idStr == "" || idStr[0] == '0' {
		return 0, false
	}
	for _, r := range idStr {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	// Only an overflow can fail here: the loop above rejected non-digits and a
	// leading zero, so anything that parses is already >= 1.
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, false
	}
	return id, true
}
