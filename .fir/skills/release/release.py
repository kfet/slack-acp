#!/usr/bin/env python3
"""Automate the slack-acp release flow.

Steps (mirrors .fir/skills/release/SKILL.md):
  1. Verify clean working tree (or --allow-dirty).
  2. `make all` — full build + 100% coverage gate + cross-builds.
  3. Determine target version: --version, else auto-bump from CHANGELOG.md.
  4. Roll CHANGELOG.md: insert `## [VERSION] - YYYY-MM-DD` below `## [Unreleased]`.
  5. Write new VERSION file.
  6. `git add -A && git commit -m "release: vVERSION"`.
  7. `git tag -a vVERSION -m "release: vVERSION"`.
  8. `make install`; verify `slack-acp --version` matches.
  9. Optional: `git push origin main vVERSION` (only with --push).

Idempotency: refuses to overwrite an existing vVERSION tag.
Dry run: --dry-run skips every mutating command and prints what would run.

Stdlib only — works on macOS' built-in python3.9.
"""

import argparse
import datetime
import re
import subprocess
import sys
from pathlib import Path

# .fir/skills/release/release.py → repo root is 4 levels up.
REPO_ROOT = Path(__file__).resolve().parents[3]
VERSION_FILE = REPO_ROOT / "VERSION"
CHANGELOG = REPO_ROOT / "CHANGELOG.md"


def fail(msg: str) -> None:
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(1)


def run(cmd, *, capture=False, dry_run=False, check=True) -> str:
    """Run a subprocess. `dry_run=True` skips mutating commands."""
    label = " ".join(cmd)
    if dry_run:
        print(f"(dry-run) $ {label}")
        return ""
    print(f"$ {label}")
    res = subprocess.run(
        cmd, cwd=REPO_ROOT, check=check,
        capture_output=capture, text=True,
    )
    return (res.stdout or "").strip() if capture else ""


def current_version() -> str:
    v = VERSION_FILE.read_text().strip()
    if not re.fullmatch(r"\d+\.\d+\.\d+", v):
        fail(f"VERSION file does not contain X.Y.Z, got {v!r}")
    return v


def determine_bump() -> str:
    """Inspect [Unreleased] in CHANGELOG.md → 'minor' | 'patch' | 'empty'."""
    text = CHANGELOG.read_text()
    m = re.search(r"^## \[Unreleased\]\s*\n(.*?)(?=^## )", text, re.S | re.M)
    if not m:
        fail("Cannot find ## [Unreleased] section in CHANGELOG.md")
    body = m.group(1)
    has_bullets = bool(re.search(r"^\s*-\s+\S", body, re.M))
    if not has_bullets:
        return "empty"
    has_added = bool(re.search(r"^### Added\s*$", body, re.M)) and bool(
        re.search(r"^### Added\s*\n+\s*-", body, re.M)
    )
    has_removed = bool(re.search(r"^### Removed\s*$", body, re.M)) and bool(
        re.search(r"^### Removed\s*\n+\s*-", body, re.M)
    )
    return "minor" if (has_added or has_removed) else "patch"


def bump(version: str, kind: str) -> str:
    major, minor, patch = (int(x) for x in version.split("."))
    if kind == "minor":
        return f"{major}.{minor + 1}.0"
    if kind == "patch":
        return f"{major}.{minor}.{patch + 1}"
    fail(f"unknown bump kind: {kind}")


def roll_changelog(new_version: str, dry_run: bool) -> None:
    today = datetime.date.today().isoformat()
    text = CHANGELOG.read_text()
    if f"## [{new_version}]" in text:
        fail(f"CHANGELOG already has a section for {new_version}")
    new_text, n = re.subn(
        r"^## \[Unreleased\]\s*\n",
        f"## [Unreleased]\n\n## [{new_version}] - {today}\n",
        text, count=1, flags=re.M,
    )
    if n != 1:
        fail("Failed to splice new release section into CHANGELOG.md")
    if dry_run:
        print(f"(dry-run) would insert ## [{new_version}] - {today} into CHANGELOG.md")
    else:
        CHANGELOG.write_text(new_text)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--version", help="explicit X.Y.Z; auto-bump from CHANGELOG if omitted")
    ap.add_argument("--push", action="store_true", help="git push origin main + tag after committing")
    ap.add_argument("--dry-run", action="store_true", help="print what would run; mutate nothing")
    ap.add_argument("--allow-dirty", action="store_true",
                    help="include any uncommitted changes in the release commit")
    ap.add_argument("--skip-make", action="store_true",
                    help="skip `make all` (use only when you've just run it)")
    args = ap.parse_args()

    # 0. Clean tree gate.
    status = run(["git", "status", "--porcelain"], capture=True)
    if status and not args.allow_dirty:
        print("Working tree has uncommitted changes:")
        print(status)
        print("\nPass --allow-dirty to include them in the release commit, or commit/stash first.")
        sys.exit(1)

    # 1. make all.
    if not args.skip_make:
        run(["make", "all"], dry_run=args.dry_run)

    # 2. Determine version.
    cur = current_version()
    if args.version:
        new_version = args.version
        if not re.fullmatch(r"\d+\.\d+\.\d+", new_version):
            fail(f"--version must be X.Y.Z, got {new_version!r}")
    else:
        kind = determine_bump()
        if kind == "empty":
            fail("CHANGELOG [Unreleased] is empty — populate it or pass --version")
        new_version = bump(cur, kind)
        print(f"\n→ auto-bump: {cur} → {new_version} ({kind})")

    if new_version == cur:
        fail(f"target version equals current ({cur})")

    existing = run(["git", "tag", "-l", f"v{new_version}"], capture=True)
    if existing:
        fail(f"tag v{new_version} already exists; aborting")

    # 3. Mutate: CHANGELOG, VERSION, commit, tag, install.
    roll_changelog(new_version, args.dry_run)

    if args.dry_run:
        print(f"(dry-run) would write VERSION = {new_version}")
    else:
        VERSION_FILE.write_text(new_version + "\n")

    run(["git", "add", "-A"], dry_run=args.dry_run)
    run(["git", "commit", "-m", f"release: v{new_version}"], dry_run=args.dry_run)
    run(["git", "tag", "-a", f"v{new_version}", "-m", f"release: v{new_version}"],
        dry_run=args.dry_run)

    run(["make", "install"], dry_run=args.dry_run)

    # 4. Verify the installed binary reports the new version.
    if not args.dry_run:
        out = run(["slack-acp", "--version"], capture=True, check=False)
        if new_version not in out:
            fail(f"`slack-acp --version` returned {out!r}; expected to contain {new_version}")
        print(f"✓ installed: {out}")

    # 5. Push.
    if args.push:
        run(["git", "push", "origin", "main"], dry_run=args.dry_run)
        run(["git", "push", "origin", f"v{new_version}"], dry_run=args.dry_run)
        print(f"\n✓ released v{new_version}")
    else:
        print(f"\n✓ local release v{new_version} ready.")
        print(f"  To publish:  git push origin main v{new_version}")
        print(f"  Or re-run:   {Path(__file__).name} --push")


if __name__ == "__main__":
    main()
