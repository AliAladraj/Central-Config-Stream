<!--
CONTRIBUTING.md asks for one thing above all: be honest about what you did and
did not verify. Nothing below is a formality — an unticked box with a sentence
next to it is a better pull request than a ticked one that was not run.
-->

## What this changes, and why

<!-- The failure it closes or the capability it adds. The "why" is often the
     only record of what the code is defending against. -->

## What you verified

- [ ] `gofmt -l .` prints nothing, and `go build ./...`, `go vet ./...`, `go test ./...` and `go test -race ./...` all pass (`make lint` and `make test` run these)
- [ ] `cd webui && npm ci && npm run lint && npm test && npm run build` — only if the console changed
- [ ] There is a test for every behavioural change; a rename or a comment needs none
- [ ] `make test-postgres` passes, if you touched a `repository.go`, a migration, or how a value is bound or compared — CI runs it too, and fails the step if it skipped
- [ ] `migrations/` and `internal/database/sqlite.go` moved together, if the schema changed — a missing table is caught by the Postgres suite, a column that merely differs is not, and that divergence is a green build against a schema production does not have
- [ ] The documentation moved with the behaviour: a variable into `.env.example`, a metric into `docs/OBSERVABILITY.md`, a change in what a token reaches into `docs/SECURITY.md`, a change in what consumers observe into `docs/CONSUMER_CONTRACT.md`

## What you could not verify

<!-- Say it plainly. If you changed Postgres-only SQL without running
     `make test-postgres`, say so rather than letting the pull request imply you
     watched it pass. And none of this has run in a production deployment yet
     (docs/PRODUCTION_READINESS.md §3.1), so a green integration suite is
     evidence and not a guarantee. That is an acceptable state to be in and a
     bad one to leave implicit. Same for anything you could not reproduce
     locally. -->

<!-- Adding a config domain? CONTRIBUTING.md's "Adding a config domain" is the
     ~45 sites it touches, with the silent ones marked. The end of that section
     also has the manual check people skip: write with NATS stopped, start it
     again, and confirm the reconciler republishes without a restart. That one
     catches a missed byName case, a missed reconciler registration and a missed
     Ensure swap — none of which fail to compile. -->
