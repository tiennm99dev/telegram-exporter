"""messages.jsonl: partial-line repair, rotation, and union semantics.

Not in the plan's original file list, but phase 5's success criteria name both
behaviours ("a truncated final line is repaired at startup", "--reset-state
rotates the sidecar"), and neither belongs in the download-decision table.
"""

from __future__ import annotations

import json

from telegram_exporter.sidecar import SIDECAR_NAME, Sidecar
from telegram_exporter.traversal import Post
from tests.support import FakeMsg


def lines(root):
    return (root / SIDECAR_NAME).read_text().splitlines()


def test_a_partial_trailing_line_is_truncated_at_startup(tmp_path):
    # A host crash mid-write otherwise leaves a fragment that breaks every JSONL
    # consumer downstream.
    path = tmp_path / SIDECAR_NAME
    path.write_text('{"message_id": 1}\n{"message_id": 2, "fi')

    with Sidecar(tmp_path):
        pass

    assert lines(tmp_path) == ['{"message_id": 1}']


def test_a_complete_final_line_without_a_newline_is_kept(tmp_path):
    path = tmp_path / SIDECAR_NAME
    path.write_text('{"message_id": 1}\n{"message_id": 2}')

    with Sidecar(tmp_path):
        pass

    assert [json.loads(line)["message_id"] for line in lines(tmp_path)] == [1, 2]


def test_an_intact_file_is_left_alone(tmp_path):
    path = tmp_path / SIDECAR_NAME
    path.write_text('{"message_id": 1}\n')

    with Sidecar(tmp_path):
        pass

    assert lines(tmp_path) == ['{"message_id": 1}']


def test_an_empty_or_absent_file_is_not_a_repair_case(tmp_path):
    with Sidecar(tmp_path):
        pass
    assert lines(tmp_path) == []


def test_rotation_preserves_the_old_records_under_a_timestamp(tmp_path):
    (tmp_path / SIDECAR_NAME).write_text('{"message_id": 1}\n')
    sidecar = Sidecar(tmp_path)

    rotated = sidecar.rotate()

    assert rotated is not None and rotated.exists()
    assert rotated.name.startswith("messages-") and rotated.suffix == ".jsonl"
    assert not (tmp_path / SIDECAR_NAME).exists()
    # Rotation, not deletion: the current sidecar describes exactly one filter
    # regime while the previous one stays inspectable.
    assert json.loads(rotated.read_text())["message_id"] == 1


def test_rotating_a_missing_sidecar_is_a_no_op(tmp_path):
    assert Sidecar(tmp_path).rotate() is None


def test_records_are_append_only_events_so_consumers_union_them(tmp_path):
    """Two runs under different filters both record message 1043. Reading only
    the last record would report a narrower file list than what is on disk."""
    msg_photo = FakeMsg(1043, kind="photo", grouped_id=8)
    post = Post(post_id=1042, messages=[msg_photo], max_message_id=1043)

    for _ in range(2):
        with Sidecar(tmp_path) as sidecar:
            sidecar.append(post, [])
            sidecar.fsync()

    records = [json.loads(line) for line in lines(tmp_path)]
    assert [r["message_id"] for r in records] == [1043, 1043]
    assert all(r["post_id"] == 1042 and r["grouped_id"] == 8 for r in records)


def test_a_text_only_message_gets_the_same_shape_with_no_files(tmp_path):
    post = Post(post_id=12, messages=[FakeMsg(12, text="hello")], max_message_id=12)
    with Sidecar(tmp_path) as sidecar:
        sidecar.append(post, [])
        sidecar.fsync()

    record = json.loads(lines(tmp_path)[0])
    assert record["files"] == [] and record["caption"] == "hello"
    assert record["errors"] == []


def test_sender_name_is_only_taken_from_an_already_cached_sender(tmp_path):
    # Never an extra get_entity call: on a 100k-message sweep that is 100k extra
    # RPCs and a flood ban, in exchange for a display string.
    class Sender:
        first_name, last_name, username, title = "Alice", None, "alice", None

    with_sender = FakeMsg(1, kind="photo", sender=Sender(), sender_id=777)
    without = FakeMsg(2, kind="photo", sender=None, sender_id=778)
    post = Post(post_id=1, messages=[with_sender, without], max_message_id=2)

    with Sidecar(tmp_path) as sidecar:
        sidecar.append(post, [])
        sidecar.fsync()

    records = [json.loads(line) for line in lines(tmp_path)]
    assert records[0]["sender_name"] == "Alice"
    assert records[1]["sender_name"] is None
    assert records[1]["sender_id"] == 778
