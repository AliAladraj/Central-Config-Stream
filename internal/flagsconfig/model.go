package flagsconfig

import (
	"time"
)

type Flag struct {
	ID        int64     `json:"id"`
	FlagKey   string    `json:"flagKey"`
	IsActive  int64     `json:"isActive"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type FlagValue struct {
	ID            int64     `json:"id"`
	EnvironmentID int64     `json:"environmentId"`
	FlagId        int64     `json:"flagId"`
	Value         string    `json:"value"`
	Enabled       int64     `json:"enabled"`
	UpdatedAt     time.Time `json:"updatedAt"`

	// ExpectedUpdatedAt opts an update into optimistic concurrency: the write
	// only applies while the stored row still carries this timestamp, so two
	// admins editing the same flag no longer silently overwrite each other.
	// Absent (nil) keeps the original last-write-wins behaviour.
	ExpectedUpdatedAt *time.Time `json:"expectedUpdatedAt,omitempty"`
}

// Page bounds a list query. The service normalises it, so a repository can bind
// both values as given.
type Page struct {
	Limit  int
	Offset int
}

// FlagFilter narrows the flag list. An empty FlagKey matches every flag.
type FlagFilter struct {
	Page
	FlagKey string
}

// FlagValueFilter narrows the flag-value list. Zero values match everything.
type FlagValueFilter struct {
	Page
	EnvironmentID int64
	FlagKey       string
}

// FlagValueRow is a flag value with its flag's key already resolved — without
// it a list caller cannot tell which flag a row belongs to without a second
// request per row.
type FlagValueRow struct {
	ID            int64     `json:"id"`
	EnvironmentID int64     `json:"environmentId"`
	FlagId        int64     `json:"flagId"`
	FlagKey       string    `json:"flagKey"`
	Value         string    `json:"value"`
	Enabled       int64     `json:"enabled"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// DeletedFlagValue carries just enough of a removed row to purge its KV key.
// Deleting a flag removes many of them at once.
type DeletedFlagValue struct {
	EnvironmentID int64
	FlagKey       string
}
