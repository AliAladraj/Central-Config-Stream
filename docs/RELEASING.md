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
## [0.1.0] — UNRELEASED — tag pending
```

Replace the suffix with the release date, in ISO form and in the em-dashed shape
the rest of the file uses:

```
## [0.1.0] — 2026-08-12
```

The workflow matches on `## [0.1.0]` and stops at the next `##`, so the suffix
does not change whether the extraction works — it changes what the release page
says. A release announced as `UNRELEASED — tag pending` is the kind of detail
that makes people distrust the rest of the page.

Then leave `## [Unreleased]` in place above it with nothing under it, so the
next change has somewhere obvious to land.

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

Annotated (`-a`), not lightweight. An annotated tag carries a tagger, a date and
a message of its own, and `git describe --tags` — which is where the version
stamped into every binary comes from — reads it as a first-class object rather
than as a stray ref that happens to look like one.

The tag name is the version with a `v`. That prefix is load-bearing in three
places: it is the workflow's trigger pattern, it is what `git describe` returns
and therefore what `./central-config --version` prints, and the workflow strips
it to find the changelog heading and to build the image's semver tags.

---

## 3. What the tag then does

Three jobs. The first is a gate and the other two only start if it passes.

| job | what it produces |
|---|---|
| **gate** | nothing — `go build`, `go vet`, `go test -race` on the tagged tree |
| **binaries and release** | the GitHub release: `central-config` and `testconsole` for linux and darwin × amd64 and arm64, one `.tar.gz` per binary per platform with `LICENSE` and `README.md` inside, plus `checksums.txt`. Release notes are this tag's `CHANGELOG.md` section, nothing generated |
| **image to GHCR** | `ghcr.io/erasedkyte/central-config`, tagged `latest`, `0.1.0`, `0.1` and `sha-<short>` |

Everything is stamped from the same three values the `Makefile` computes, via
the same `-ldflags -X` into `internal/buildinfo`, so an image and a downloaded
binary of one release answer `--version` identically.

Two things the release does **not** contain, both on purpose:

- **The console's React bundle.** `web/` is gitignored and building it needs
  Node, so `testconsole` ships as a bare binary. It starts, serves `/api/state`
  and `/api/events`, and answers the browser with a page saying the UI has not
  been built. The Dockerfile's `testconsole` stage *does* build and copy the
  bundle, but that stage is a local harness and is not pushed — the GHCR image
  is the `central-config` stage, the service alone.
- **A linux/arm64 image.** The Dockerfile compiles in-image without a
  `$BUILDPLATFORM`/`$TARGETARCH` split, so an arm64 image would mean running the
  whole Go build under QEMU on every release. The arm64 *binaries* are real
  cross-compiles — `CGO_ENABLED=0` throughout, because the Oracle driver and
  SQLite are both pure Go.

---

## 4. Checking it landed

```bash
# the release exists and carries eight archives and a checksums file
gh release view v0.1.0

# the image is there and knows what it is
docker run --rm ghcr.io/erasedkyte/central-config:0.1.0 --version
# central-config v0.1.0 (commit 1a2b3c4, built 2026-08-12T09:14:02Z)

# and the labels agree with it
docker inspect --format '{{json .Config.Labels}}' \
  ghcr.io/erasedkyte/central-config:0.1.0

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

## 5. When a release is wrong

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
