package tgsource

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
)

// ErrGone marks a message this run can no longer fetch: it was deleted, it no
// longer carries media, or the file behind it has been replaced by a different
// one.
//
// The distinction from an ordinary re-read failure is what the caller acts on.
// There is nothing here to retry and nothing a later pass would do differently,
// so one such message is left unarchived and the run carries on — whereas a
// re-read that failed because the connection did must count as a failure like
// any other, or a dead source stops looking like one.
var ErrGone = errors.New("message is gone")

// Refresh re-reads one message and returns the item with a live file reference.
//
// A function type rather than an interface because there is one implementation
// and the download half only ever needs to call it; a func also lets a test
// supply a canned reference without a Telegram connection.
type Refresh func(context.Context, Item) (Item, error)

// fetchMessage reads one message of a chat by id, reporting ErrGone if it is not
// there any more. It is the seam Refresher is built on, so the checks that
// decide whether a re-read describes the same file can be tested without a
// Telegram client.
type fetchMessage func(ctx context.Context, messageID int) (*tg.Message, error)

// Refresher re-mints the file reference for an item from the chat it came from.
//
// The reference in Media.InputFileLoc is a short-lived token, and Telegram
// deliberately does not document how long it lives — the documented contract is
// the other way round: a client caches the reference *together with the source
// it came from*, and re-reads that source when the reference expires
// (core.telegram.org/api/file-references). This is that re-read. The source is
// the message, so reading the message again yields a document carrying a new
// reference.
func Refresher(api *tg.Client, peer peers.Peer) Refresh {
	return refresher(historyFetch(api, peer.InputPeer()))
}

func refresher(get fetchMessage) Refresh {
	return func(ctx context.Context, it Item) (Item, error) {
		msg, err := get(ctx, it.MessageID)
		if err != nil {
			if errors.Is(err, ErrGone) {
				return Item{}, err
			}
			return Item{}, fmt.Errorf("re-read message %d: %w", it.MessageID, err)
		}

		media, ok := tmedia.GetMedia(msg)
		if !ok {
			// The walk only yields messages that had media, so this means the
			// message was edited into something else.
			return Item{}, fmt.Errorf("%w: message %d no longer carries a file",
				ErrGone, it.MessageID)
		}

		// The re-read has to describe the same file, and both halves of that are
		// checked because both are load-bearing. Name is what the remote index
		// was diffed against and what the file will be written as, so a run that
		// silently accepted a new one would archive different bytes under the
		// old message's name. Size is what reserved the staging budget, what the
		// finished download is measured against, and what the closing verify
		// compares — a changed size would either fail that check every pass or
		// be re-fetched forever as a size mismatch.
		//
		// A mismatch is therefore not a refresh failure but a different file,
		// which this run has no name for and must leave alone.
		if name := naming.For(it.DialogID, it.MessageID, media); name != it.Name || media.Size != it.Media.Size {
			return Item{}, fmt.Errorf("%w: message %d now holds a different file (%q, %d bytes; was %q, %d bytes)",
				ErrGone, it.MessageID, name, media.Size, it.Name, it.Media.Size)
		}

		it.Media = media
		return it, nil
	}
}

// historyFetch reads a single message out of a chat's history.
//
// This is a one-message GetHistory rather than channels.getMessages so that
// channels, groups and users need no branching, and it is written here rather
// than reusing tutil.GetSingleMessage because the classification is the point.
// tutil reports a deleted message three different ways — its own
// ErrMessageDeleted when an older message sits in the slot, "invalid message"
// when that message is a service one, and a wrapped nil when the page comes back
// empty — and only the first is distinguishable. This needs every one of them to
// be exactly ErrGone, because the caller retries anything else.
//
// OffsetID is exclusive, so id+1 asks for the message itself first. Telegram
// answers with the newest message at or below that offset, and gotd drops empty
// slots while paging (query/messages/iter.go), so a returned id lower than the
// one asked for means the message is not in the history any more.
func historyFetch(api *tg.Client, peer tg.InputPeerClass) fetchMessage {
	return func(ctx context.Context, messageID int) (*tg.Message, error) {
		it := query.NewQuery(api).Messages().GetHistory(peer).
			OffsetID(messageID + 1).BatchSize(1).Iter()

		if !it.Next(ctx) {
			// gotd ends the iterator with a nil error on an empty page, so a
			// nil here is not success: it is "nothing at or below this id".
			if err := it.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: no message at or before %d", ErrGone, messageID)
		}

		msg, ok := it.Value().Msg.(*tg.Message)
		if !ok || msg.ID != messageID {
			return nil, fmt.Errorf("%w: message %d is no longer in the chat's history",
				ErrGone, messageID)
		}
		return msg, nil
	}
}
