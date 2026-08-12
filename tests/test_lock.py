"""Single-instance lock under real process contention.

flock is tested with a second process because that is the only thing that
exercises it: two acquisitions inside one process would succeed on most
platforms and prove nothing.
"""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest

from telegram_exporter.cli import exclusive
from telegram_exporter.session import Abort

REPO_SRC = str(Path(__file__).resolve().parents[1] / "src")

HOLDER = """
import sys, time
from pathlib import Path
from telegram_exporter.cli import exclusive
with exclusive(Path(sys.argv[1])):
    print("HELD", flush=True)
    time.sleep(60)
"""

CONTENDER = """
import sys
from pathlib import Path
from telegram_exporter.cli import exclusive
from telegram_exporter.session import Abort
try:
    with exclusive(Path(sys.argv[1])):
        sys.exit(0)
except Abort as a:
    print(a.reason, file=sys.stderr)
    sys.exit(a.code)
"""


def _env():
    env = dict(os.environ)
    env["PYTHONPATH"] = REPO_SRC + os.pathsep + env.get("PYTHONPATH", "")
    return env


@pytest.fixture
def holder(tmp_path):
    lock = tmp_path / ".export.lock"
    proc = subprocess.Popen([sys.executable, "-c", HOLDER, str(lock)],
                            stdout=subprocess.PIPE, text=True, env=_env())
    assert proc.stdout.readline().strip() == "HELD", "holder failed to acquire"
    try:
        yield lock
    finally:
        proc.kill()
        proc.wait(timeout=10)


def test_a_second_acquire_exits_three(holder):
    result = subprocess.run([sys.executable, "-c", CONTENDER, str(holder)],
                            capture_output=True, text=True, env=_env(), timeout=30)
    assert result.returncode == 3
    assert "busy" in result.stderr


def test_contention_names_the_holder(holder):
    with pytest.raises(Abort) as excinfo:
        with exclusive(holder):
            pass
    assert excinfo.value.code == 3
    for field in ("pid=", "host=", "started="):
        assert field in excinfo.value.reason


def test_contention_touches_no_file(holder, tmp_path):
    before = sorted(p.name for p in tmp_path.iterdir())
    with pytest.raises(Abort):
        with exclusive(holder):
            pass
    assert sorted(p.name for p in tmp_path.iterdir()) == before


def test_the_lock_is_released_when_the_holder_dies(tmp_path):
    lock = tmp_path / ".export.lock"
    proc = subprocess.Popen([sys.executable, "-c", HOLDER, str(lock)],
                            stdout=subprocess.PIPE, text=True, env=_env())
    assert proc.stdout.readline().strip() == "HELD"
    proc.kill()                      # kill -9: flock releases on process death,
    proc.wait(timeout=10)            # so there is no stale-lock cleanup to get wrong
    with exclusive(lock):
        pass


def test_different_export_roots_do_not_contend(holder, tmp_path):
    # Exporting two different groups concurrently is legitimate, so the lock is
    # per-root rather than global.
    with exclusive(tmp_path / "other" / ".export.lock"):
        pass
