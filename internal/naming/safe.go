package naming

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rclone/rclone/lib/encoder"
)

// maxNameBytes is the longest stored name that can actually be written.
//
// NAME_MAX on Linux is 255 bytes for a single path component, but the name is
// not what lands on disk first: a download is written to name+".part" and
// renamed afterwards, so the suffix has to fit inside the limit too. Checking
// the bare 255 would pass a name whose part file then fails with ENAMETOOLONG —
// after the item had already been queued, which stalls the walk on the same
// message on every pass.
const maxNameBytes = 255 - len(PartSuffix)

// PartSuffix marks a download still in flight. It lives here because Safe's
// length limit has to account for it.
const PartSuffix = ".part"

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
	case encoder.OS.FromStandardName(name) != name:
		// rclone does not address files by the bytes on disk. Every name given
		// to an Fs is run through the backend's encoder, and every name listed
		// back is re-encoded to the standard set — neither of which os.OpenFile
		// performs. So a name containing one of the characters those encoders
		// rewrite is written verbatim, then looked up under a different string:
		// the upload fails with "object not found" forever, or it succeeds and
		// the index records a name the presence check will never match.
		//
		// That is the original bug exactly — one name derived two ways — with
		// rclone's encoder in the place filenamify used to occupy. Rejecting
		// rather than encoding is the same choice made everywhere else here:
		// encoding would give the two derivations a chance to disagree again.
		return fmt.Errorf("filename is rewritten by rclone's path encoder: %q", name)
	case encoder.Standard.Encode(encoder.Standard.Decode(name)) != name:
		// The listing side of the same problem, and it is not backend-specific:
		// the re-encode to the standard set happens above the backend encoder,
		// so it applies to every remote. Control characters and DEL are the
		// common case, and both are trivially settable in a Telegram filename.
		return fmt.Errorf("filename is rewritten when rclone lists it back: %q", name)
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
