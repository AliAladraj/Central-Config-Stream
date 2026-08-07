package messaging

import "fmt"

// KV bucket names. These are the live "get-latest + watch" surfaces that
// consuming microservices read from: one bucket per config domain, each
// holding the current value for every key and pushing every change.
const (
	BucketFlags           = "FLAGS"
	BucketServiceSettings = "SERVICESETTINGS"
	BucketLocalization    = "LOCALIZATION"
)

// Key builders. Keys are environment-scoped so a consumer for a given
// environment can watch a single prefix ("{envID}.>") and receive every
// update relevant to it.
//
// KV keys allow: A-Z a-z 0-9 - _ . / = — so "{env}.{key}" is safe.

// FlagKey builds the FLAGS bucket key: {environmentID}.{flagKey}
// e.g. FlagKey(3, "search_v2") -> "3.search_v2"
func FlagKey(environmentID int64, flagKey string) string {
	return fmt.Sprintf("%d.%s", environmentID, flagKey)
}

// ServiceSettingsKey builds the SERVICESETTINGS bucket key: {environmentID}.{microserviceID}
// e.g. ServiceSettingsKey(3, 42) -> "3.42"
func ServiceSettingsKey(environmentID, microserviceID int64) string {
	return fmt.Sprintf("%d.%d", environmentID, microserviceID)
}

// LocalizationKey builds the LOCALIZATION bucket key:
// {environmentID}.{microserviceID}.{locale}
// e.g. LocalizationKey(3, 42, "pt-BR") -> "3.42.pt-BR"
func LocalizationKey(environmentID, microserviceID int64, locale string) string {
	return fmt.Sprintf("%d.%d.%s", environmentID, microserviceID, locale)
}

// EnvironmentPrefix is what a consumer watches to receive every key for its
// environment. e.g. EnvironmentPrefix(3) -> "3.>"
func EnvironmentPrefix(environmentID int64) string {
	return fmt.Sprintf("%d.>", environmentID)
}
