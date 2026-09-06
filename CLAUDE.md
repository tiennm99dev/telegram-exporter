# Project instructions

## No real data in the repository

Everything committed here — code, tests, comments, docs, sample output — uses
abstract example values. Never a real chat name, chat id, message id, filename,
account handle, or archive statistic, including ones taken from a session
transcript or a live run.

This repository archives a private Telegram chat, so real values name a real
account and its contents. Once committed they persist in history even after the
file is edited, and scrubbing them means rewriting history and force-pushing.

Use placeholders instead:

| Kind | Use |
|---|---|
| chat username | `mychannel`, `@mychannel` |
| chat / dialog id | `1234567890` (Bot API form `-1001234567890`) |
| message id | small round numbers, e.g. `4242` |
| Telegram file id | `1000000000000000001` and up |
| remote | `myremote:archive`, or a named backend when the backend matters |
| filenames | descriptive fakes; keep the property under test (unicode, emoji, length) |
| counts and sizes | round numbers that are obviously illustrative |

Keep the property a fixture exists to test. A test for multi-byte names still
needs a multi-byte name — make it an invented one, not a real file's.

Real values from a live run belong in the terminal, and in `plans/`, which is
git-ignored.
