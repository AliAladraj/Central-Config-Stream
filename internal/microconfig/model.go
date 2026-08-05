package microconfig

import (
	"encoding/json"
	"time"
)

type MicroserviceAppSettings struct {
	ID             int64           `json:"id"`
	MicroserviceID int64           `json:"microserviceId"`
	EnvironmentID  int64           `json:"environmentId"`
	SettingsJSON   json.RawMessage `json:"settingsJson"`
	UpdatedAt      time.Time       `json:"updatedAt"`

	// ExpectedUpdatedAt opts an update into optimistic concurrency: the write
	// only applies while the stored row still carries this timestamp, so two
	// admins editing the same config no longer silently overwrite each other.
	// Absent (nil) keeps the original last-write-wins behaviour.
	ExpectedUpdatedAt *time.Time `json:"expectedUpdatedAt,omitempty"`
}

type Microservice struct {
	ID           int64     `json:"id"`
	Microservice string    `json:"name"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Environment is the other reference table every domain keys off. It lives here
// rather than in its own package because this is where the reference tables are
// already served from, and a separate package would buy nothing but wiring.
type Environment struct {
	ID          int64     `json:"id"`
	Environment string    `json:"name"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Page bounds a list query. The service normalises it, so a repository can bind
// both values as given.
type Page struct {
	Limit  int
	Offset int
}

// AppSettingsFilter narrows the appsettings list. Zero ids match everything.
type AppSettingsFilter struct {
	Page
	MicroserviceID int64
	EnvironmentID  int64
}
