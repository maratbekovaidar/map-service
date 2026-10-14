// Package pipeline implements the data update pipeline: download the country
// extract, cut the city, build OSRM/tiles/Nominatim/Photon, verify the new
// version with smoke tests and switch the stack over to it.
package pipeline

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"map-service/internal/config"
)

// Extract modes.
const (
	ExtractBBox    = "bbox"
	ExtractPolygon = "polygon"
)

// Config is the updater configuration (see .env.example).
type Config struct {
	config.City

	SourceURL       string
	ExtractMode     string
	ExtractStrategy string
	PolygonFile     string

	DataDir     string // absolute; identical on the host and inside the updater container
	RepoDir     string // absolute
	ComposeFile string
	EnvFile     string
	Project     string

	KeepVersions           int
	Cron                   string
	NominatimPassword      string
	NominatimImportTimeout time.Duration
	SwitchTimeout          time.Duration

	// GatewayURL is where the updater checks /readyz after switching (the
	// gateway service on the internal compose network).
	GatewayURL string

	SmokeGeocodeQuery      string
	SmokeGeocodeMinResults int

	OsmiumBin string
	DockerBin string
}

// LoadConfig reads the updater configuration from the environment.
func LoadConfig(get config.Getenv) (*Config, error) {
	city, err := config.LoadCity(get)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	str := func(key, def string) string {
		if v := strings.TrimSpace(get(key)); v != "" {
			return v
		}
		return def
	}
	c := &Config{City: city}

	c.SourceURL = str("OSM_SOURCE_URL", "")
	if c.SourceURL == "" {
		errs = append(errs, errors.New("OSM_SOURCE_URL is required"))
	}
	c.ExtractMode = strings.ToLower(str("EXTRACT_MODE", ExtractBBox))
	c.ExtractStrategy = str("EXTRACT_STRATEGY", "smart")
	c.PolygonFile = str("CITY_POLYGON_FILE", "")

	c.RepoDir = str("REPO_DIR", "")
	if c.RepoDir == "" {
		wd, werr := os.Getwd()
		if werr != nil {
			errs = append(errs, werr)
		}
		c.RepoDir = wd
	}
	c.DataDir = str("DATA_DIR", filepath.Join(c.RepoDir, "data"))
	for name, dir := range map[string]string{"REPO_DIR": c.RepoDir, "DATA_DIR": c.DataDir} {
		if !filepath.IsAbs(dir) {
			errs = append(errs, fmt.Errorf("%s=%q must be an absolute path", name, dir))
		}
	}
	c.RepoDir = filepath.Clean(c.RepoDir)
	c.DataDir = filepath.Clean(c.DataDir)
	if c.PolygonFile != "" && !filepath.IsAbs(c.PolygonFile) {
		c.PolygonFile = filepath.Join(c.RepoDir, c.PolygonFile)
	}
	c.ComposeFile = str("COMPOSE_FILE_PATH", filepath.Join(c.RepoDir, "deploy", "docker-compose.yml"))
	c.EnvFile = str("COMPOSE_ENV_FILE", filepath.Join(c.RepoDir, ".env"))
	c.Project = str("COMPOSE_PROJECT_NAME", "mapsvc")

	c.KeepVersions = 2
	if v := strings.TrimSpace(get("DATA_KEEP_VERSIONS")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			errs = append(errs, fmt.Errorf("DATA_KEEP_VERSIONS=%q: want an integer >= 1", v))
		} else {
			c.KeepVersions = n
		}
	}
	c.Cron = str("UPDATE_CRON", "0 3 * * 0")
	if _, err := NextRun(c.Cron, time.Now()); err != nil {
		errs = append(errs, err)
	}
	c.NominatimPassword = str("NOMINATIM_PASSWORD", "")
	if c.NominatimPassword == "" {
		errs = append(errs, errors.New("NOMINATIM_PASSWORD is required"))
	}
	c.NominatimImportTimeout = durOr(get, "NOMINATIM_IMPORT_TIMEOUT", 2*time.Hour, &errs)
	c.SwitchTimeout = durOr(get, "SWITCH_TIMEOUT", 5*time.Minute, &errs)

	c.GatewayURL = strings.TrimRight(str("UPDATER_GATEWAY_URL", "http://gateway:8080"), "/")
	c.SmokeGeocodeQuery = str("SMOKE_GEOCODE_QUERY", "Хан Шатыр")
	c.SmokeGeocodeMinResults = 1
	if v := strings.TrimSpace(get("SMOKE_GEOCODE_MIN_RESULTS")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			errs = append(errs, fmt.Errorf("SMOKE_GEOCODE_MIN_RESULTS=%q: want an integer >= 1", v))
		} else {
			c.SmokeGeocodeMinResults = n
		}
	}
	c.OsmiumBin = str("OSMIUM_BIN", "osmium")
	c.DockerBin = str("DOCKER_BIN", "docker")

	switch c.ExtractMode {
	case ExtractBBox:
	case ExtractPolygon:
		if c.PolygonFile == "" {
			errs = append(errs, errors.New("EXTRACT_MODE=polygon requires CITY_POLYGON_FILE"))
		}
	default:
		errs = append(errs, fmt.Errorf("EXTRACT_MODE=%q: want bbox or polygon", c.ExtractMode))
	}
	switch c.ExtractStrategy {
	case "complete_ways", "smart", "simple":
	default:
		errs = append(errs, fmt.Errorf("EXTRACT_STRATEGY=%q: want complete_ways, smart or simple", c.ExtractStrategy))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid updater configuration: %w", err)
	}
	return c, nil
}

func durOr(get config.Getenv, key string, def time.Duration, errs *[]error) time.Duration {
	s := strings.TrimSpace(get(key))
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s=%q: want a positive duration like 90m", key, s))
		return def
	}
	return d
}

// sourceKind classifies OSM_SOURCE_URL.
type sourceKind int

const (
	sourceHTTP sourceKind = iota
	sourceFile
)

// ParseSource resolves OSM_SOURCE_URL into a kind and a location
// (URL for HTTP, absolute path for files).
func ParseSource(raw string) (sourceKind, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, "", fmt.Errorf("OSM_SOURCE_URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return 0, "", fmt.Errorf("OSM_SOURCE_URL %q: missing host", raw)
		}
		return sourceHTTP, raw, nil
	case "file":
		if u.Path == "" {
			return 0, "", fmt.Errorf("OSM_SOURCE_URL %q: empty path", raw)
		}
		return sourceFile, u.Path, nil
	case "":
		if !filepath.IsAbs(raw) {
			return 0, "", fmt.Errorf("OSM_SOURCE_URL %q: local paths must be absolute", raw)
		}
		return sourceFile, raw, nil
	default:
		return 0, "", fmt.Errorf("OSM_SOURCE_URL %q: unsupported scheme %q", raw, u.Scheme)
	}
}
