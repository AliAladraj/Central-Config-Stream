package flagsconfig

import "errors"

var ErrFlagNotFound = errors.New("flag not found")
var ErrInvalidFlagID = errors.New("invalid flag id")
var ErrInvalidEnvironmentID = errors.New("invalid environment id")

// ErrInvalidFlagKey rejects a key that cannot be part of a KV key. The FLAGS
// bucket key is "{environmentID}.{flagKey}", so a key containing a dot or a
// space would either be ambiguous or refused by JetStream at publish time.
var ErrInvalidFlagKey = errors.New("invalid flag key")

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
