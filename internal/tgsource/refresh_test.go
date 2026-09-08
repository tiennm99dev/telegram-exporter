package tgsource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
)

// These drive refresher through its fetch seam rather than through a Telegram
// client, because what needs testing is not the round trip — it is the judgement
// afterwards. A re-read that describes a different file must be refused, and a
// re-read that failed because the message is gone must be distinguishable from
// one that failed because the connection did: the download half retries the
// second and skips the first.

const testDialogID = 1234567890

// docMessage builds a message carrying one document, the shape tmedia extracts.
func docMessage(t *testing.T, id int, file string, size int64, ref string) *tg.Message {
	t.Helper()
	msg := &tg.Message{ID: id}
	msg.SetMedia(&tg.MessageMediaDocument{
		Document: &tg.Document{
			ID:            1000000000000000001,
			Size:          size,
			DCID:          2,
			MimeType:      "video/mp4",
			FileReference: []byte(ref),
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: file},
			},
		},
	})
	return msg
}

// walked is the item as the walk first saw it, carrying a reference that has
// since expired.
func walked(t *testing.T, id int, file string, size int64) Item {
	t.Helper()
	media, ok := tmedia.GetMedia(docMessage(t, id, file, size, "stale"))
	if !ok {
		t.Fatal("fixture message carries no media")
	}
	return Item{
		DialogID:  testDialogID,
		MessageID: id,
		Name:      naming.For(testDialogID, id, media),
		Media:     media,
	}
}

func TestRefresherSubstitutesALiveReference(t *testing.T) {
	it := walked(t, 4242, "clip.mp4", 5000)
	name := it.Name

	got, err := refresher(func(_ context.Context, id int) (*tg.Message, error) {
		if id != 4242 {
			t.Errorf("fetched message %d, want 4242", id)
		}
		return docMessage(t, 4242, "clip.mp4", 5000, "fresh"), nil
	})(t.Context(), it)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	loc, ok := got.Media.InputFileLoc.(*tg.InputDocumentFileLocation)
	if !ok {
		t.Fatalf("location is %T, want *tg.InputDocumentFileLocation", got.Media.InputFileLoc)
	}
	if string(loc.FileReference) != "fresh" {
		t.Errorf("file reference = %q, want the re-read one", loc.FileReference)
	}
	// The name is the remote's key: it is what the run diffed against the index
	// and what the file will be written as, so a refresh must never move it.
	if got.Name != name {
		t.Errorf("name = %q, want it unchanged at %q", got.Name, name)
	}
	if got.Media.Size != 5000 {
		t.Errorf("size = %d, want 5000", got.Media.Size)
	}
}

// Every permanent reason a re-read cannot be used has to arrive as ErrGone,
// because that is the only signal that says "skip this one and keep going"
// rather than "the source is failing".
func TestRefresherReportsGoneForEveryPermanentCase(t *testing.T) {
	it := walked(t, 4242, "clip.mp4", 5000)

	for _, tc := range []struct {
		name string
		get  fetchMessage
		want string
	}{
		{
			name: "message no longer in the history",
			get: func(context.Context, int) (*tg.Message, error) {
				return nil, fmt.Errorf("%w: no message at or before 4242", ErrGone)
			},
			want: "4242",
		},
		{
			name: "message carries no media any more",
			get: func(context.Context, int) (*tg.Message, error) {
				return &tg.Message{ID: 4242, Message: "edited into text"}, nil
			},
			want: "no longer carries a file",
		},
		{
			name: "a different file under the same message",
			get: func(_ context.Context, _ int) (*tg.Message, error) {
				return docMessage(t, 4242, "other.mp4", 5000, "fresh"), nil
			},
			want: "different file",
		},
		{
			// Size is what reserved the staging budget, what the finished
			// download is measured against and what the closing verify
			// compares, so a same-name replacement of a different length is a
			// different file too.
			name: "same name, different length",
			get: func(_ context.Context, _ int) (*tg.Message, error) {
				return docMessage(t, 4242, "clip.mp4", 6000, "fresh"), nil
			},
			want: "different file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := refresher(tc.get)(t.Context(), it)
			if !errors.Is(err, ErrGone) {
				t.Fatalf("err = %v, want ErrGone", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A re-read that failed for any other reason is not permanent and must not be
// dressed up as one: the caller counts these as download failures, retries them,
// and gives up on the source once enough of them land in a row.
func TestRefresherKeepsATransientFailureRetryable(t *testing.T) {
	it := walked(t, 4242, "clip.mp4", 5000)

	_, err := refresher(func(context.Context, int) (*tg.Message, error) {
		return nil, errors.New("dial telegram: connection refused")
	})(t.Context(), it)

	if err == nil {
		t.Fatal("a failed re-read reported success")
	}
	if errors.Is(err, ErrGone) {
		t.Errorf("a connection failure was classified as ErrGone: %v", err)
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("err = %v, want it to name the message", err)
	}
}
