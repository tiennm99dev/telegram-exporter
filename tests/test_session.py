"""Session file privacy, the git-safety refusal, and the flood-wait primitives.

Phase 2's success criteria name all three; none of them belongs in the logging or
lock test files, and every one is a silent-failure mode if it regresses.
"""

from __future__ import annotations

import os
import re
import stat
import subprocess
from pathlib import Path

import pytest
from telethon.errors import FloodWaitError

from telegram_exporter import session as session_mod
from telegram_exporter.session import (
    Abort,
    _flood_sleep,
    aiter_with_flood_retry,
    assert_not_committable,
    assert_session_private,
    default_session_path,
    load_api_credentials,
    prepare_session_path,
    with_flood_retry,
)
from tests.support import FakeMsg, collect, run

CLI_SRC = Path(__file__).resolve().parents[1] / "src" / "telegram_exporter" / "cli.py"


@pytest.fixture
def no_sleep(monkeypatch):
    """Assert the wait *happens* without waiting for it."""
    slept = []

    async def fake_sleep(seconds):
        slept.append(seconds)

    monkeypatch.setattr(session_mod.asyncio, "sleep", fake_sleep)
    return slept


# --------------------------------------------------------------------------- #
# Session file privacy
# --------------------------------------------------------------------------- #

def test_the_session_suffix_is_added_so_the_right_file_is_protected(tmp_path):
    # Telethon appends '.session' when absent, so chmod-ing the un-suffixed path
    # passed to --session would protect nothing.
    created = prepare_session_path(tmp_path / "mine")
    assert created.name == "mine.session"
    assert created.exists()


def test_a_new_session_file_is_created_private(tmp_path):
    created = prepare_session_path(tmp_path / "s.session")
    assert stat.S_IMODE(created.stat().st_mode) & 0o077 == 0


def test_an_existing_loose_session_file_is_tightened(tmp_path):
    loose = tmp_path / "s.session"
    loose.touch()
    os.chmod(loose, 0o644)

    prepare_session_path(loose)

    assert stat.S_IMODE(loose.stat().st_mode) == 0o600


def test_every_sqlite_sibling_is_checked_not_just_the_session(tmp_path):
    # -journal, -wal and -shm hold the same secrets as the database itself.
    created = prepare_session_path(tmp_path / "s.session")
    sibling = tmp_path / "s.session-journal"
    sibling.touch()
    os.chmod(sibling, 0o644)

    with pytest.raises(SystemExit, match="insecure mode"):
        assert_session_private(created)


def test_a_private_session_passes_the_assertion(tmp_path):
    created = prepare_session_path(tmp_path / "s.session")
    (tmp_path / "s.session-journal").touch(mode=0o600)
    assert_session_private(created)


def test_the_default_session_path_is_outside_any_repo():
    # The safe location is the default, so the git check only bites a
    # deliberately chosen path.
    assert default_session_path().is_absolute()
    assert "tg-export" in str(default_session_path())


def test_umask_is_the_first_statement_of_main():
    # It has to precede anything that can create a file: sqlite writes the auth
    # key *during* login, so a post-hoc chmod is provably too late.
    body = CLI_SRC.read_text().split("def main()")[1]
    first = next(line.strip() for line in body.splitlines()[1:] if line.strip())
    assert first.startswith("os.umask(0o077)")


# --------------------------------------------------------------------------- #
# Git safety - robust to --out / --session pointing anywhere
# --------------------------------------------------------------------------- #

def _git_repo(tmp_path: Path) -> Path:
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    (repo / ".gitignore").write_text("ignored/\n*.session\n")
    return repo


def test_a_committable_path_inside_a_repo_is_refused(tmp_path):
    repo = _git_repo(tmp_path)
    with pytest.raises(SystemExit, match="refusing"):
        assert_not_committable(repo / "exports", is_dir=True)


def test_an_ignored_path_inside_a_repo_is_allowed(tmp_path):
    repo = _git_repo(tmp_path)
    (repo / "ignored").mkdir()
    assert_not_committable(repo / "ignored")
    assert_not_committable(repo / "creds.session")


def test_a_directory_pattern_matches_before_the_directory_exists(tmp_path):
    # git check-ignore returns "not ignored" for `exports` when it cannot see
    # that it is a directory, so without the trailing-slash retry the export root
    # would be refused on the first run and accepted on every run after it.
    repo = _git_repo(tmp_path)
    assert not (repo / "ignored").exists()
    assert_not_committable(repo / "ignored", is_dir=True)
    assert_not_committable(repo / "ignored" / "g-1001", is_dir=True)


