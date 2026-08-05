package localization

import "errors"

var ErrLocalizationNotFound = errors.New("localization not found")
var ErrInvalidLocalizationID = errors.New("invalid localization id")
var ErrInvalidMicroserviceID = errors.New("invalid microservice id")
var ErrInvalidEnvironmentID = errors.New("invalid environment id")
var ErrInvalidLocale = errors.New("invalid locale")
var ErrInvalidBundleJSON = errors.New("invalid bundle json")

// Referential integrity is checked before the insert so a missing parent row
// surfaces as a 404 instead of an opaque foreign-key failure.
var ErrMicroserviceNotFound = errors.New("microservice not found")
var ErrEnvironmentNotFound = errors.New("environment not found")

// ErrLocalizationExists collides with the natural unique key
// (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE). Distinct so a handler can map it
// to HTTP 409.
var ErrLocalizationExists = errors.New("localization for this microservice, environment and locale already exists")

// ErrConflict is returned when an update carried an expected UPDATED_AT that no
// longer matches the stored row: somebody else edited it first. Distinct so a
// handler can map it to HTTP 409.
var ErrConflict = errors.New("localization was modified by someone else")
