"""End-to-end dispatch through cli.main() against a fake Telegram client.

This file exists because of a specific escape: `Config.max_flood_wait` was
removed from the dataclass while `cli._real_run` kept passing it, so every real
export died with an uncaught TypeError after taking both locks and writing the
state file - and 180 tests still passed, because every one of them built Config
by hand and none of them ever called main().

Anything that only the wiring can break belongs here.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from telegram_exporter import cli, session as session_mod
from telegram_exporter.state import STATE_NAME
from tests.support import FakeMsg


class FakeClient:
    """Enough of a TelegramClient for a whole run, with no network."""

    def __init__(self, messages, entity):
        self.messages = messages
        self.entity = entity
        self.downloads: list[int] = []
        self.flood_sleep_threshold = None
        self.disconnected = False

    def iter_messages(self, entity, *, reverse=False, offset_id=0, **kwargs):
        async def gen():
            for m in self.messages:
                if m.id > offset_id:
                    yield m
        return gen()

    async def download_media(self, msg, file=None):
        self.downloads.append(msg.id)
        file.write(b"x" * (msg.file.size or 10))
        return file

    async def get_messages(self, entity, ids=None):
        return next((m for m in self.messages if m.id == ids), None)

    async def connect(self):
        return None

    async def is_user_authorized(self):
        return True                      # a second run is non-interactive

    async def disconnect(self):
        self.disconnected = True


class FakeEntity:
    id, title, username = 1234567890, "My Group", "mygroup"


@pytest.fixture
def wired(monkeypatch, tmp_path):
    """Patch out only the network boundary: credentials, client, entity."""
    monkeypatch.setenv("TG_API_ID", "12345")
    monkeypatch.setenv("TG_API_HASH", "0" * 32)
    monkeypatch.setenv("XDG_STATE_HOME", str(tmp_path / "state"))

    messages = [FakeMsg(10, kind="photo", grouped_id=7, size=100),
                FakeMsg(11, kind="photo", grouped_id=7, size=100),
                FakeMsg(12, text="just talking"),
                FakeMsg(13, kind="video", size=500)]
    client = FakeClient(messages, FakeEntity())

    class FakeClientFactory:
        def __init__(self, *a, **kw):
            pass

    def fake_telegram_client(session, api_id, api_hash):
        return client

    monkeypatch.setattr(session_mod, "TelegramClient", fake_telegram_client)

    async def fake_resolve(_client, _spec):
        return client.entity, -1001234567890

    monkeypatch.setattr(cli, "resolve_entity", fake_resolve)
    return client


def run_cli(monkeypatch, *argv) -> int:
    monkeypatch.setattr("sys.argv", ["tg-export", *argv])
    return cli.main()


# --------------------------------------------------------------------------- #
# The real run - the path C1 broke
# --------------------------------------------------------------------------- #

def test_a_real_export_runs_end_to_end(wired, monkeypatch, tmp_path):
    out = tmp_path / "exports"
    code = run_cli(monkeypatch, "--group", "@mygroup", "--out", str(out))

    assert code == 0
    root = out / "g-1001234567890"
    assert sorted(p.name for p in (root / "10").iterdir()) == \
        ["10_photo.jpg", "11_photo.jpg"]
    assert (root / "13" / "13_video.mp4").exists()
    assert (root / "title.txt").read_text().strip() == "My Group"
    assert json.loads((root / STATE_NAME).read_text())["completed_at"] is not None
    assert wired.disconnected, "disconnect must run even on the success path"


def test_a_real_export_honors_the_flood_ceiling_flag(wired, monkeypatch, tmp_path):
    # The ceiling reaches the network through Fetcher and iter_posts, not
    # through Config. Passing the flag must not blow up the wiring.
    code = run_cli(monkeypatch, "--group", "@mygroup",
                   "--out", str(tmp_path / "e"), "--max-flood-wait", "3600")
    assert code == 0


def test_a_second_run_downloads_nothing(wired, monkeypatch, tmp_path):
    out = str(tmp_path / "exports")
    assert run_cli(monkeypatch, "--group", "@mygroup", "--out", out) == 0
    before = len(wired.downloads)
    assert run_cli(monkeypatch, "--group", "@mygroup", "--out", out) == 0
    assert len(wired.downloads) == before, "resume re-downloaded completed files"


def test_limit_zero_is_a_non_interactive_connectivity_check(wired, monkeypatch,
                                                            tmp_path):
    code = run_cli(monkeypatch, "--group", "@mygroup",
                   "--out", str(tmp_path / "e"), "--limit", "0")
    assert code == 0
    assert wired.downloads == []


# --------------------------------------------------------------------------- #
# Dry run
# --------------------------------------------------------------------------- #

def test_dry_run_writes_nothing_and_downloads_nothing(wired, monkeypatch,
                                                      tmp_path, capsys):
    out = tmp_path / "exports"
    code = run_cli(monkeypatch, "--group", "@mygroup", "--out", str(out),
                   "--dry-run", "--min-free", "0")

    assert code == 0
    assert wired.downloads == []
    assert not out.exists()
    assert "Remaining:" in capsys.readouterr().out


def test_dry_run_exits_two_when_it_will_not_fit(wired, monkeypatch, tmp_path):
    code = run_cli(monkeypatch, "--group", "@mygroup",
                   "--out", str(tmp_path / "e"), "--dry-run",
                   "--min-free", "1000GiB")
    assert code == 2


# --------------------------------------------------------------------------- #
# Exit-code contract through the real dispatcher
# --------------------------------------------------------------------------- #

def test_a_filter_change_exits_seven_and_names_the_change(wired, monkeypatch,
                                                          tmp_path, caplog):
    out = str(tmp_path / "exports")
    assert run_cli(monkeypatch, "--group", "@mygroup", "--out", out,
                   "--types", "photo") == 0
    code = run_cli(monkeypatch, "--group", "@mygroup", "--out", out,
                   "--types", "video")

    assert code == 7
    assert "filter mismatch" in caplog.text and "types" in caplog.text


def test_a_token_less_types_value_never_reaches_a_run(wired, monkeypatch, tmp_path):
    code = run_cli(monkeypatch, "--group", "@mygroup",
                   "--out", str(tmp_path / "e"), "--types", " ")
    assert code == 1
    assert wired.downloads == []


def test_missing_credentials_exit_one(monkeypatch, tmp_path):
    monkeypatch.delenv("TG_API_ID", raising=False)
    monkeypatch.delenv("TG_API_HASH", raising=False)
    assert run_cli(monkeypatch, "--group", "@g", "--out", str(tmp_path)) == 1


def test_an_unexpected_exception_still_honors_the_exit_contract(wired, monkeypatch,
                                                                tmp_path):
    async def boom(_client, _spec):
        raise RuntimeError("something nobody predicted")

    monkeypatch.setattr(cli, "resolve_entity", boom)
    code = run_cli(monkeypatch, "--group", "@mygroup", "--out", str(tmp_path / "e"))
    assert code == 1                      # not a bare traceback with no code


def test_a_second_concurrent_run_exits_three(wired, monkeypatch, tmp_path):
    out = tmp_path / "exports"
    root = out / "g-1001234567890"
    root.mkdir(parents=True)
    with cli.exclusive(root / cli.LOCK_NAME):
        code = run_cli(monkeypatch, "--group", "@mygroup", "--out", str(out))
    assert code == 3
