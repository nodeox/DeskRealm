"""Check a fresh public source checkout and reject local source drift."""

import json
from pathlib import Path
import runpy
import subprocess
from tempfile import TemporaryDirectory


ROOT = Path(__file__).resolve().parents[2]
TOOL = ROOT / "scripts/deskrealm-managed-source"


def command(*args, expected_error=None):
    result = subprocess.run([TOOL, *args], text=True, capture_output=expected_error is not None)
    if expected_error is None:
        result.check_returncode()
    elif result.returncode == 0 or expected_error not in result.stderr:
        raise AssertionError(f"expected {expected_error!r}: {result.stdout}{result.stderr}")


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args], text=True).rstrip("\n")


def next_commit(repo):
    return git(repo, "-c", "user.name=DeskRealm Fixture", "-c", "user.email=fixture@example.test",
               "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "source gate fixture")


def main():
    lock = json.loads((ROOT / "managed/upstream.lock").read_text())["repositories"]
    with TemporaryDirectory(prefix="deskrealm-m2-source-") as temp:
        tmp = Path(temp)
        source = tmp / "source"
        command("prepare", source)
        command("verify", source)
        command("prepare", source, expected_error="destination must not already exist")
        client = source / "client"
        tag = "refs/tags/" + lock["client"]["tag"]
        git(client, "update-ref", tag, next_commit(client))
        command("verify", source, expected_error="release tag moved")
        git(client, "update-ref", tag, lock["client"]["commit"])
        child = client / "libs/hbb_common"
        child_pin = lock["client"]["submodules"]["libs/hbb_common"]["commit"]
        git(child, "update-ref", "HEAD", next_commit(child))
        command("verify", source, expected_error="HEAD differs from lock")
        git(child, "update-ref", "HEAD", child_pin)
        command("verify", source)

        # Build an ordinary patch from the fixed source and replay it against
        # the restored checkout without changing the upstream commits.
        apply_patches = runpy.run_path(str(TOOL))["apply_patches"]
        patch_root = tmp / "patches"
        (patch_root / "root").mkdir(parents=True)
        readme = client / "README.md"
        prior = readme.read_bytes()
        readme.write_bytes(prior + b"\nDeskRealm source-gate fixture.\n")
        patch = git(client, "diff", "--", "README.md")
        (patch_root / "root/0001-fixture.patch").write_text(patch + "\n")
        readme.write_bytes(prior)
        command("verify", source)
        apply_patches(client, lock["client"], patch_root)
        assert readme.read_bytes().endswith(b"DeskRealm source-gate fixture.\n")
        command("verify", source, expected_error="local changes")
        print("Source checks passed: locked checkout, moved tag, wrong submodule, patch replay, dirty tree.")


if __name__ == "__main__":
    main()
