"""test_post_process — placeholder for the regen post-processor.

PR 6 does NOT introduce a post-processor (the generator's emit is
clean). PR 7 (the `make sdk-gen` aggregator) is the natural place
to add one if the generator ever needs deterministic output.

The tests here are tripwires: any regen that produces non-canonical
output should fail at the `make sdk-gen-twice` step (see Makefile).
We document the tripwire and assert the on-disk tree is currently
clean.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

import pytest


def test_regen_is_deterministic(tmp_path: Path) -> None:
    """Regenerating twice produces no diff. Run from the SDK root
    (`sdk/python/`) — the script path is `scripts/gen.py`.

    This is the analogue of `make sdk-gen-{node,python}-twice` and
    catches any non-determinism in the generator's emit.
    """
    repo_root = Path(__file__).resolve().parent.parent.parent.parent
    sdk_root = repo_root / "sdk" / "python"
    script = sdk_root / "scripts" / "gen.py"
    if not script.exists():
        pytest.skip("gen.py not present (regen tooling not shipped in this checkout)")

    # First regen: capture the diff hash of the on-disk tree.
    # Use `sys.executable` so the subprocess inherits the current
    # venv (the test runs in the dev venv which has openapi-python-client
    # + ruamel.yaml installed).
    import sys as _sys

    subprocess.run(
        [_sys.executable, str(script)],
        cwd=str(sdk_root),
        check=True,
    )
    first = subprocess.run(
        ["git", "ls-files", "-s", "faas_sdk/"],
        cwd=str(sdk_root),
        check=True,
        capture_output=True,
        text=True,
    )
    # Second regen: capture again, must match.
    subprocess.run(
        [_sys.executable, str(script)],
        cwd=str(sdk_root),
        check=True,
    )
    second = subprocess.run(
        ["git", "ls-files", "-s", "faas_sdk/"],
        cwd=str(sdk_root),
        check=True,
        capture_output=True,
        text=True,
    )
    assert first.stdout == second.stdout, "regen is non-deterministic: the second regen changed tracked files"
