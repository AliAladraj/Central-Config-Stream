package localization

import (
	"encoding/json"
	"time"
)

// Localization is one translation bundle for a (microservice, environment,
// locale) tuple. The whole locale is stored as a single JSON bundle so apps
// load a locale atomically and KV holds one entry per locale.
type Localization struct {
	ID             int64           `json:"id"`
	MicroserviceID int64           `json:"microserviceId"`
	EnvironmentID  int64           `json:"environmentId"`
	Locale         string          `json:"locale"` // e.g. "pt-BR", "en-US"
	BundleJSON     json.RawMessage `json:"bundleJson"`
	UpdatedAt      time.Time       `json:"updatedAt"`

	// ExpectedUpdatedAt opts an update into optimistic concurrency: the write
	// only applies while the stored row still carries this timestamp, so two
	// admins editing the same bundle no longer silently overwrite each other.
	// Absent (nil) keeps the original last-write-wins behaviour.
	ExpectedUpdatedAt *time.Time `json:"expectedUpdatedAt,omitempty"`
}

// Page bounds a list query. The service normalises it, so a repository can bind
// both values as given.
type Page struct {
	Limit  int
	Offset int
}

// Filter narrows the localization list. Zero values match everything.
type Filter struct {
	Page
	MicroserviceID int64
	EnvironmentID  int64
	Locale         string
}
