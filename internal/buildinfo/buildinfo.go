// Package buildinfo carries the identity of the binary: which version, which
// commit it was built from, and when. The values are stamped by the linker at
// build time (`-ldflags -X`) by the Makefile and the Dockerfile; nothing in the
// service ever assigns them, and nothing reads them on a request path.
//
// The defaults are words rather than empty strings because an unstamped build
// is a normal thing to have — `go build ./cmd/central-config` on a laptop is
// one — and "dev"/"none"/"unknown" say so plainly. An empty version, by
// contrast, is indistinguishable from a stamping step that has quietly stopped
// working, which is exactly the case where you most want the binary to be
// honest about what it is.
package buildinfo

import "fmt"

// The three stamped fields. They are vars rather than consts precisely so the
// linker can rewrite them: `-X <module>/internal/buildinfo.Version=v1.2.3`.
var (
	// Version is `git describe --tags --always --dirty`: the nearest tag, how
	// far past it this build is, and whether the working tree was clean. With
	// no tags in the repository it degrades to the short commit hash, which is
	// still more than nothing.
	Version = "dev"

	// Commit is the short commit hash — the identifier SECURITY.md asks a
	// vulnerability report to quote.
	Commit = "none"

	// Date is the build time, UTC and RFC 3339, so two builds of the same
	// commit can still be told apart.
	Date = "unknown"
)

// String renders the three fields as one line, in the order that matters when
// you are looking at a process and wondering what it is: what it claims to be,
// what it was actually built from, and how old that is.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
