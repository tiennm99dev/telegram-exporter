"""Per-file download decisions and the sink loop, against a tmpdir and a fake
fetcher. No network.

The cases here are the ones where "looks like success" and "is success" can
diverge: dedupe, partial files, the disk guard, and Invariant 2.
"""

from __future__ import annotations

import errno
import json
import os
import stat

import pytest
from telethon.errors import (
    AuthKeyUnregisteredError,
    ChannelPrivateError,
    FileReferenceExpiredError,
    MediaEmptyError,
    ServerError,
)
from telethon.errors.common import (
    AuthKeyNotFound,
    CdnFileTamperedError,
    InvalidBufferError,
)

from telegram_exporter import downloader, paths
from telegram_exporter.downloader import (
    DOWNLOADED,
    FAILED,
    SKIPPED,
    Config,
    download_one,
    run_download,
    sweep_part_files,
    write_title,
)
from telegram_exporter.session import Abort
from telegram_exporter.sidecar import Sidecar
from telegram_exporter.state import State
from telegram_exporter.traversal import Post, build_filters
from tests.support import FakeMsg, as_agen, run

PAYLOAD = b"x" * 1000
KIB = 1024


@pytest.fixture(autouse=True)
def no_real_backoff(monkeypatch):
    """The retry ladder is 5/10 s in production; tests assert the ladder is
    walked, not that it sleeps. Yields the real function for the one test that
    is about the ladder's shape."""
    real = downloader.backoff
    monkeypatch.setattr(downloader, "backoff", lambda attempt: 0)
    return real


class FakeFetcher:
    def __init__(self, payload: bytes = PAYLOAD, raises=None, refresh_to="same"):
        self.payload = payload
        self.raises = list(raises or [])      # exceptions to raise, in order
        self.refresh_to = refresh_to
        self.calls = 0
        self.refresh_calls = 0

    async def fetch(self, msg, fh) -> None:
        self.calls += 1
        if self.raises:
            raise self.raises.pop(0)
        fh.write(self.payload)

    async def refresh(self, message_id):
        self.refresh_calls += 1
        return FakeMsg(message_id, kind="photo") if self.refresh_to == "same" \
            else self.refresh_to


def cfg(tmp_path, *, min_free=0):
    return Config(min_free=min_free, disk_path=tmp_path)


def post_dir_for(tmp_path, post_id=1042):
    root = paths.export_root(tmp_path, -1001)
    target = paths.post_dir(root, post_id)
    target.mkdir(parents=True, exist_ok=True)
    return root, target


# --------------------------------------------------------------------------- #
# The plan's decision table
# --------------------------------------------------------------------------- #

def test_target_absent_downloads(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher()

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert result.path.read_bytes() == PAYLOAD
    assert fetcher.calls == 1
    assert list(target_dir.glob("*.part")) == []


def test_an_existing_non_empty_target_is_skipped_without_a_download(tmp_path):
    # Existence, not size equality. fsync before os.replace means a file at the
    # target path can only have arrived fully downloaded - and a size-equality
    # gate would delete every completed photo on resume, because photo .size
    # describes the variant Telethon picked.
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=999_999)
    (target_dir / "1042_photo.jpg").write_bytes(b"already here")
    fetcher = FakeFetcher()

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == SKIPPED
    assert fetcher.calls == 0


