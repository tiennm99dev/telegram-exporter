"""Path safety. Every row of the plan's test table, plus the invariant that no
sanitized name can escape its post directory."""

from __future__ import annotations

from pathlib import Path

import pytest

from telegram_exporter.paths import (
    MAX_NAME_BYTES,
    derive_filename,
    export_root,
    post_dir,
    safe_join,
    sanitize,
)
from tests.support import FakeMsg


@pytest.mark.parametrize("raw,expected", [
    ("../../../etc/passwd", "passwd"),
    ("a/b/c.jpg", "c.jpg"),
    ("..", "FALLBACK"),
    (".", "FALLBACK"),
    ("", "FALLBACK"),
    ("...", "FALLBACK"),
    ("\x00evil.jpg", "evil.jpg"),
    ("....jpg", "jpg"),
    ("-rf.jpg", "rf.jpg"),
    ("--force  me.jpg", "force me.jpg"),
    ("a\\b\\c.jpg", "c.jpg"),
    ("we:ird*na?me.jpg", "we_ird_na_me.jpg"),
    ("tab\tsep.jpg", "tabsep.jpg"),          # control chars are removed, not spaced
    ("sp  aced.jpg", "sp aced.jpg"),         # whitespace runs collapse
    ("/etc/shadow", "shadow"),
])
def test_sanitize_table(raw, expected):
    assert sanitize(raw, fallback="FALLBACK") == expected


def test_truncation_is_byte_counted_and_keeps_the_extension():
    # 300 CJK characters is ~900 bytes: a character-counted truncation would
    # produce a name ext4 rejects with OSError: File name too long.
    name = "あ" * 300 + ".jpg"
    out = sanitize(name, fallback="x")
    assert len(out.encode()) <= MAX_NAME_BYTES
    assert out.endswith(".jpg")
    assert len(out) > 10                       # not collapsed to the fallback


def test_truncation_of_a_pathological_extension_still_yields_a_usable_name():
    out = sanitize("stem." + "e" * 400, fallback="fb")
    assert 0 < len(out.encode()) <= MAX_NAME_BYTES


def test_no_sanitized_name_escapes_its_post_dir(tmp_path):
    hostile = ["../../../etc/passwd", "..", "/etc/shadow", "a/../../b",
               "\x00../x", "....//..//etc/passwd"]
    base = post_dir(export_root(tmp_path, -1001), 1042)
    for raw in hostile:
        joined = safe_join(base, sanitize(raw, fallback="fb"))
        assert joined.is_relative_to(base.resolve())


def test_safe_join_raises_rather_than_repairing():
    base = Path("/tmp/exports/g-1001/1042")
    for escape in ["../x", "../../etc/passwd", "/etc/passwd", ".."]:
        with pytest.raises(ValueError):
            safe_join(base, escape)


def test_export_root_is_the_chat_id_never_the_title(tmp_path):
    assert export_root(tmp_path, -1001234567890).name == "g-1001234567890"


@pytest.mark.parametrize("bad", ["..", "../../etc", "", "1042; rm -rf", None, 1.5, True])
def test_int_only_path_components(tmp_path, bad):
    # A group title cannot reach a path component: there is no code path from
    # title to export_root, and anything non-int is refused by type.
    with pytest.raises(TypeError):
        export_root(tmp_path, bad)
    with pytest.raises(TypeError):
        post_dir(tmp_path, bad)


def test_derive_filename_is_keyed_on_message_id():
    msg = FakeMsg(1043, kind="photo")
    assert derive_filename(msg) == "1043_photo.jpg"


def test_hostile_declared_filename_becomes_a_leaf():
    msg = FakeMsg(7, kind="document", name="../../../etc/passwd", ext=".bin")
    assert derive_filename(msg) == "7_passwd.bin"


def test_two_media_in_one_post_get_distinct_names():
    a, b = FakeMsg(1042, kind="photo", grouped_id=9), FakeMsg(1043, kind="photo",
                                                              grouped_id=9)
    assert derive_filename(a) != derive_filename(b)


def test_filename_is_identical_across_filter_sets():
    # derive_filename takes no filter argument, which is the point: the same
    # message addresses the same file whether --types photo or --types video was
    # used, so dedupe survives a filter change.
    msg = FakeMsg(1044, kind="video", name="clip.mp4")
    assert derive_filename(msg) == derive_filename(msg) == "1044_clip.mp4"


@pytest.mark.parametrize("name,expected", [
    ("holiday.\x00jpg", "7_holiday.jpg"),        # NUL: used to escape as ValueError
    # The ESC byte is removed; the remaining "[31m" is inert text, not an escape.
    ("clip.\x1b[31mmp4", "7_clip.[31mmp4"),
    ("doc.j:pg", "7_doc.j_pg"),                  # reserved character
    ("shot.jp g", "7_shot.jp g"),
    ("archive.tar.gz", "7_archive.tar.gz"),      # a real double extension survives
    ("weird.", "7_weird.bin"),                   # empty extension falls back
])
def test_the_extension_is_sanitized_like_the_stem(name, expected):
    """The extension comes from the same attacker-supplied filename attribute as
    the stem. Sanitizing only the stem left a hole: a NUL in the extension made
    Path.resolve raise inside safe_join, and that ValueError escaped the download
    loop - so the cursor never advanced past the message and every later run died
    on it."""
    assert derive_filename(FakeMsg(7, kind="document", name=name)) == expected


def test_no_control_character_survives_anywhere_in_a_filename():
    for raw in ["a\x00.b\x00c", "\x1b].jpg", "x.\ny", "n.\r\tz"]:
        out = derive_filename(FakeMsg(3, kind="document", name=raw))
        assert not any(ord(ch) < 0x20 or ord(ch) == 0x7F for ch in out), out


def test_a_whole_filename_stays_inside_ext4s_255_byte_component_limit():
    # The stem is capped at 200 UTF-8 bytes, so the "{id}_" prefix and the
    # extension have to be byte-counted too or the total overflows.
    worst = FakeMsg(9_999_999_999_999_999,
                    kind="document",
                    name="あ" * 300 + "." + "ん" * 16)
    assert len(derive_filename(worst).encode()) <= 255


def test_missing_name_falls_back_to_the_media_kind():
    assert derive_filename(FakeMsg(5, kind="voice")) == "5_voice.ogg"


def test_fallbacks_never_include_a_timestamp_or_anything_nondeterministic():
    msg = FakeMsg(11, kind="document", name="   ", ext=".pdf")
    assert derive_filename(msg) == derive_filename(msg) == "11_document.pdf"
