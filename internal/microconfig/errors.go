package microconfig

import "errors"

var ErrConfigNotFound = errors.New("config not found")
var ErrInvalidConfigID = errors.New("invalid config id")
var ErrInvalidMicroserviceID = errors.New("invalid microservice id")
var ErrInvalidEnvironmentID = errors.New("invalid environment id")
var ErrInvalidSettingsJSON = errors.New("invalid settings json")

// ErrSettingsTooLarge rejects a tree bigger than a KV value may be. Accepting
// it would return a 201 for a row that can never be published, so the write
// would look successful while consumers never saw it.
var ErrSettingsTooLarge = errors.New("settings json exceeds the maximum value size")
var ErrMicroserviceNotFound = errors.New("microservice not found")
var ErrEnvironmentNotFound = errors.New("environment not found")

// Names are the natural keys of the two reference tables and they end up in log
// lines and admin UIs, so they are bounded and non-empty.
var ErrInvalidMicroserviceName = errors.New("invalid microservice name")
var ErrInvalidEnvironmentName = errors.New("invalid environment name")

// Creation collides with the natural unique keys: MICROSERVICE, ENVIRONMENT,
// and (MICROSERVICE_ID, ENVIRONMENT_ID) on the appsettings table. Distinct so a
// handler can map them to HTTP 409.
var ErrMicroserviceExists = errors.New("microservice with this name already exists")
var ErrEnvironmentExists = errors.New("environment with this name already exists")
var ErrConfigExists = errors.New("appsettings for this microservice and environment already exist")

// Deleting a reference row that something still points at would orphan the
// pointing rows, so it is refused rather than cascaded: an environment or a
// microservice is not the kind of thing to remove by accident.
var ErrMicroserviceInUse = errors.New("microservice still has appsettings or localization rows")
var ErrEnvironmentInUse = errors.New("environment still has flag, appsettings or localization rows")

// ErrConflict is returned when an update carried an expected UPDATED_AT that no
// longer matches the stored row: somebody else edited it first. Distinct so a
// handler can map it to HTTP 409.
var ErrConflict = errors.New("config was modified by someone else")
