"""Concurrency 1, enforced by an AST scan rather than by discipline.

Telethon does not parallelize transfers, so parallelism buys nothing and costs
flood-wait escalation: waits climb from ~7 s to 4 h+ once Telegram decides an
account is abusive. The temptation arrives precisely when an export feels slow,
which is why this is a test and not a comment.

There is deliberately no --workers flag for this test to have to police.
"""

from __future__ import annotations

import ast
from pathlib import Path

import pytest

SRC = Path(__file__).resolve().parents[1] / "src" / "telegram_exporter"

FORBIDDEN = {
    "gather", "create_task", "ensure_future", "as_completed", "wait_for_all",
    "to_thread", "run_in_executor", "TaskGroup",
    "ThreadPoolExecutor", "ProcessPoolExecutor", "Thread", "Process", "Pool",
}


def _called_names(tree: ast.AST):
    for node in ast.walk(tree):
        if isinstance(node, ast.Call):
            func = node.func
            if isinstance(func, ast.Attribute):
                yield func.attr, node.lineno
            elif isinstance(func, ast.Name):
                yield func.id, node.lineno
        elif isinstance(node, ast.ImportFrom):
            for alias in node.names:
                yield alias.name, node.lineno
        elif isinstance(node, ast.AsyncWith):
            pass


@pytest.mark.parametrize("path", sorted(SRC.glob("*.py")), ids=lambda p: p.name)
def test_no_parallelism_primitive_is_used(path):
    tree = ast.parse(path.read_text(), filename=str(path))
    hits = [(name, line) for name, line in _called_names(tree) if name in FORBIDDEN]
    assert not hits, f"{path.name} uses a parallelism primitive: {hits}"


def test_the_scan_would_actually_catch_something():
    # A guard that cannot fail is worse than no guard: it makes passing the
    # default. This is the same shape of mistake the album-adjacency test had.
    tree = ast.parse("import asyncio\n"
                     "async def f():\n"
                     "    await asyncio.gather(g(), h())\n")
    assert any(name in FORBIDDEN for name, _ in _called_names(tree))


def test_there_is_no_workers_flag():
    from telegram_exporter.cli import build_parser
    flat = [opt for action in build_parser()._actions for opt in action.option_strings]
    assert not any("worker" in opt or "concurren" in opt or "parallel" in opt
                   for opt in flat)
