# Security policy

This is the policy for reporting a vulnerability in central-config. If you are
looking for the threat model — what the service enforces, what it does not, and
what you have to harden around it — that is [`docs/SECURITY.md`](docs/SECURITY.md).

## Why this file exists

central-config is a control plane. A single authenticated write changes the
runtime behaviour of every consuming service within milliseconds, with no deploy
and no review step in between. A defect that lets someone reach the admin API,
widen a token's environment scope, or write to a KV bucket is not a bug in a CRUD
app — it is a way to change production. That deserves a private channel rather
than a public issue.

## Supported versions

**There is no release yet — nothing is tagged, so `main` is the only supported
line.** Test against `main` and report against a commit on it.

Once a first `v*` tag exists — [`docs/RELEASING.md`](docs/RELEASING.md) is how —
the supported line becomes the latest release plus `main`, on these terms: a fix
lands on `main` and goes out in the next release. There is no backporting to an
older tag and no security branch, so the remedy is always to move forward rather
than to wait for a patch on the version you happen to be on.

Say which build you tested, because a fix is written against a commit rather
than against a version number. Both binaries answer `--version` now, from the
linker's stamp alone, before any configuration is read and before the database
or NATS is dialled — so it works on a machine with neither:

```
central-config v0.1.0 (commit 1a2b3c4, built 2026-08-12T09:14:02Z)
```

That line is the whole answer once a release exists. **Until one does, every
build says `dev (commit none, built unknown)`** — the stamp comes from the
release pipeline or from `make build`, and neither a laptop `go build` nor a
`go install` passes `-ldflags`, deliberately, rather than pretending to a
version. A plain checkout has no binary to ask at all. In any of those cases
give `git rev-parse HEAD` instead; that is the identifier a fix gets written
against anyway. [`internal/buildinfo`](internal/buildinfo) is where those three
values live.

## Reporting a vulnerability

**Use GitHub's private vulnerability reporting**, from the *Security* tab of
[the repository](https://github.com/AliAladraj/Central-Config-Stream), via
*Report a vulnerability*. It opens a private advisory visible only to the
maintainer, which is what you want here: **do not open a public issue for a
security problem.** GitHub Issues on this repository are world-readable, and an
issue describing a way to reach an admin API is a working exploit published for
anyone watching.

If the *Report a vulnerability* button is not there, private reporting has not
been enabled on the repository yet — the maintainer has to switch it on under
*Settings → Advanced Security → Private vulnerability reporting*. Until that
happens there is no private channel, so open a public issue titled only
*"Security contact needed"*, with **no detail of the vulnerability in it**, and
wait to be contacted.

A useful report has: the commit you tested, what an attacker gains, and the
smallest sequence of requests or configuration that demonstrates it. A patch is
welcome and never required.

## What to expect

One maintainer, working on this in their own time, best effort. That is the
honest position and it is worth stating rather than inventing a rota:

- **Acknowledgement within about a week.** If you have heard nothing after two,
  assume the notification was missed and follow up.
- **An assessment after that**, saying whether the report is accepted, and if so
  roughly how serious it looks and what the fix involves.
- **A fix on `main` when there is one**, with the advisory published and
  credit to you in it unless you would rather not be named.

There is no service-level agreement, no bounty, and no security team. If you
need a guaranteed response time, this is not a project that can offer one.

Please give the fix a reasonable window before disclosing publicly — 90 days is
the usual convention and is what is asked for here. If the problem is being
exploited, or the maintainer has gone quiet past the timelines above, disclose;
warning users beats waiting politely.

## Scope

**In scope** — anything in this repository that a deployment actually runs:

- `cmd/central-config` and everything under `internal/` — the admin API, the
  token and environment-scope model, the audit trail and its redaction, the rate
  limiter, the reconciler, the KV write-through path.
- `pkg/configclient` — the consumer library, including its HTTP bootstrap
  fallback.
- The shipped deployment material in `deploy/k8s/` and the `Dockerfile`, where a
  defect would mean an insecure default rather than an insecure environment.

**Out of scope:**

- **The bundled test console (`cmd/testconsole`, `webui/`).** It is a
  development tool and **not a supported deployment target**. It proxies a
  full-scope admin token with no authentication of its own, so anyone who can
  reach its port can change configuration fleet-wide. That is not a
  vulnerability; it is what the tool is. It binds to loopback, refuses a `Host`
  it was not configured to answer to, checks `Origin` and forwards only JSON to
  an allowlisted set of paths precisely because it is never meant to be exposed
  — see [`docs/SECURITY.md`](docs/SECURITY.md). A way to defeat *those* defences
  from a page in another tab, on a console bound to loopback as intended, is in
  scope and worth reporting.
- **The compose stack (`deploy/compose/`).** It runs NATS with no
  authentication, a shared dev token and plain HTTP, all on purpose and all
  documented. It demonstrates the data path; it is not a manifest shape to
  promote.
- **Documented limitations.** The non-transactional dual write, the absence of
  per-key KV access control, plain HTTP when TLS is unconfigured, and disabled
  auth when no token is set are all known, deliberate and written down in
  [`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md) and
  [`docs/SECURITY.md`](docs/SECURITY.md). A report that they exist will be
  closed; a report that one of them is worse than documented is exactly what
  this file is for.
- **Your NATS cluster and your database.** Securing those is the deployment's
  job, and `docs/SECURITY.md` says what that involves.
- Vulnerabilities in third-party dependencies, unless this repository uses them
  in a way that makes it exploitable here. Report those upstream first.

## Handling of secrets in reports

Please do not include real tokens, connection strings or `.creds` files in a
report. If a reproduction needs one, generate a throwaway. The service redacts
secret-shaped fields from its own audit trail; an advisory is not covered by
that.
