package geo

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	DefaultNominatimURL = "https://nominatim.openstreetmap.org"
	DefaultOSRMURL      = "https://router.project-osrm.org"

	// Nominatim's usage policy allows an absolute maximum of one request per
	// second per IP. Pad slightly so clock jitter never trips it.
	DefaultMinInterval = 1100 * time.Millisecond
)

// Config holds what both clients need. Base URLs are overridable so tests and
// local dev can point at a stub or a self-hosted instance.
type Config struct {
	NominatimURL string
	OSRMURL      string

	// Contact is put in the User-Agent. Nominatim throttles anonymous or
	// placeholder agents, so it is required.
	Contact string

	// MinInterval is the minimum gap between upstream Nominatim requests.
	MinInterval time.Duration
}

// ConfigFromEnv reads NOMINATIM_URL, OSRM_URL and MSO_CONTACT. Only
// MSO_CONTACT is mandatory; it is read from the environment (not hard-coded)
// so a personal address never lands in the repository.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		NominatimURL: envOr("NOMINATIM_URL", DefaultNominatimURL),
		OSRMURL:      envOr("OSRM_URL", DefaultOSRMURL),
		Contact:      strings.TrimSpace(os.Getenv("MSO_CONTACT")),
		MinInterval:  DefaultMinInterval,
	}
	if cfg.Contact == "" {
		return Config{}, fmt.Errorf("MSO_CONTACT must be set to an email address or URL: it goes in the User-Agent sent to Nominatim and OSRM")
	}
	return cfg, nil
}

// UserAgent identifies this project and a way to reach its operator.
func (c Config) UserAgent() string {
	return fmt.Sprintf("MSO-Planner/1.0 (+https://github.com/Cuban-Yetis/mso-planner; contact: %s)", c.Contact)
}

// envOr returns an environment value or def when the variable is unset.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}
