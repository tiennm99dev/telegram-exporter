"""Album grouping, filter-invariant identity, and the album-split tripwire.

All synthetic: no client, no network.
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest

from telegram_exporter.media import media_size
from telegram_exporter.traversal import (
    AlbumSplitError,
    Filters,
    SweepOrderError,
    TraversalError,
    build_filters,
    iter_posts,
    keep,
    parse_date,
    parse_size,
)
from tests.support import FakeClient, FakeMsg, collect

NO_FILTERS = Filters()


def sweep(messages, *, filters=NO_FILTERS, after_id=0):
    return collect(iter_posts(FakeClient(messages), object(),
                              after_id=after_id, filters=filters))


# --------------------------------------------------------------------------- #
# Grouping
# --------------------------------------------------------------------------- #

def test_a_three_photo_album_is_one_post():
    posts = sweep([FakeMsg(i, kind="photo", grouped_id=7) for i in (10, 11, 12)])
    assert len(posts) == 1
    assert posts[0].post_id == 10
    assert [m.id for m in posts[0].messages] == [10, 11, 12]
    assert posts[0].max_message_id == 12


def test_two_adjacent_albums_do_not_merge():
    posts = sweep([FakeMsg(10, kind="photo", grouped_id=7),
                   FakeMsg(11, kind="photo", grouped_id=7),
                   FakeMsg(12, kind="photo", grouped_id=8),
                   FakeMsg(13, kind="photo", grouped_id=8)])
    assert [p.post_id for p in posts] == [10, 12]


def test_a_standalone_message_between_albums_gets_its_own_post():
    posts = sweep([FakeMsg(10, kind="photo", grouped_id=7),
                   FakeMsg(11, kind="video"),
                   FakeMsg(12, kind="photo", grouped_id=8)])
    assert [p.post_id for p in posts] == [10, 11, 12]


def test_an_album_at_the_end_of_history_is_flushed():
    posts = sweep([FakeMsg(10, kind="video"),
                   FakeMsg(11, kind="photo", grouped_id=9),
                   FakeMsg(12, kind="photo", grouped_id=9)])
    assert [p.post_id for p in posts] == [10, 11]
    assert len(posts[-1].messages) == 2


def test_after_id_is_an_exclusive_lower_bound():
    msgs = [FakeMsg(i, kind="photo") for i in (10, 11, 12)]
    assert [p.post_id for p in sweep(msgs, after_id=11)] == [12]


# --------------------------------------------------------------------------- #
# Invariant 1 - identity is filter-invariant
# --------------------------------------------------------------------------- #

def test_identity_survives_a_filter_dropping_members():
    album = [FakeMsg(10, kind="photo", grouped_id=7),
             FakeMsg(11, kind="video", grouped_id=7),
             FakeMsg(12, kind="photo", grouped_id=7)]
    photos = sweep(album, filters=build_filters(types="photo"))[0]
    videos = sweep(album, filters=build_filters(types="video"))[0]

    assert photos.post_id == videos.post_id == 10
    assert photos.max_message_id == videos.max_message_id == 12
    assert [m.id for m in photos.messages] == [10, 12]
    assert [m.id for m in videos.messages] == [11]


def test_a_fully_filtered_group_still_yields_a_post_so_the_cursor_advances():
    # Skipping empty posts stalls the cursor: a --types video run over a
    # photo-heavy tail would advance only at videos, so every resume re-sweeps
    # tens of thousands of messages to download nothing.
    posts = sweep([FakeMsg(10, kind="photo", grouped_id=7),
                   FakeMsg(11, kind="photo", grouped_id=7)],
                  filters=build_filters(types="video"))
    assert len(posts) == 1
    assert posts[0].messages == []
    assert posts[0].max_message_id == 11


# --------------------------------------------------------------------------- #
# The tripwire and the ordering assertion
# --------------------------------------------------------------------------- #

def test_a_reopened_grouped_id_raises_album_split_error():
    with pytest.raises(AlbumSplitError, match="grouped_id 7 reopened"):
        sweep([FakeMsg(10, kind="photo", grouped_id=7),
               FakeMsg(11, kind="photo", grouped_id=8),
               FakeMsg(12, kind="photo", grouped_id=9),
               FakeMsg(13, kind="photo", grouped_id=7)])   # A B C A


def test_the_tripwire_catches_an_immediately_reopened_album():
    with pytest.raises(AlbumSplitError):
        sweep([FakeMsg(10, kind="photo", grouped_id=7),
               FakeMsg(11, kind="video"),
               FakeMsg(12, kind="photo", grouped_id=7)])    # A B A


def test_a_non_ascending_stream_is_refused():
    class Descending(FakeClient):
        def iter_messages(self, entity, **kwargs):
            async def gen():
                for m in self.messages:
                    yield m
            return gen()

    with pytest.raises(SweepOrderError, match="not strictly ascending"):
        collect(iter_posts(Descending([FakeMsg(12, kind="photo"),
                                       FakeMsg(10, kind="photo")]),
                           object(), filters=NO_FILTERS))


def test_the_album_buffer_is_bounded():
    with pytest.raises(SweepOrderError, match="album buffer exceeded"):
        sweep([FakeMsg(i, kind="photo", grouped_id=7) for i in range(100, 130)])


def test_the_sweep_tripwires_survive_python_dash_o():
    # `python -O` strips bare asserts. These two guard the same silent corruption
    # as AlbumSplitError beside them, so they are real exceptions.
    assert issubclass(SweepOrderError, TraversalError)
    assert issubclass(AlbumSplitError, TraversalError)
    assert not issubclass(SweepOrderError, AssertionError)


# --------------------------------------------------------------------------- #
# Filters
# --------------------------------------------------------------------------- #

def test_text_only_messages_are_dropped_by_default():
    posts = sweep([FakeMsg(10, text="hello"), FakeMsg(11, kind="photo")])
    assert [p.post_id for p in posts] == [10, 11]
    assert posts[0].messages == []                      # yielded, but empty
    assert len(posts[1].messages) == 1


def test_include_text_keeps_text_messages_without_media():
    posts = sweep([FakeMsg(10, text="hello")],
                  filters=build_filters(include_text=True))
    assert len(posts[0].messages) == 1
    assert posts[0].messages[0].file is None            # nothing to download


def test_a_link_preview_is_not_media():
    # Telethon's Message.photo also returns the web preview's photo, so without
    # the web_preview guard every message containing a URL would look like an
    # attachment.
    msg = FakeMsg(10, kind="photo", text="look", web_preview=object())
    assert keep(msg, NO_FILTERS) is False
    assert keep(msg, build_filters(include_text=True)) is True


def test_filters_compose():
    filters = build_filters(types="video", since="2026-01-01", max_size="50MB")
    inside = FakeMsg(1, kind="video", size=1_000_000,
                     date=datetime(2026, 6, 1, tzinfo=UTC))
    too_old = FakeMsg(2, kind="video", size=1_000_000,
                      date=datetime(2025, 6, 1, tzinfo=UTC))
    too_big = FakeMsg(3, kind="video", size=99_000_000,
                      date=datetime(2026, 6, 1, tzinfo=UTC))
    wrong_kind = FakeMsg(4, kind="photo", size=1_000,
                         date=datetime(2026, 6, 1, tzinfo=UTC))
    assert keep(inside, filters) is True
    assert not any(keep(m, filters) for m in (too_old, too_big, wrong_kind))


def test_unknown_size_is_included_by_max_size_never_silently_dropped():
    msg = FakeMsg(1, kind="document", size=None)
    assert media_size(msg) is None
    assert keep(msg, build_filters(max_size="10MB")) is True


def test_media_size_is_none_rather_than_zero_for_all_three_consumers():
    # The estimator, the validator and the disk guard each have their own rule
    # for None; what matters here is that the helper never raises and never
    # invents a 0.
    assert media_size(FakeMsg(1, kind="document", size=None)) is None
    assert media_size(FakeMsg(2, text="hi")) is None
    assert media_size(FakeMsg(3, kind="photo", size=17)) == 17


def test_unknown_types_are_rejected_at_parse_time():
    with pytest.raises(ValueError, match="unknown media types"):
        build_filters(types="photo,sticker")


@pytest.mark.parametrize("spec", [" ", ",", " , ", ",,"])
def test_a_token_less_types_value_is_refused(spec):
    """`--types " "` (easy to produce from an unset shell variable) used to build
    an empty kind set: a filter that drops every media kind while serializing as
    "no filter". A run would sweep all history, download nothing, commit an
    end-of-history cursor and set completed_at - and a later correct run would see
    matching filters, resume from that cursor and report success having fetched
    nothing. Silent total data loss, so it is refused at the boundary."""
    with pytest.raises(ValueError, match="names no media kind"):
        build_filters(types=spec)


def test_an_empty_kind_set_could_not_masquerade_as_no_filter():
    # Second line of defense, in case a caller bypasses build_filters.
    assert Filters(types=frozenset()).to_state()["types"] == []
    assert Filters(types=None).to_state()["types"] is None


@pytest.mark.parametrize("text,expected", [
    ("100MB", 100_000_000), ("2GiB", 2 * 2**30), ("1500000", 1_500_000),
    ("50 MB", 50_000_000), ("1KiB", 1024), ("1.5GB", 1_500_000_000),
])
def test_parse_size(text, expected):
    assert parse_size(text) == expected


def test_parse_size_rejects_nonsense():
    with pytest.raises(ValueError):
        parse_size("plenty")


def test_dates_are_utc_aware_so_comparison_cannot_raise():
    # msg.date is UTC-aware; a naive bound would raise TypeError mid-sweep.
    assert parse_date("2026-01-01").tzinfo is not None
    assert parse_date("2026-01-01T00:00:00+07:00").tzinfo is not None


def test_stored_filters_are_verbatim_and_comparable():
    stored = build_filters(types="video,photo", max_size="100MB",
                           include_text=True).to_state()
    assert stored == {"types": ["photo", "video"], "since": None, "until": None,
                      "max_size": 100_000_000, "include_text": True}
