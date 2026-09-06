package tgsource

import (
	"context"
	"fmt"
	"iter"

	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
)

// Item is one downloadable media message.
//
// Name is filled here, at the single point where the message is seen, and is the
// same string used to check the remote and to write the file. See the naming
// package for why that matters. Media carries the location, size and DC that the
// downloader needs, so nothing has to be looked up a second time.
type Item struct {
	DialogID  int64
	MessageID int
	Name      string
	Media     *tmedia.Media
}

// Size reports the media size in bytes.
func (i Item) Size() int64 { return i.Media.Size }

// Walk yields every media message in a chat, newest first.
//
// Order is Telegram's: GetHistory pages backwards from the most recent message.
// The shell pipeline fetched oldest-first, so an interrupted run leaves a
// different subset archived than the old one would have.
//
// A sequence rather than a callback because the downloader consumes a pull
// iterator (Next/Value/Err), and range-over-func converts either way for free:
// callers that want the callback shape just range over it, while iter.Pull2
// gives the pull shape without anyone owning a goroutine. Messages are streamed,
// never collected — an 18k-message chat is tens of thousands of descriptors and
// the caller decides what to keep.
//
// Text-only and service messages carry no file and are skipped, the same rule
// the export JSON encoded as an empty "file" field. On error the sequence yields
// a zero Item with that error and stops; cancelling ctx stops it too, so an
// interrupted run does not keep paging.
func Walk(ctx context.Context, api *tg.Client, peer peers.Peer) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) {
		dialogID := peer.ID()

		it := query.NewQuery(api).Messages().GetHistory(peer.InputPeer()).BatchSize(100).Iter()
		for it.Next(ctx) {
			msg, ok := it.Value().Msg.(*tg.Message)
			if !ok {
				continue // service messages have no media
			}

			media, ok := tmedia.GetMedia(msg)
			if !ok {
				continue // text-only, or a media kind tmedia cannot download
			}

			if !yield(Item{
				DialogID:  dialogID,
				MessageID: msg.ID,
				Name:      naming.For(dialogID, msg.ID, media),
				Media:     media,
			}, nil) {
				return
			}
		}

		if err := it.Err(); err != nil {
			yield(Item{}, fmt.Errorf("walk chat history: %w", err))
		}

		// There is no cross-check that the walk saw the whole history, and the
		// obvious one does not work. gotd's iterator ends with a nil error if a
		// fetch yields an empty buffer (messages/iter.go:97,103,155-160), so a
		// truncated walk is indistinguishable from a complete one — but
		// Iterator.Total is the server's history count, which includes the
		// deleted slots that Next skips at iter.go:159-162. Comparing the two
		// reports a short read on any chat that has ever had a message deleted,
		// which is nearly all of them. A real check would need a count of
		// non-empty messages, which the API does not offer.
	}
}

// Manager builds a peers manager over the session's peer cache, so resolving the
// same chat twice does not cost a second round trip.
func Manager(api *tg.Client, storage peers.Storage) *peers.Manager {
	return peers.Options{Storage: storage}.Build(api)
}
