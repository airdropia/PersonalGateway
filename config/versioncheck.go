package config

// VersionCheckConfig controls the optional outbound update check.
//
// The upstream telemetry beacon (visit cookies, install ids, forwarding
// visit data to the release host) was removed with the versioncheck
// package in plan §16 Stage 3. The struct stays so on-disk config files
// that declare version_check do not fail strict parsing; a lightweight
// GitHub-releases check is planned for Stage 9/10 (plan §8.3 Settings
// update notice).
type VersionCheckConfig struct {
	// Enabled turns the daily update check on.
	// Default: true
	Enabled bool `yaml:"enabled" env:"GOMODEL_VERSION_CHECK_ENABLED"`

	// URL is the base URL of the version manifest. The channel file
	// ("core.txt" or "pro.txt") is appended to it.
	// Default: https://gomodel.enterpilot.io/version
	URL string `yaml:"url" env:"GOMODEL_VERSION_CHECK_URL"`

	// IntervalHours is how often the background check runs. Each run is
	// jittered so gateways started together do not query in lockstep.
	// Default: 24
	IntervalHours int `yaml:"interval_hours" env:"GOMODEL_VERSION_CHECK_INTERVAL_HOURS"`

	// TimeoutSeconds bounds a single manifest request.
	// Default: 5
	TimeoutSeconds int `yaml:"timeout_seconds" env:"GOMODEL_VERSION_CHECK_TIMEOUT_SECONDS"`

	// MaxDailyChecks caps how many manifest requests this gateway makes per
	// day in total, so a hostile client cycling cookies cannot turn /version
	// into an outbound request amplifier.
	// Default: 500
	MaxDailyChecks int `yaml:"max_daily_checks" env:"GOMODEL_VERSION_CHECK_MAX_DAILY"`
}

// DefaultVersionCheckURL is the legacy upstream manifest base. Kept as a
// literal now that the versioncheck package is gone; Stage 9/10 replaces
// this with the pgw release manifest.
const DefaultVersionCheckURL = "https://gomodel.enterpilot.io/version"
