package tgsource

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/gotd/td/telegram/peers"

	"github.com/iyear/tdl/core/util/tutil"
)

// botAPIID matches a Bot API chat id: the same channel as an MTProto id, but
// with a -100 prefix that MTProto itself does not use.
var botAPIID = regexp.MustCompile(`^-100(\d+)$`)

// telegramHosts are the hosts Telegram deep links use. gotd accepts all three
// (telegram/deeplink/deeplink.go hasTelegramPrefix), so checking only t.me would
// let a telegram.me or telegram.dog message link through — and gotd's parser
// keeps just the domain and silently drops the message number, which would walk
// an entire chat when the operator asked for one message.
var telegramHosts = []string{"t.me/", "telegram.me/", "telegram.dog/"}

// isMessageLink reports whether s points at a single message rather than a chat.
//
// This is parsed rather than pattern-matched because the two HTTPS shapes overlap
// in a way a regex gets wrong: a private link is t.me/c/<id>/<msg> and a public
// one is t.me/<name>/<msg>, so "t.me/c/1234567890" — a perfectly good private
// *channel* link — looks exactly like a public message link with the username
// "c". The distinction is whether a trailing numeric component follows the chat,
// and where that component sits depends on the "c" marker.
func isMessageLink(s string) bool {
	lower := strings.ToLower(s)

	// tg:// links name the message in a query parameter rather than the path.
	if strings.HasPrefix(lower, "tg://") {
		for _, key := range []string{"post=", "message_id="} {
			if strings.Contains(lower, "?"+key) || strings.Contains(lower, "&"+key) {
				return true
			}
		}
		return false
	}

	rest, ok := afterHost(lower)
	if !ok {
		return false
	}
	rest, _, _ = strings.Cut(rest, "?")
	rest, _, _ = strings.Cut(rest, "#")

	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) >= 1 && parts[0] == "c" {
		// c/<id> is the channel; c/<id>/<msg> is one message in it.
		return len(parts) >= 3 && isDigits(parts[2])
	}
	// t.me/joinchat/<hash> and t.me/s/<name> are chats, not messages, and their
	// second component is not a bare number — except for a hypothetical all-digit
	// invite hash, which is not worth mis-parsing every real link to guard.
	if len(parts) >= 1 && (parts[0] == "joinchat" || parts[0] == "s") {
		return false
	}
	return len(parts) >= 2 && isDigits(parts[1])
}

// afterHost returns the path following a Telegram host, if s names one.
func afterHost(lower string) (string, bool) {
	for _, host := range telegramHosts {
		if i := strings.Index(lower, host); i >= 0 {
			return lower[i+len(host):], true
		}
	}
	return "", false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// NormalizeChat converts a chat argument into the form the resolver expects.
//
// The accepted forms are the ones the shell pipeline accepted, because they are
// what an operator already has to hand: a numeric MTProto id as printed by
// `tdl chat ls`, a username with or without '@', or a t.me/tg:// link. Two need
// help. A Bot API id carries a -100 prefix that MTProto does not use, and a
// message link is not a chat — silently treating one as a chat would export the
// wrong thing, so it is refused with an explanation rather than guessed at.
func NormalizeChat(chat string) (string, error) {
	chat = strings.TrimSpace(chat)
	if chat == "" {
		return "", fmt.Errorf("a chat is required")
	}

	if isMessageLink(chat) {
		return "", fmt.Errorf("%q is a message link, not a chat — "+
			"pass the chat's username or id instead", chat)
	}

	if m := botAPIID.FindStringSubmatch(chat); m != nil {
		return m[1], nil
	}

	// The resolver takes a bare username; '@' is how humans write it.
	return strings.TrimPrefix(chat, "@"), nil
}

// ResolveChat turns a chat argument into a peer.
//
// Numeric arguments are looked up as channel, then user, then chat ids;
// everything else goes through the resolver, which handles usernames and
// t.me/tg:// links. That ordering is tdl's (core/util/tutil.GetInputPeer), kept
// so an id that works in `tdl` works here.
func ResolveChat(ctx context.Context, manager *peers.Manager, chat string) (peers.Peer, error) {
	normalized, err := NormalizeChat(chat)
	if err != nil {
		return nil, err
	}

	peer, err := tutil.GetInputPeer(ctx, manager, normalized)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve chat %q: %w", chat, err)
	}
	return peer, nil
}
