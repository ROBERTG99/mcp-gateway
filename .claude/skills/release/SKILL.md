---
name: release
description: Prepare a release - changelog, version, local tag. Never push tags or publish images; the maintainer does that.
---

# Release

1. `make verify` must be green on the commit.
2. Update `CHANGELOG.md` (Keep a Changelog) with the version and date.
3. Commit `chore(release): vX.Y.Z`, then create the annotated tag locally: `git tag -a vX.Y.Z -m "vX.Y.Z"`.
4. Do not push the tag, do not run `release.yml`, do not publish images. Add "push tag vX.Y.Z" under "Needs the maintainer" in `PROGRESS.md`.
