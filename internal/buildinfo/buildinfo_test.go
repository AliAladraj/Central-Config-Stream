package buildinfo

import (
	"strings"
	"testing"
)

// The defaults are load-bearing rather than decorative: an unstamped
// `go build` has to print something a human can read, and an empty field would
// look like a stamping step that had broken rather than like a laptop build.
func TestDefaultsAreNamed(t *testing.T) {
	for _, tc := range []struct{ field, got, want string }{
		{"Version", Version, "dev"},
		{"Commit", Commit, "none"},
		{"Date", Date, "unknown"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (this test runs unstamped)", tc.field, tc.got, tc.want)
		}
	}
}

// String is the whole of what `--version` prints, so a field that reaches the
// linker but not the output is invisible without this.
func TestStringCarriesEveryField(t *testing.T) {
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-08-06T00:00:00Z"
	t.Cleanup(func() { Version, Commit, Date = "dev", "none", "unknown" })

	got := String()
	for _, want := range []string{"v1.2.3", "abc1234", "2026-08-06T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}
