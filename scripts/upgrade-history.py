#!/usr/bin/env python3
"""Generate the last three changed, tagged database updates (no network access)."""
import argparse
import json
import re
import subprocess
from pathlib import Path

BASE_TAG = "v0.3.1"
KEEP = 3
SCHEMA_PATH = "internal/platform/database.go"
HISTORY_PATH = "internal/platform/upgrade_history.json"
TAG_RE = re.compile(r"^v0\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$")
SCHEMA_RE = re.compile(r"^const DatabaseVersion = (\d+)\s*$", re.MULTILINE)


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True, stderr=subprocess.PIPE).strip()


def schema(source):
    match = SCHEMA_RE.search(source)
    if not match:
        raise ValueError("DatabaseVersion is missing or not an integer constant")
    return int(match[1])


def retain_updates(base, tagged_schemas, current):
    """Several development schemas under one tag occupy just one slot."""
    previous = base
    updates = []
    for tag, version in tagged_schemas:
        if version < previous:
            raise ValueError(f"database schema decreases at {tag}: {previous} -> {version}")
        if version == previous:
            continue
        updates.append(dict(tag=tag, **{"from": previous, "to": version}))
        previous = version
    if current < previous:
        raise ValueError("working schema is older than the latest tagged schema")
    retained = updates[-KEEP:]
    return dict(base_schema=retained[0]["from"] if retained else base, updates=retained)


def generate(root, release_tag=None):
    if git(root, "rev-parse", "--is-shallow-repository") != "false":
        raise ValueError("full Git history and tags required; fetch with --unshallow --tags")
    baseline = git(root, "rev-parse", f"{BASE_TAG}^{{commit}}")
    # Do not accept unrelated release histories or tags on a different branch.
    git(root, "merge-base", "--is-ancestor", baseline, "HEAD")
    current = schema((root / SCHEMA_PATH).read_text())
    if release_tag:
        if not TAG_RE.fullmatch(release_tag):
            raise ValueError(f"unsupported release tag: {release_tag}")
        if git(root, "rev-parse", f"{release_tag}^{{commit}}") != git(root, "rev-parse", "HEAD"):
            raise ValueError("release tag must point to HEAD")
        if schema(git(root, "show", f"{release_tag}:{SCHEMA_PATH}")) != current:
            raise ValueError("release schema differs from its tag")
    base = schema(git(root, "show", f"{BASE_TAG}:{SCHEMA_PATH}"))
    by_commit = {}
    for tag in git(root, "tag", "--merged", "HEAD", "--sort=version:refname").splitlines():
        if not TAG_RE.fullmatch(tag):
            continue
        commit = git(root, "rev-parse", f"{tag}^{{commit}}")
        ancestor = subprocess.run(["git", "-C", str(root), "merge-base", "--is-ancestor", baseline, commit], check=False)
        if ancestor.returncode == 1:
            continue
        if ancestor.returncode != 0:
            raise ValueError(f"cannot establish ancestry for {tag}")
        by_commit.setdefault(commit, []).append(tag)
    tagged = []
    # Commit ancestry orders the updates, not tag creation time. Multiple tags
    # on a commit (or unchanged schemas on later commits) add no extra update.
    for commit in git(root, "rev-list", "--topo-order", "--reverse", f"{baseline}..HEAD").splitlines():
        if commit not in by_commit:
            continue
        value = schema(git(root, "show", f"{commit}:{SCHEMA_PATH}"))
        tagged.extend((tag, value) for tag in by_commit[commit])
    return retain_updates(base, tagged, current)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="store_true", help="write the embedded history")
    parser.add_argument("--release-tag", help="validate that the release being built is a real tag on HEAD")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    try:
        history = generate(root, args.release_tag)
    except (ValueError, subprocess.CalledProcessError) as exc:
        parser.exit(1, f"upgrade history generation failed: {exc}\n")
    text = json.dumps(history, indent=2) + "\n"
    if args.write:
        (root / HISTORY_PATH).write_text(text)
    print(text, end="")


if __name__ == "__main__":
    main()