def test_the_directory_retry_never_excuses_a_file(tmp_path):
    # The retry asks git to read the path as a directory, which would be unsound
    # for a session file: a `.gitignore` line of `creds.session/` must not excuse
    # a *file* named creds.session, which git would happily commit.
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    (repo / ".gitignore").write_text("creds.session/\n")

    assert_not_committable(repo / "creds.session", is_dir=True)     # a dir: ignored
    with pytest.raises(SystemExit):
        assert_not_committable(repo / "creds.session")              # a file: refused


def test_a_path_outside_any_repo_is_allowed(tmp_path):
    assert_not_committable(tmp_path / "anywhere" / "exports")


def test_a_nonexistent_path_is_still_checked(tmp_path):
    # An export root does not exist on a first run, which is exactly when this
    # check matters; running git from a nonexistent cwd would raise instead.
    repo = _git_repo(tmp_path)
    with pytest.raises(SystemExit):
        assert_not_committable(repo / "deep" / "not" / "created" / "yet")


# --------------------------------------------------------------------------- #
# Credentials
# --------------------------------------------------------------------------- #

def test_missing_credentials_produce_an_actionable_error(monkeypatch):
    monkeypatch.delenv("TG_API_ID", raising=False)
    monkeypatch.delenv("TG_API_HASH", raising=False)
    with pytest.raises(Abort) as excinfo:
        load_api_credentials()
    assert "my.telegram.org" in excinfo.value.reason


def test_a_non_numeric_api_id_is_named(monkeypatch):
    monkeypatch.setenv("TG_API_ID", "abc")
    monkeypatch.setenv("TG_API_HASH", "hash")
    with pytest.raises(Abort, match="must be an integer"):
        load_api_credentials()


# --------------------------------------------------------------------------- #
# Flood-wait primitives
# --------------------------------------------------------------------------- #

def test_a_flood_wait_is_slept_through_once_by_default(no_sleep):
    calls = []

    async def flaky():
        calls.append(1)
        if len(calls) == 1:
            raise FloodWaitError(request=None, capture=30)
        return "ok"

    assert run(with_flood_retry(flaky)) == "ok"
    assert len(calls) == 2
    assert no_sleep and no_sleep[0] >= 30       # the wait plus jitter, never a tight retry


def test_a_wait_beyond_the_explicit_ceiling_exits_six(no_sleep):
    with pytest.raises(Abort) as excinfo:
        run(_flood_sleep(FloodWaitError(request=None, capture=7200), 3600))
    assert excinfo.value.code == 6
    assert not no_sleep                          # aborted instead of sleeping


def test_with_no_ceiling_even_a_four_hour_wait_is_slept(no_sleep):
    run(_flood_sleep(FloodWaitError(request=None, capture=4 * 3600), None))
    assert no_sleep[0] >= 4 * 3600


def test_the_flood_log_names_the_computed_wake_time(no_sleep, caplog):
    # The mitigation for the no-ceiling default: an unattended four-hour sleep
    # has to read as a sleep rather than as a hang.
    with caplog.at_level("WARNING"):
        run(_flood_sleep(FloodWaitError(request=None, capture=90), None))
    assert "sleeping until" in caplog.text
    assert re.search(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}", caplog.text)


def test_a_mid_sweep_flood_wait_resumes_from_the_last_yielded_id(no_sleep):
    """The single-value wrapper cannot cover `async for`: Telethon raises on the
    *next page fetch*, deep inside the loop. Rebuilding from the last yielded id
    is what keeps the sweep gap-free."""
    messages = [FakeMsg(i, kind="photo") for i in (10, 11, 12, 13)]
    attempts = []

    def make_agen(since):
        attempts.append(since)

        async def gen():
            for m in messages:
                if m.id <= since:
                    continue
                if m.id == 12 and len(attempts) == 1:
                    raise FloodWaitError(request=None, capture=5)
                yield m

        return gen()

    got = collect(aiter_with_flood_retry(make_agen, start_id=0))

    assert [m.id for m in got] == [10, 11, 12, 13]    # no gap, no replay
    assert attempts == [0, 11]                        # rebuilt from the last yielded
    assert len(no_sleep) == 1
