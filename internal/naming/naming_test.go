package naming

import (
	"testing"

	"github.com/iyear/tdl/core/tmedia"
)

// The format is a compatibility contract, not a style choice: ~15k files are
// already stored under it. Changing it makes every one of them look absent and
// re-downloads the entire archive.
func TestForMatchesStoredLayout(t *testing.T) {
	tests := []struct {
		name     string
		dialogID int64
		msgID    int
		file     string
		want     string
	}{
		{
			name:     "document with a filename attribute",
			dialogID: 1234567890,
			msgID:    14726,
			file:     "298.mp4",
			want:     "1234567890_14726_298.mp4",
		},
		{
			// The message that started this rewrite. Its name carries a doubled
			// '!' which tdl's default template collapsed to one, via filenamify,
			// while the export JSON kept both — the two derivations that never
			// agreed. This package keeps the name as Telegram reports it, so the
			// doubled '!' must survive.
			name:     "punctuation is preserved verbatim",
			dialogID: 1234567890,
			msgID:    4242,
			file:     "Pipe her!! And by her, we mean pipeperr! 1080p.mp4",
			want:     "1234567890_4242_Pipe her!! And by her, we mean pipeperr! 1080p.mp4",
		},
		{
			name:     "photo gets tmedia's generated name",
			dialogID: 1234567890,
			msgID:    42,
			file:     "5901234567890123456.jpg",
			want:     "1234567890_42_5901234567890123456.jpg",
		},
		{
			name:     "spaces and separators inside the filename are untouched",
			dialogID: 1234567890,
			msgID:    7,
			file:     "a_b c-d.e.mp4",
			want:     "1234567890_7_a_b c-d.e.mp4",
		},
		{
			name:     "non-ascii is untouched",
			dialogID: 1234567890,
			msgID:    8,
			file:     "ünïcödé näme 🍓.mp4",
			want:     "1234567890_8_ünïcödé näme 🍓.mp4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := For(tt.dialogID, tt.msgID, &tmedia.Media{Name: tt.file})
			if got != tt.want {
				t.Errorf("For() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Guards the reason the field layout is what it is. If this ever starts failing,
// tdl changed its default template and the compatibility note on For is stale.
func TestForKeepsCharactersTdlWouldRewrite(t *testing.T) {
	// filenamify (tdl's default template applies it) collapses runs of '!' and
	// replaces reserved characters. None of that may happen here.
	for _, file := range []string{
		"double!!bang.mp4",
		"a?b:c.mp4",
		".leading-dot.mp4",
		"trailing!.mp4",
	} {
		got := For(1, 2, &tmedia.Media{Name: file})
		want := "1_2_" + file
		if got != want {
			t.Errorf("For(%q) = %q, want %q — a sanitiser crept in", file, got, want)
		}
	}
}