def test_a_zero_byte_target_is_unlinked_and_refetched(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    (target_dir / "1042_photo.jpg").touch()
    fetcher = FakeFetcher()

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert fetcher.calls == 1
    assert result.path.read_bytes() == PAYLOAD


def test_stray_part_files_are_swept(tmp_path):
    root, target_dir = post_dir_for(tmp_path)
    (target_dir / "1042_photo.jpg.part").write_bytes(b"half")
    (root / "9/9_video.mp4.part").parent.mkdir(parents=True)
    (root / "9/9_video.mp4.part").write_bytes(b"half")

    assert sweep_part_files(root) == 2
    assert list(root.rglob("*.part")) == []


def test_a_valid_target_wins_over_a_leftover_part_file(tmp_path):
    root, target_dir = post_dir_for(tmp_path)
    (target_dir / "1042_photo.jpg").write_bytes(PAYLOAD)
    (target_dir / "1042_photo.jpg.part").write_bytes(b"half")
    sweep_part_files(root)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))

    result = run(download_one(FakeFetcher(), target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == SKIPPED
    assert (target_dir / "1042_photo.jpg").read_bytes() == PAYLOAD
    assert list(target_dir.glob("*.part")) == []


def test_unknown_declared_size_is_accepted_by_the_validator(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="document", size=None)

    result = run(download_one(FakeFetcher(), target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert result.declared_size is None


def test_a_hostile_declared_filename_lands_inside_the_post_dir(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="document", name="../../../etc/passwd",
                  size=len(PAYLOAD))

    result = run(download_one(FakeFetcher(), target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert result.path.parent == target_dir.resolve()
    assert result.path.name == "1042_passwd.bin"


def test_the_disk_guard_aborts_before_writing_a_single_byte(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="video", size=10 * KIB)
    fetcher = FakeFetcher()

    with pytest.raises(Abort) as excinfo:
        run(download_one(fetcher, target_dir, msg,
                         cfg=cfg(tmp_path, min_free=1 << 62)))

    assert excinfo.value.code == 3
    assert fetcher.calls == 0
    assert list(target_dir.iterdir()) == []


# --------------------------------------------------------------------------- #
# Validator and error taxonomy
# --------------------------------------------------------------------------- #

def test_a_photo_size_mismatch_is_accepted_as_a_variant(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD) + 431)

    result = run(download_one(FakeFetcher(), target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert result.declared_size != result.size          # recorded, not hidden


def test_a_document_size_mismatch_retries_then_fails_without_raising(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="document", size=len(PAYLOAD) + 1)
    fetcher = FakeFetcher()

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == FAILED
    assert fetcher.calls == downloader.MAX_ATTEMPTS
    assert list(target_dir.glob("*.part")) == []


def test_an_expired_file_reference_is_refreshed_once_then_succeeds(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher(raises=[FileReferenceExpiredError(request=None)])

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert fetcher.refresh_calls == 1


def test_a_deleted_message_is_recorded_and_skipped(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher(raises=[FileReferenceExpiredError(request=None)],
                          refresh_to=None)

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == FAILED
    assert result.error == "message deleted"


def test_unavailable_media_is_recorded_and_skipped(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher(raises=[MediaEmptyError(request=None)])

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == FAILED
    assert "unavailable" in result.error


def test_a_transient_network_error_retries_and_then_succeeds(tmp_path):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    # ConnectionResetError with errno=None is the case that would fall through
    # the errno test into "unexpected OS error" if OSError were caught first.
    fetcher = FakeFetcher(raises=[ConnectionResetError()])

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert fetcher.calls == 2


def test_a_zero_byte_download_is_never_accepted(tmp_path):
    """download_media returns without writing when the media resolves to an empty
    variant. The photo excuse in validate() would otherwise rename a 0-byte file
    to the target, where the "zero bytes is junk" dedupe rule could never see it
    again - a completed export does not revisit existing targets."""
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher(payload=b"")

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == FAILED
    assert fetcher.calls == downloader.MAX_ATTEMPTS
    assert not (target_dir / "1042_photo.jpg").exists()
    assert list(target_dir.glob("*.part")) == []


def test_an_unsafe_filename_skips_one_file_instead_of_ending_the_run(tmp_path):
    # Defense in depth behind the extension sanitizer: one hostile message must
    # never be able to halt an export permanently, which is what happened while
    # the ValueError from safe_join escaped and the cursor could not advance.
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher()

    # Simulate a hole in the sanitizer rather than asserting one exists.
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(paths, "derive_filename", lambda m: "../escape.jpg")
        result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == FAILED
    assert "unsafe filename" in result.error
    assert fetcher.calls == 0
    assert not (target_dir.parent / "escape.jpg").exists()


@pytest.mark.parametrize("exc", [
    # A plain -500: RpcCallFailError is only one leaf of ServerError, so catching
    # the leaf alone let the parent escape and end the run.
    ServerError(request=None, message="INTERNAL_SERVER_ERROR"),
    InvalidBufferError(payload=b"\x00\x00\x00\x00"),  # corrupt packet on a flaky link
])
def test_transport_level_errors_retry_rather_than_killing_the_run(tmp_path, exc):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher(raises=[exc])

    result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == DOWNLOADED
    assert fetcher.calls == 2


def test_a_cdn_integrity_failure_is_named_rather_than_called_a_network_error(
        tmp_path, caplog):
    """CdnFileTamperedError subclasses SecurityError and is raised on the
    download path. Folding it into the network clause reported a trust-boundary
    event as three lines of "network error" and a bland FAILED."""
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))
    fetcher = FakeFetcher(raises=[CdnFileTamperedError()])

    with caplog.at_level("ERROR"):
        result = run(download_one(fetcher, target_dir, msg, cfg=cfg(tmp_path)))

    assert result.status == FAILED
    assert result.error == "cdn integrity check failed"
    assert fetcher.calls == 1                      # not retried as if it were noise
    assert "integrity check FAILED" in caplog.text


@pytest.mark.parametrize("exc,code", [
    (AuthKeyUnregisteredError(request=None), 4),
    # The one that actually arrives: MTProtoSender sets it on every in-flight
    # request when the connection drops, and it subclasses plain Exception - so
    # it used to escape every handler and end the run with a traceback.
    (AuthKeyNotFound(), 4),
    (ChannelPrivateError(request=None), 5),
    (OSError(errno.ENOSPC, "No space left on device"), 3),
])
def test_terminal_errors_abort_with_their_own_exit_code(tmp_path, exc, code):
    _, target_dir = post_dir_for(tmp_path)
    msg = FakeMsg(1042, kind="photo", size=len(PAYLOAD))

    with pytest.raises(Abort) as excinfo:
        run(download_one(FakeFetcher(raises=[exc]), target_dir, msg,
                         cfg=cfg(tmp_path)))

    assert excinfo.value.code == code
    assert list(target_dir.glob("*.part")) == []


# --------------------------------------------------------------------------- #
# The sink loop - Invariant 2
# --------------------------------------------------------------------------- #

def drive(tmp_path, posts, *, limit=None, fetcher=None, filters=None):
    root = paths.export_root(tmp_path, -1001)
    root.mkdir(parents=True, exist_ok=True)
    state = State.open(root, chat_id=-1001, chat_title="G",
                       filters=filters or build_filters())
    with Sidecar(root) as sidecar:
        totals = run(run_download(as_agen(posts), fetcher=fetcher or FakeFetcher(),
                                  state=state, sidecar=sidecar, root=root,
                                  cfg=cfg(tmp_path), limit=limit))
    records = [json.loads(line)
               for line in (root / "messages.jsonl").read_text().splitlines()]
    return root, state, totals, records


def album(post_id, ids, *, kind="photo"):
    messages = [FakeMsg(i, kind=kind, grouped_id=7, size=len(PAYLOAD)) for i in ids]
    return Post(post_id=post_id, messages=messages, max_message_id=max(ids))


def test_a_completed_post_commits_the_cursor_and_writes_the_sidecar(tmp_path):
    root, state, totals, records = drive(tmp_path, [album(1042, [1042, 1043])])

    assert state.cursor_id == 1043
    assert totals.downloaded == 2
    assert totals.posts == 1
    assert [r["message_id"] for r in records] == [1042, 1043]
    assert records[0]["files"][0]["path"] == "1042/1042_photo.jpg"
    assert (root / "1042" / "1042_photo.jpg").exists()


def test_the_cursor_is_never_committed_from_an_abort_path(tmp_path):
    # Invariant 2: state.commit is unreachable from every error exit, so the
    # cursor can never point past an in-flight post. This is the failure that
    # would look exactly like success.
    posts = [album(1042, [1042]), album(2000, [2000])]
    root = paths.export_root(tmp_path, -1001)
    root.mkdir(parents=True, exist_ok=True)
    state = State.open(root, chat_id=-1001, chat_title="G", filters=build_filters())

    class FailSecond(FakeFetcher):
        async def fetch(self, msg, fh):
            if msg.id == 2000:
                raise ChannelPrivateError(request=None)
            await super().fetch(msg, fh)

    with Sidecar(root) as sidecar:
        with pytest.raises(Abort) as excinfo:
            run(run_download(as_agen(posts), fetcher=FailSecond(), state=state,
                             sidecar=sidecar, root=root, cfg=cfg(tmp_path)))

    assert excinfo.value.code == 5
    assert state.cursor_id == 1042            # the first post, not the second
    assert State.read(root)["completed_at"] is None


def test_a_second_run_re_downloads_nothing(tmp_path):
    posts = [album(1042, [1042, 1043])]
    drive(tmp_path, posts)
    fetcher = FakeFetcher()
    drive(tmp_path, posts, fetcher=fetcher)

    assert fetcher.calls == 0


def test_empty_and_text_only_posts_advance_the_cursor_without_counting(tmp_path):
    posts = [Post(post_id=10, messages=[], max_message_id=11),
             Post(post_id=12, messages=[FakeMsg(12, text="hi")], max_message_id=12)]
    root, state, totals, records = drive(
        tmp_path, posts, filters=build_filters(include_text=True))

    assert state.cursor_id == 12
    assert totals.posts == 0 and totals.downloaded == 0
    assert [r["message_id"] for r in records] == [12]
    assert records[0]["files"] == []           # context, not media
    assert not (root / "12").exists()          # no folder for a text message


def test_limit_counts_posts_with_files(tmp_path):
    posts = [Post(post_id=10, messages=[], max_message_id=10),
             album(11, [11]), album(12, [12]), album(13, [13])]
    _, state, totals, _ = drive(tmp_path, posts, limit=2)

    assert totals.posts == 2
    assert state.cursor_id == 12                # stopped after the second, not the third


def test_completed_at_is_set_when_the_sweep_is_exhausted(tmp_path):
    root, _, _, _ = drive(tmp_path, [album(1042, [1042])])
    assert State.read(root)["completed_at"] is not None


def test_a_run_where_everything_failed_is_never_marked_complete(tmp_path, caplog):
    """A dead session looks exactly like this from inside the loop: ConnectionError
    is retryable, so the sweep walks the whole history failing every file and
    returns normally. Setting completed_at there would answer "did my export
    finish?" with a confident yes over an empty tree."""
    class AlwaysFails(FakeFetcher):
        async def fetch(self, msg, fh):
            self.calls += 1
            raise ConnectionError("Cannot send requests while disconnected")

    with caplog.at_level("ERROR"):
        root, state, totals, _ = drive(
            tmp_path, [album(1, [1]), album(2, [2])], fetcher=AlwaysFails())

    assert totals.failed == 2 and totals.downloaded == 0
    assert State.read(root)["completed_at"] is None
    assert "not marking this export complete" in caplog.text
    # The cursor still stands: those posts were handled, and the failures are
    # recorded in the sidecar.
    assert state.cursor_id == 2


def test_a_partly_failed_run_is_still_complete(tmp_path):
    class FailsOne(FakeFetcher):
        async def fetch(self, msg, fh):
            self.calls += 1
            if msg.id == 2:
                raise MediaEmptyError(request=None)
            fh.write(self.payload)

    root, _, totals, _ = drive(tmp_path, [album(1, [1]), album(2, [2])],
                               fetcher=FailsOne())

    assert totals.downloaded == 1 and totals.failed == 1
    assert State.read(root)["completed_at"] is not None


def test_the_retry_ladder_does_not_sleep_after_the_final_attempt(no_real_backoff):
    # 20 s per exhausted file bought nothing; on a dead connection it was the
    # difference between hours and days.
    backoff = no_real_backoff
    assert backoff(1) == 5.0
    assert backoff(2) == 10.0
    assert backoff(downloader.MAX_ATTEMPTS) == 0.0


def test_the_whole_export_tree_is_private_under_the_process_umask(tmp_path):
    """plan.md's "export tree 0700, every file 0600" rested on umask alone, with
    nothing observing the result. The sidecar carries other people's message text
    and names, so this is the criterion worth asserting rather than assuming."""
    old = os.umask(0o077)
    try:
        root, _, _, _ = drive(tmp_path, [album(1042, [1042, 1043])])
        write_title(root, "My Group")          # cli writes this; drive() does not
    finally:
        os.umask(old)

    checked = [root, root / "1042", root / "1042" / "1042_photo.jpg",
               root / "messages.jsonl", root / ".export-state.json",
               root / "title.txt"]
    for path in checked:
        assert path.exists(), path
        assert stat.S_IMODE(path.stat().st_mode) & 0o077 == 0, \
            f"{path} is group/world accessible: {oct(path.stat().st_mode)}"


def test_limit_zero_connects_and_does_nothing(tmp_path):
    root, state, totals, _ = drive(tmp_path, [album(1042, [1042])], limit=0)
    assert totals.posts == 0
    assert state.cursor_id == 0
    assert not (root / "1042").exists()
