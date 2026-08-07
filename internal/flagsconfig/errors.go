// Package flagsconfig owns feature flags: the flag definitions themselves and
// their per-environment values. Unlike appsettings and localization, a flag
// value is environment-wide rather than per-service, so every consumer in an
// environment watches every flag in it.
//
// The package is the fullest worked example of the shape all three config
// domains share — model, sentinel errors, a Postgres repository and its SQLite
// mirror, a service that validates and publishes write-through to KV, and a
// handler that maps each sentinel to a status. CONTRIBUTING.md points here as
// the pattern to copy.
//
// Flag keys are validated on the way in because they become part of a KV key
// ("{envID}.{flagKey}"): a key containing a dot would be ambiguous to a
// consumer parsing it, and would be republished by every sweep thereafter.
package flagsconfig

import "errors"

var ErrFlagNotFound = errors.New("flag not found")
var ErrInvalidFlagID = errors.New("invalid flag id")
var ErrInvalidEnvironmentID = errors.New("invalid environment id")

// ErrInvalidFlagKey rejects a key that cannot be part of a KV key. The FLAGS
// bucket key is "{environmentID}.{flagKey}", so a key containing a dot or a
// space would either be ambiguous or refused by JetStream at publish time.
var ErrInvalidFlagKey = errors.New("invalid flag key")

// ErrInvalidFlagValue rejects an empty value. VALUE is NOT NULL in
// migrations/005, but PostgreSQL keeps an empty string and NULL apart, so the
// constraint closes only half the hole and this check is the other half — the
// migration says so in as many words. It matters because the value is
// republished to every consumer in the environment and read as a rollout
// percentage, a variant name or a threshold: "" arrives at the far end
// indistinguishable from a parse bug, so a caller who means "no value" has to
// write a real placeholder.
var ErrInvalidFlagValue = errors.New("flag value must not be empty")

// ErrFlagValueTooLarge rejects a value longer than the VALUE column. Left to
// the column it arrives as an opaque driver error, which writeErr can only map
// to a 500 — a server fault for what is ordinary caller input.
var ErrFlagValueTooLarge = errors.New("flag value exceeds the maximum length")

// ENABLED and IS_ACTIVE are SMALLINT columns carrying a boolean the API spells
// as 1/0 (migrations/004 explains why the column is not BOOLEAN). The int/bool
// split is the contract, not a range: only the KV payload is a real boolean, and
// it collapses everything non-zero to true, so a stored 7 is a row the API reads
// back as 7 while consumers see the same true a 1 would have given them. The
// bound is the meaning rather than the column — 2 fits a SMALLINT and says
// nothing — and it keeps an out-of-range SMALLINT from surfacing as a 500.
var ErrInvalidEnabled = errors.New("enabled must be 0 or 1")
var ErrInvalidIsActive = errors.New("isActive must be 0 or 1")

// Creation collides with the natural unique keys: FLAG_KEY on CONFIG_FLAG, and
// (ENVIRONMENT_ID, FLAG_ID) on CONFIG_FLAG_VALUE. Distinct so a handler can map
// them to HTTP 409.
var ErrFlagExists = errors.New("flag with this key already exists")
var ErrFlagValueExists = errors.New("flag value for this environment already exists")

// ErrEnvironmentNotFound guards a flag value against pointing at an environment
// that does not exist — the referential integrity check that keeps a foreign-key
// violation from surfacing as a 500.
var ErrEnvironmentNotFound = errors.New("environment not found")

// ErrConflict is returned when an update carried an expected UPDATED_AT that no
// longer matches the stored row: somebody else edited it first. Distinct so a
// handler can map it to HTTP 409.
var ErrConflict = errors.New("flag value was modified by someone else")
