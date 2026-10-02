---
name: release
description: Release a new version of slack-acp. Confirms build/tests pass, updates VERSION and CHANGELOG.md, commits, tags, and pushes.
---

# Release Skill

Release a new version of `slack-acp`.

## Version determination

If the user provides a version, use it. Otherwise auto-determine:

1. Read the current version from `VERSION`.
2. Look at entries under `## [Unreleased]` in `CHANGELOG.md`.
3. If there are `### Added` or `### Removed` entries → **minor** bump
   (e.g. 0.1.0 → 0.2.0).
4. If there are only `### Fixed` or `### Changed` entries → **patch**
   bump (e.g. 0.1.0 → 0.1.1).
5. If the section is empty → ask the user whether to proceed or abort.

## Steps

1. **Full build & test** — execute `make all` (default target) and
   confirm everything passes: vet, test-race, 100% coverage gate (via
   `covgate`), all 5 cross-builds, native build, license check.
2. **Check CHANGELOG** — confirm there are entries under
   `## [Unreleased]`. If empty, ask the user.
3. **Determine version** — follow the rules above if the user didn't
   specify. State the version and proceed.
4. **Update CHANGELOG** — rename `## [Unreleased]` to
   `## [VERSION] - YYYY-MM-DD` (today's date) and add a fresh empty
   `## [Unreleased]` section above it. Reverse-chronological order.
5. **Update VERSION** — write the new version (single line, trailing
   newline).
6. **Commit** — check `git status` first. Stage all uncommitted
   changes with `git add -A`, then `git commit -m "release: vVERSION"`.
7. **Tag** — `git tag -a vVERSION -m "release: vVERSION"` (pass `-m`
   to avoid opening an editor).
8. **Install** — `make install` to put the new binary in `$GOBIN`.
9. **Verify** — `slack-acp --version` prints the new version.

## Important notes

- **Uncommitted changes**: always check `git status` before committing.
  All release-related and pending changes should be in the release
  commit.
- **Avoid interactive git**: pass `-m` to `git tag -a` and `git commit`.
  Git may try to open vim/nano, which fails non-interactively.
- **Moving tags**: if you need to move a tag after an extra commit,
  `git tag -d vVERSION` then re-create.
- **No PGO here.** Unlike fir, slack-acp does not use PGO, so
  `make publish` is a straight push — no amend dance.

## Publishing

After the user confirms, run `make publish`. This pushes `main` and
`vVERSION` to `origin`. The GitHub `release.yml` workflow then:

1. Runs `make all` (vet, race, coverage gate, cross-builds, license
   check).
2. Invokes GoReleaser: builds the 5 cross-compile targets, creates the
   GitHub release with binaries + checksums + `THIRD_PARTY_NOTICES.md`,
   and commits `Formula/slack-acp.rb` to `kfet/homebrew-ai` (the
   shared tap).

After which `brew install kfet/ai/slack-acp` (or `brew upgrade`) will
pick up the new version.

Fleet hosts are then moved by `scripts/converge.sh --tot` (commit
`dist.lock`) and `scripts/converge.sh <bot> --apply` — see the deploy
skill. There is no scp/`make deploy` path: every host runs a published,
checksum-verified release.

If any step fails, stop and report. Do not push or publish unless the
user confirms.

## Post-publish: track GitHub Actions

After `make publish` succeeds, poll GitHub Actions until every
triggered workflow for the release commit finishes:

```bash
gh run list --limit 10 --json status,conclusion,name,headSha,createdAt,databaseId 2>&1
```

This must **not** use `--branch` filtering — tag-triggered workflows
(`release`) do not appear under a branch filter. Match runs by
`headSha` against the release commit SHA.

## Finish on the FLEET, not on one host

**A deploy is done when the fleet is converged, not when a host is.**
This repo is public: it holds the distro spec (`distro.json`, the defaults
every slack-acp bot shares) and the resolution (`dist.lock`), never bot
instances. Each bot is one JSON file in the private fleet registry —
`$FLEET_BOTS_DIR`, default `~/sync/shared/fleet/bots` — with
`"relay": "slack-acp"` and only the fields that differ from `distro.json`.
That registry is shared by every relay (poe-acp, slack-acp, zulip-acp).

```bash
scripts/converge.sh --tot            # resolve the newest release into dist.lock; commit it
scripts/converge.sh <bot>            # dry run: binary, config.json, unit, running image
scripts/converge.sh <bot> --apply    # make the host match, restart, verify
```

Then close with the read-only sweep (runs from any fleet host):

```bash
~/sync/shared/fleet/fleet.sh status
```

It reports every relay instance on every host with wanted vs **running**
version (read from the live process, never the on-disk binary) and drift.
Do not say "released" or "deployed" until every slack-acp row is `ok`.
Paste the output into your reply.

### Two traps this sweep exists to catch

- **Never infer presence from a binary or a glob.** Under zsh,
  `ls ~/.local/bin/*-acp` **aborts the whole command** when it matches
  nothing — and the empty output reads as "not installed". Ask the supervisor:
  `systemctl --user list-units --type=service --all --no-legend --plain | awk '$1 ~ /acp/'`
  or `launchctl list | awk '$3 ~ /acp/'`. Match the unit **name**, not the
  Description.
- **A repo can have two live clones and you will release from the stale one.**
  `git fetch origin`, then `git status -sb` and read *ahead/behind*, before you
  trust any clone. Release from one canonical clone only. (A relay release once
  cut from a stale clone produced two different builds of the same tag.)
