"""The dry-run estimator.

The property under test is anti-drift: the estimate has to describe exactly what
a real run from the same start would fetch, because that number is the one users
make disk decisions on.
"""

from __future__ import annotations

import pytest

from telegram_exporter import paths
from telegram_exporter.downloader import run_download
from telegram_exporter.estimate import EstimateSink, describe_filters, resume_from, run_estimate
from telegram_exporter.sidecar import Sidecar
from telegram_exporter.state import STATE_NAME, State
from telegram_exporter.traversal import Post, build_filters
from tests.support import FakeMsg, as_agen, run
from tests.test_download_decisions import PAYLOAD, FakeFetcher, cfg

GIB = 1 << 30


def album(post_id, ids, *, kind="photo", size=len(PAYLOAD)):
    return Post(post_id=post_id,
                messages=[FakeMsg(i, kind=kind, grouped_id=7, size=size)
                          for i in ids],
                max_message_id=max(ids))


def estimate(tmp_path, posts, *, min_free=0, limit=None, filters=None):
    root = paths.export_root(tmp_path, -1001)
    sink = EstimateSink()
    filters = filters or build_filters()
    code = run(run_estimate(as_agen(posts), sink=sink, root=root, title="My Group",
                            peer_id=-1001, filters=filters,
                            start_id=resume_from(root, filters),
                            min_free=min_free, limit=limit))
    return sink, code, root


# --------------------------------------------------------------------------- #
# Counting
# --------------------------------------------------------------------------- #

def test_counts_are_broken_down_by_kind(tmp_path):
    posts = [album(1, [1, 2], kind="photo", size=100),
             album(3, [3], kind="video", size=5000)]
    sink, code, _ = estimate(tmp_path, posts)

    assert sink.posts == 2 and sink.files == 3
    assert sink.by_kind == {"photo": [2, 200], "video": [1, 5000]}
    assert sink.total_bytes == 5200
    assert code == 0


def test_unknown_size_files_get_their_own_tally_never_a_zero(tmp_path):
    # An unbounded estimate error must be visible, so these are counted on their
    # own line rather than coerced into the byte total.
    posts = [album(1, [1], kind="document", size=None)]
    sink, _, _ = estimate(tmp_path, posts)

    assert sink.unknown_size_files == 1
    assert sink.files == 1
    assert sink.total_bytes == 0


def test_empty_and_text_only_posts_are_not_counted(tmp_path):
    posts = [Post(post_id=1, messages=[], max_message_id=2),
             Post(post_id=3, messages=[FakeMsg(3, text="hi")], max_message_id=3),
             album(4, [4])]
    sink, _, _ = estimate(tmp_path, posts)

    assert sink.posts == 1 and sink.files == 1


# --------------------------------------------------------------------------- #
# Verdict
# --------------------------------------------------------------------------- #

def test_the_verdict_is_short_and_exit_two_when_it_will_not_fit(tmp_path, capsys):
    _, code, _ = estimate(tmp_path, [album(1, [1])], min_free=1 << 62)
    assert code == 2
    out = capsys.readouterr().out
    assert "SHORT by" in out
    # A SHORT verdict is advisory: the per-file check_disk is the enforcement,
    # and the run stops partway with a resumable cursor rather than corrupting.
    assert "advisory" in out


def test_the_verdict_fits_when_there_is_room(tmp_path, capsys):
    _, code, _ = estimate(tmp_path, [album(1, [1])], min_free=0)
    assert code == 0
    assert "FITS" in capsys.readouterr().out


def test_photo_totals_are_labelled_approximate(tmp_path, capsys):
    estimate(tmp_path, [album(1, [1], kind="photo"), album(2, [2], kind="video")])
    out = capsys.readouterr().out
    photo_line = next(line for line in out.splitlines() if "photos" in line)
    video_line = next(line for line in out.splitlines() if "videos" in line)
    assert "approximate" in photo_line
    assert "approximate" not in video_line


# --------------------------------------------------------------------------- #
# The cursor fix, and no side effects
# --------------------------------------------------------------------------- #

def test_dry_run_writes_nothing_at_all(tmp_path):
    before = sorted(p.name for p in tmp_path.iterdir())
    _, _, root = estimate(tmp_path, [album(1, [1])])

    assert not root.exists()                       # not even the export root
    assert sorted(p.name for p in tmp_path.iterdir()) == before


def test_dry_run_starts_where_the_real_run_would(tmp_path):
    # Without reading the cursor, `--dry-run && run` fails closed on every
    # resumed export: a mostly-downloaded group would be re-measured in full,
    # exit 2, and refuse work that fits comfortably.
    root = paths.export_root(tmp_path, -1001)
    root.mkdir(parents=True)
    filters = build_filters(types="photo")
    State.open(root, chat_id=-1001, chat_title="G", filters=filters).commit(1042)

    assert resume_from(root, filters) == 1042


def test_a_filter_change_makes_dry_run_measure_from_zero(tmp_path):
    # The real run would refuse (exit 7), so reusing its cursor would describe a
    # run that cannot happen.
    root = paths.export_root(tmp_path, -1001)
    root.mkdir(parents=True)
    State.open(root, chat_id=-1001, chat_title="G",
               filters=build_filters(types="photo")).commit(1042)

    assert resume_from(root, build_filters(types="video")) == 0


def test_resume_from_is_zero_with_no_prior_state(tmp_path):
    assert resume_from(paths.export_root(tmp_path, -1001), build_filters()) == 0
    assert not (tmp_path / STATE_NAME).exists()


# --------------------------------------------------------------------------- #
# Anti-drift: the estimate and the real run describe the same work
# --------------------------------------------------------------------------- #

@pytest.mark.parametrize("limit", [None, 2])
def test_dry_run_and_real_run_agree_on_posts_and_files(tmp_path, limit):
    posts = [album(1, [1, 2]), album(3, [3]), album(4, [4, 5, 6]),
             Post(post_id=7, messages=[], max_message_id=7), album(8, [8])]

    sink, _, _ = estimate(tmp_path, posts, limit=limit)

    root = paths.export_root(tmp_path / "real", -1001)
    root.mkdir(parents=True)
    state = State.open(root, chat_id=-1001, chat_title="G", filters=build_filters())
    with Sidecar(root) as sidecar:
        totals = run(run_download(as_agen(posts), fetcher=FakeFetcher(), state=state,
                                  sidecar=sidecar, root=root,
                                  cfg=cfg(tmp_path), limit=limit))

    assert sink.posts == totals.posts
    assert sink.files == totals.downloaded
    # Bytes agree exactly here because the fake payload matches the declared
    # size; against the live API only photo sizes are approximate.
    assert sink.total_bytes == totals.bytes_downloaded


def test_filters_are_described_for_the_report_header():
    described = describe_filters(build_filters(types="video,photo",
                                               since="2026-01-01",
                                               max_size="100MB",
                                               include_text=True))
    assert "types=photo,video" in described
    assert "since=2026-01-01" in described
    assert "max-size" in described and "include-text" in described
    assert describe_filters(build_filters()) == "(none)"
