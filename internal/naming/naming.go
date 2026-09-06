// Package naming builds the filename a media message is stored under.
//
// This package exists to have exactly one answer to "what is this file called".
// The shell pipeline it replaces had two, and they disagreed. `tdl chat export`
// wrote the raw Telegram filename into a JSON, while `tdl dl` ran that same name
// through its download template — whose default is
//
//	{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}
//
// (tdl@v0.20.4/cmd/dl.go:50). `filenamify` rewrites characters a filesystem
// rejects and, incidentally, collapses any run of two or more '!' into one. So a
// message whose filename contained "!!" was checked for under one name and
// stored under another; the verifier never found it and re-fetched it on every
// pass, forever. That is not a hypothetical — it cost 966 MB per pass on one
// message in this repo's own archive.
//
// The rule here is therefore not "be careful to keep the two in sync". There is
// one function, called once per message, and the string it returns is used both
// to ask whether the file is already archived and to write it. A divergence
// between those two questions is not made unlikely; it is made unrepresentable.
package naming

import (
	"strconv"
	"strings"

	"github.com/iyear/tdl/core/tmedia"
)

// Separator between the three fields of a stored filename.
const sep = "_"

// For returns the archive filename for one media message:
//
//	{DialogID}_{MessageID}_{FileName}
//
// The field layout matches tdl's default template, but FileName does not: tdl
// passes it through `filenamify` and this does not. That is a deliberate,
// recorded choice (see the plan's Phase 2 notes) — names stay as Telegram
// reports them rather than being rewritten — and it means a file the old shell
// pipeline stored under a filenamify-altered name will not be recognised here
// and will be fetched again. For this repo's archive that affects exactly one
// message out of 12,000, and it was already removed.
//
// FileName comes from tmedia, the same extractor tdl uses: a document's
// DocumentAttributeFilename, or a generated stable name when it has none
// (`<photoID>.jpg` for a photo, `<docID><ext>` for a document).
//
// The name is taken verbatim and is therefore NOT safe to join onto a path.
// DocumentAttributeFilename is set by whoever uploaded the file, so it can
// contain '/' or '..' and escape a staging directory. Callers that turn a name
// into a path must check containment themselves; doing it here would silently
// rewrite names and reintroduce exactly the two-derivations problem above.
func For(dialogID int64, messageID int, m *tmedia.Media) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(dialogID, 10))
	b.WriteString(sep)
	b.WriteString(strconv.Itoa(messageID))
	b.WriteString(sep)
	b.WriteString(m.Name)
	return b.String()
}
