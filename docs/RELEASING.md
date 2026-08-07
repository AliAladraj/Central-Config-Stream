# Releasing

Cutting a release is pushing a `v*` tag. Everything after that is
[`.github/workflows/release.yml`](../.github/workflows/release.yml), which
re-runs build, vet and test before it publishes anything, then produces a GitHub
release with binaries and checksums and pushes a container image to GHCR.

The part that is not automated is the part that needs judgement: deciding the
version number and finishing the changelog entry. Do those first, in a normal
pull request, and the tag becomes a one-line operation on a commit that is
already reviewed and already green.

---

## 1. Before the tag

**Pick the version.** [Semantic Versioning](https://semver.org/spec/v2.0.0.html),
read against the two contracts this project actually has: the admin API
(`docs/openapi.yaml`) and the KV value shapes consumers deserialize
(`docs/CONSUMER_CONTRACT.md`). A change that alters either of those is breaking
for somebody who never deployed anything — a consumer watching a bucket does not
get to review your release. While the version is `0.y.z`, a `0.y` bump is where
a breaking change goes; SemVer promises nothing across 0.x, which is also why
the image gets no bare `0` tag.

**Finish the changelog entry.** In [`CHANGELOG.md`](../CHANGELOG.md), the
section being released still reads:

```
## [0.1.0] — unreleased
```

Replace the suffix with the release date, in ISO form and in the em-dashed shape
the rest of the file uses:

```
## [0.1.0] — 2026-08-12
```

The workflow matches on `## [0.1.0]` and stops at the next `##`, and it skips
the heading line itself — so the suffix reaches neither the extraction nor the
release page, and leaving it in place breaks nothing. It stays wrong in
`CHANGELOG.md`, though, which is the copy people read in the repository and in
every fork of it, permanently announcing a shipped version as unreleased.

`## [Unreleased]` above it is deliberately empty and stays that way, so the next
change has somewhere obvious to land. The two link definitions at the foot of
the file resolve for the first time at this point too; until a tag exists,
neither a `compare/v0.1.0...HEAD` nor a `releases/tag/v0.1.0` URL has anything
to point at.

**Merge that, and check CI is green on the commit you are about to tag.** The
workflow re-runs the gate anyway, but finding out here costs a push and finding
out there costs a version number.

---

## 2. The tag

From the merged commit on `main`:

```bash
git checkout main && git pull
git tag -a v0.1.0 -m "central-config v0.1.0"
git push origin v0.1.0
```

Annotated (`-a`), not lightweight. An annotated tag is a real object in the
repository carrying a tagger, a date and a message of its own, so the record of
who cut a release and when survives in the history rather than only in whatever
GitHub displays. Note that this is not what `--tags` is about: `git describe
--tags` — which is where the version stamped into every binary comes from — is
precisely the flag that tells `describe` to consider *lightweight* tags too, so
a lightweight tag would still produce the right version string. The reason to
annotate is the metadata, not the describe.

The tag name is the version with a `v`. That prefix is load-bearing in three
places: it is the workflow's trigger pattern, it is what `git describe` returns
and therefore what `./central-config --version` prints, and the workflow strips
it to find the changelog heading and to build the image's semver tags.

---

## 3. What the tag then does

Three jobs. The first is a gate and the other two only start if it passes.

| job | what it produces |
|---|---|
| **gate** | nothing — `go build`, `go vet`, `go test -race` on the tagged tree, against the same `postgres:17-alpine` service container CI uses, with the same check that `internal/pgintegration` ran rather than skipped |
| **binaries and release** | the GitHub release: `central-config` and `testconsole` for linux and darwin × amd64 and arm64, one `.tar.gz` per binary per platform with `LICENSE` and `README.md` inside, plus `checksums.txt`. Release notes are this tag's `CHANGELOG.md` section, nothing generated |
| **image to GHCR** | `ghcr.io/alialadraj/central-config`, tagged `latest`, `0.1.0`, `0.1` and `sha-<short>` |

Everything is stamped from the same three values the `Makefile` computes, via
the same `-ldflags -X` into `internal/buildinfo`, so an image and a downloaded
binary of one release report the same version and the same commit. The build
timestamp is the one field that will differ: the two jobs run in parallel and
each computes its own `date -u`, so expect them to disagree by seconds. Compare
the version and the commit; the timestamp records when that artefact was built,
not which release it belongs to.

Two things the release does **not** contain, both on purpose:

- **The console's React bundle.** `web/` is gitignored and building it needs
  Node, so `testconsole` ships as a bare binary. It starts, serves `/api/state`
  and `/api/events`, and answers the browser with a page saying the UI has not
  been built. The Dockerfile's `testconsole` stage *does* build and copy the
  bundle, but that stage is a local harness and is not pushed — the GHCR image
  is the `central-config` stage, the service alone.

---

## 4. Checking it landed

```bash
# the release exists and carries eight archives and a checksums file
gh release view v0.1.0

# the image is there and knows what it is
docker run --rm ghcr.io/alialadraj/central-config:0.1.0 --version
# central-config v0.1.0 (commit 1a2b3c4, built 2026-08-12T09:14:02Z)

# and the labels agree with it: title central-config, version v0.1.0 and
# revision the same short commit the line above printed
docker inspect --format '{{json .Config.Labels}}' \
  ghcr.io/alialadraj/central-config:0.1.0

# a downloaded binary says the same thing, and matches its checksum
shasum -a 256 -c checksums.txt --ignore-missing
tar xzf central-config_0.1.0_linux_amd64.tar.gz && ./central-config --version
```

`--version` is answered before any configuration is read and before the database
or NATS is dialled, so none of the above needs a running stack.

**The first release only:** GHCR creates the package private, and it inherits no
visibility from the repository. Until it is switched to public in the package
settings, `docker pull` asks for credentials — which looks exactly like the push
having failed. Check that before debugging anything else.

---

## 5. After the tag lands

The workflow publishes artefacts; it does not edit prose. Three places in the
repository make claims that only a tag can settle, and every one of them is
still wrong the moment §4 says the release is real:

- **[`README.md`](../README.md) § *Install*** — whether a `ghcr.io` pull and a
  release-page download work at all, and therefore whether the from-source
  paths are the only ones on offer.
- **[`SECURITY.md`](../SECURITY.md) § *Supported versions*** — what the
  supported line is. Before a release it can only be `main`; after one it is
  the latest release plus `main`, which is a different answer to the only
  question that section is asked.
- **[`CHANGELOG.md`](../CHANGELOG.md)'s link definitions** — the `[Unreleased]`
  compare has to move to the tag just cut, and the released section needs its
  own line: a `compare/v0.1.0...v0.1.1` for a patch, a
  `releases/tag/v0.1.0` for the first one. Any caveat above them about the URLs
  not resolving yet goes with them.

Open that as a pull request off `main` in the same sitting as the tag. It cannot
be done before, because until the tag exists the old text is the true text.

This is a numbered step rather than a footnote because both of the first two
shipped stale through v0.1.0 *and* v0.1.1, still announcing that nothing was
tagged and that a pull 404s, and it was an external reader who noticed rather
than anything in this process. A claim about whether a release exists is
precisely the claim that cutting a release invalidates, and nothing else here
reads those files.

---

## 6. When a release is wrong

A version number is cheap and a moving one is not. Deleting the GitHub release
and the tag is easy; retracting the image is not — GHCR keeps the digest,
`latest` has already moved, and anyone who pulled in the meantime has it. Go
forwards instead: fix it, add a `## [0.1.1]` section, tag `v0.1.1`.

The exception is a release that never completed — the gate failed, or the
GoReleaser job died part-way. Nothing was published under a name anyone can
have pulled, so deleting the tag locally and on the remote and re-pushing it
after the fix is fine:

```bash
git tag -d v0.1.0 && git push origin :refs/tags/v0.1.0
```

If the failure was `CHANGELOG.md has no '## [0.1.0]' section`, that is §1 of
this document catching a tag pushed ahead of its changelog entry, which is the
one release mistake that is otherwise completely silent.
