"""The resume cursor: Invariant 3 (filters stored verbatim, mismatch refused with
a named diff) and atomic saves."""

from __future__ import annotations

import json

import pytest

from telegram_exporter.session import Abort
from telegram_exporter.state import STATE_NAME, State
from telegram_exporter.traversal import build_filters

CHAT = -1001234567890


def open_state(root, *, filters=None, chat_id=CHAT, reset=False):
    return State.open(root, chat_id=chat_id, chat_title="My Group",
                      filters=filters or build_filters(), reset=reset)


def test_a_new_state_file_starts_at_zero(tmp_path):
    state = open_state(tmp_path)
    assert state.cursor_id == 0
    assert (tmp_path / STATE_NAME).exists()
    assert json.loads((tmp_path / STATE_NAME).read_text())["chat_id"] == CHAT


def test_commit_persists_and_leaves_no_temp_file(tmp_path):
    state = open_state(tmp_path)
    state.commit(1042)
    assert json.loads((tmp_path / STATE_NAME).read_text())["cursor_id"] == 1042
    assert list(tmp_path.glob("*.tmp")) == []
    assert open_state(tmp_path).cursor_id == 1042


def test_a_cursor_regression_is_ignored(tmp_path):
    state = open_state(tmp_path)
    state.commit(1042)
    state.commit(7)
    assert state.cursor_id == 1042


def test_completed_at_is_set_only_on_clean_exhaustion(tmp_path):
    state = open_state(tmp_path)
    assert state.data["completed_at"] is None
    state.mark_completed()
    assert state.data["completed_at"] is not None
    # A new run clears it: the field answers "did *this* export finish?".
    assert open_state(tmp_path).data["completed_at"] is None


def test_a_filter_change_is_refused_and_names_what_changed(tmp_path):
    open_state(tmp_path, filters=build_filters(types="photo,video",
                                               max_size="100MB")).commit(1042)
    with pytest.raises(Abort) as excinfo:
        open_state(tmp_path, filters=build_filters(types="video"))
    assert excinfo.value.code == 7
    reason = excinfo.value.reason
    assert "filter mismatch" in reason
    assert "types" in reason and "max_size" in reason
    assert "--reset-state" in reason


def test_toggling_include_text_is_a_filter_change(tmp_path):
    # It changes what a post *contains*, so running without it and then with it
    # against the same cursor would silently skip every text record.
    open_state(tmp_path, filters=build_filters()).commit(50)
    with pytest.raises(Abort) as excinfo:
        open_state(tmp_path, filters=build_filters(include_text=True))
    assert excinfo.value.code == 7
    assert "include_text" in excinfo.value.reason


def test_a_different_chat_against_an_existing_state_file_is_refused(tmp_path):
    open_state(tmp_path)
    with pytest.raises(Abort) as excinfo:
        open_state(tmp_path, chat_id=-100999)
    assert excinfo.value.code == 7
    assert "chat mismatch" in excinfo.value.reason


def test_reset_state_starts_over_without_complaining(tmp_path):
    open_state(tmp_path, filters=build_filters(types="photo")).commit(1042)
    fresh = open_state(tmp_path, filters=build_filters(types="video"), reset=True)
    assert fresh.cursor_id == 0
    assert fresh.data["filters"]["types"] == ["video"]


def test_limit_is_not_part_of_the_stored_filter_set(tmp_path):
    # A `--limit 20` run is a resumable partial export; if --limit were stored,
    # a limited test run would poison every later full run.
    assert "limit" not in open_state(tmp_path).data["filters"]


@pytest.mark.parametrize("filters_value", ["MISSING", None, "not-a-dict"])
def test_a_state_file_with_no_filter_set_is_refused_as_its_own_condition(
        tmp_path, filters_value):
    data = {"version": 1, "chat_id": CHAT, "cursor_id": 99, "filters": filters_value}
    if filters_value == "MISSING":
        del data["filters"]
    (tmp_path / STATE_NAME).write_text(json.dumps(data))

    with pytest.raises(Abort) as excinfo:
        open_state(tmp_path)

    assert excinfo.value.code == 7
    assert "no filter set recorded" in excinfo.value.reason
    # Not reported as a phantom diff on a flag the user never passed.
    assert "include_text:" not in excinfo.value.reason


def test_a_corrupt_state_file_is_refused_rather_than_ignored(tmp_path):
    (tmp_path / STATE_NAME).write_text("{not json")
    with pytest.raises(Abort) as excinfo:
        open_state(tmp_path)
    assert excinfo.value.code == 7


def test_read_is_side_effect_free_for_dry_run(tmp_path):
    assert State.read(tmp_path) is None
    assert not (tmp_path / STATE_NAME).exists()
