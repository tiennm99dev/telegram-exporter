"""Logging policy and the credential surface.

telethon's request logging carries api_id, phone_number and phone_code_hash, so
its floor is enforced by this test rather than by a checkbox.
"""

from __future__ import annotations

import logging
from pathlib import Path

import pytest

from telegram_exporter.cli import build_parser, configure_logging

SRC = Path(__file__).resolve().parents[1] / "src" / "telegram_exporter"
SOURCES = sorted(SRC.glob("*.py"))


@pytest.mark.parametrize("verbose", [False, True])
def test_the_telethon_logger_never_drops_below_warning(verbose):
    configure_logging(verbose=verbose)
    assert logging.getLogger("telethon").getEffectiveLevel() >= logging.WARNING
    assert logging.getLogger("asyncio").getEffectiveLevel() >= logging.WARNING


def test_verbose_affects_only_our_own_logger():
    configure_logging(verbose=True)
    assert logging.getLogger("telegram_exporter").level == logging.DEBUG
    configure_logging(verbose=False)
    assert logging.getLogger("telegram_exporter").level == logging.INFO


def test_there_is_no_flag_that_unmutes_telethon():
    options = [action.option_strings for action in build_parser()._actions]
    flat = [opt for group in options for opt in group]
    assert not any("telethon" in opt or "debug" in opt for opt in flat)


def test_the_2fa_password_is_reachable_only_through_getpass():
    """No flag, no env var, no file - it is needed once per session lifetime, and
    prompting costs nothing."""
    joined = "\n".join(p.read_text() for p in SOURCES)
    assert joined.count("getpass(") == 1
    for forbidden in ["TG_PASSWORD", "TG_2FA", "TG_PASS", "--password", "--2fa"]:
        assert forbidden not in joined


def test_no_credential_is_read_from_anywhere_but_the_documented_sources():
    joined = "\n".join(p.read_text() for p in SOURCES)
    env_reads = {"TG_API_ID", "TG_API_HASH", "TG_PHONE", "XDG_STATE_HOME"}
    found = {token for token in env_reads if token in joined}
    assert found == env_reads                      # and nothing else
    # The app does not read .env; the shell already handles that.
    assert "dotenv" not in joined


def test_no_credential_appears_in_the_captured_log_output(caplog):
    configure_logging(verbose=True)
    log = logging.getLogger("telegram_exporter.test")
    with caplog.at_level(logging.DEBUG):
        log.debug("group %r (id %s)", "My Group", -1001234567890)
    assert "api_hash" not in caplog.text
    assert "+1555" not in caplog.text
