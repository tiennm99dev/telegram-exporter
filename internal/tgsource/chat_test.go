package tgsource

import (
	"strings"
	"testing"
)

// Every form the shell pipeline accepted must still be accepted, and the one
// form it refused must still be refused. An operator's existing command lines
// are the compatibility surface here.
func TestNormalizeChat(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"numeric mtproto id", "1234567890", "1234567890"},
		{"username with at", "@mychannel", "mychannel"},
		{"bare username", "mychannel", "mychannel"},
		{"public t.me link", "https://t.me/mychannel", "https://t.me/mychannel"},
		{"tg protocol link", "tg://resolve?domain=mychannel", "tg://resolve?domain=mychannel"},
		{"bot api id loses the -100 prefix", "-1001234567890", "1234567890"},
		{"surrounding whitespace is trimmed", "  mychannel\n", "mychannel"},
		{"private channel link without a message", "https://t.me/c/1234567890", "https://t.me/c/1234567890"},
		// Invite and preview links are chats; their second component is not a
		// bare message number and must not be read as one.
		{"invite link", "https://t.me/+AbCd_1234", "https://t.me/+AbCd_1234"},
		{"joinchat link", "https://t.me/joinchat/AbCd1234", "https://t.me/joinchat/AbCd1234"},
		{"preview link", "https://t.me/s/mychannel", "https://t.me/s/mychannel"},
		{"other telegram host, no message", "https://telegram.dog/mychannel", "https://telegram.dog/mychannel"},
		{"tg link without a post parameter", "tg://resolve?domain=mychannel", "tg://resolve?domain=mychannel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeChat(tt.in)
			if err != nil {
				t.Fatalf("NormalizeChat(%q) returned an error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeChat(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A message link names one message, not a chat. Guessing the chat from it would
// quietly export something the operator did not ask for, so it is refused.
func TestNormalizeChatRejectsMessageLinks(t *testing.T) {
	for _, in := range []string{
		"https://t.me/c/1234567890/4242",
		"t.me/c/1234567890/4242",
		"https://t.me/mychannel/4242",
		// gotd accepts all three Telegram hosts and its parser keeps only the
		// domain, silently dropping the message number — so missing one of these
		// would walk an entire chat when one message was asked for.
		"https://telegram.me/mychannel/4242",
		"https://telegram.dog/mychannel/4242",
		"https://T.ME/mychannel/4242",
		// tg:// names the message in a query parameter, not the path.
		"tg://privatepost?channel=1234567890&post=4242",
		"tg://resolve?domain=mychannel&post=4242",
		"tg://openmessage?user_id=1&message_id=4242",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := NormalizeChat(in)
			if err == nil {
				t.Fatalf("NormalizeChat(%q) succeeded; a message link is not a chat", in)
			}
			if !strings.Contains(err.Error(), "message link") {
				t.Errorf("error should say it is a message link, got: %v", err)
			}
		})
	}
}

func TestNormalizeChatRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := NormalizeChat(in); err == nil {
			t.Errorf("NormalizeChat(%q) succeeded, want an error", in)
		}
	}
}

// -100 is stripped only when it prefixes a Bot API id, never from an ordinary
// number that happens to start with those digits.
func TestNormalizeChatOnlyStripsRealBotAPIPrefix(t *testing.T) {
	tests := map[string]string{
		"-1001234567890": "1234567890",     // Bot API id
		"1001234567890":  "1001234567890",  // no leading '-', not a Bot API id
		"-100":           "-100",           // prefix with no id after it
		"-2001234567890": "-2001234567890", // different prefix
	}
	for in, want := range tests {
		got, err := NormalizeChat(in)
		if err != nil {
			t.Fatalf("NormalizeChat(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeChat(%q) = %q, want %q", in, got, want)
		}
	}
}
