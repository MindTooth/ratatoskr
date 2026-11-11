// Package config loads, validates, and expands application configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/MindTooth/ratatoskr/internal/service"
	"github.com/MindTooth/ratatoskr/pkg/catalog"
	"go.yaml.in/yaml/v4"
)

const (
	DefaultPlatform         = "linux/amd64"
	DefaultRefreshInterval  = 6 * time.Hour
	DefaultRefreshTimeout   = 30 * time.Minute
	DefaultOpenShiftTimeout = 30 * time.Second
	DefaultParseConcurrency = 2
)

// Config is the effective runtime configuration after defaults and expansion.
type Config struct {
	ListenAddress string
	Debug         bool
	Service       service.Config
}

type fileConfig struct {
	ListenAddress     string           `yaml:"listenAddress"`
	Debug             bool             `yaml:"debug"`
	RefreshInterval   string           `yaml:"refreshInterval"`
	RefreshTimeout    string           `yaml:"refreshTimeout"`
	MaxSnapshotAge    string           `yaml:"maxSnapshotAge"`
	SignaturePolicy   string           `yaml:"signaturePolicy"`
	ParseConcurrency  int              `yaml:"parseConcurrency"`
	RefreshTokenFile  string           `yaml:"refreshTokenFile"`
	OpenShiftGraphURL string           `yaml:"openshiftGraphURL"`
	OpenShiftTimeout  string           `yaml:"openshiftTimeout"`
	Platform          string           `yaml:"platform"`
	Channels          []catalogChannel `yaml:"channels"`
	Catalogs          []string         `yaml:"catalogs"`
	Sources           []catalog.Source `yaml:"sources"`
}

type catalogDefinition struct {
	Name  string
	Image string
}

var builtInCatalogs = []catalogDefinition{
	{Name: "redhat", Image: "registry.redhat.io/redhat/redhat-operator-index"},
	{Name: "certified", Image: "registry.redhat.io/redhat/certified-operator-index"},
	{Name: "community", Image: "registry.redhat.io/redhat/community-operator-index"},
}

var catalogChannelPattern = regexp.MustCompile(`^v?([1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type catalogChannel string

func (c *catalogChannel) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New(`catalog channel must be a string; quote versions without a "v" prefix, for example "4.22"`)
	}
	match := catalogChannelPattern.FindStringSubmatch(node.Value)
	if match == nil {
		return fmt.Errorf("invalid catalog channel %q: expected major.minor or vmajor.minor", node.Value)
	}
	*c = catalogChannel("v" + match[1] + "." + match[2])
	return nil
}

// Parse strictly decodes YAML and returns a validated, fully expanded config.
func Parse(data []byte) (Config, error) {
	var raw fileConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode configuration: multiple YAML documents are not supported")
		}
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	return resolve(raw)
}

func resolve(raw fileConfig) (Config, error) {
	platform := raw.Platform
	if platform == "" {
		platform = DefaultPlatform
	}
	if err := catalog.ValidatePlatform(platform); err != nil {
		return Config{}, fmt.Errorf("platform: %w", err)
	}

	interval, err := parseDuration("refreshInterval", raw.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	timeout, err := parsePositiveDuration("refreshTimeout", raw.RefreshTimeout, DefaultRefreshTimeout)
	if err != nil {
		return Config{}, err
	}
	maxSnapshotAge, err := parsePositiveDuration("maxSnapshotAge", raw.MaxSnapshotAge, service.DefaultMaxSnapshotAge)
	if err != nil {
		return Config{}, err
	}
	effectiveInterval := interval
	if effectiveInterval <= 0 {
		effectiveInterval = DefaultRefreshInterval
	}
	// Subtract positive durations instead of adding a potentially overflowing sum.
	if maxSnapshotAge-timeout <= effectiveInterval {
		return Config{}, fmt.Errorf("maxSnapshotAge must exceed refreshInterval + refreshTimeout (%s + %s)", effectiveInterval, timeout)
	}
	openshiftTimeout, err := parseDuration("openshiftTimeout", raw.OpenShiftTimeout, DefaultOpenShiftTimeout)
	if err != nil {
		return Config{}, err
	}
	parseConcurrency := raw.ParseConcurrency
	if parseConcurrency == 0 {
		parseConcurrency = DefaultParseConcurrency
	}
	if parseConcurrency < 1 {
		return Config{}, errors.New("parseConcurrency must be at least 1")
	}

	sources, err := expandSources(raw, platform)
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddress: raw.ListenAddress,
		Debug:         raw.Debug,
		Service: service.Config{
			Sources:           sources,
			RefreshInterval:   interval,
			RefreshTimeout:    timeout,
			MaxSnapshotAge:    maxSnapshotAge,
			SignaturePolicy:   raw.SignaturePolicy,
			ParseConcurrency:  parseConcurrency,
			RefreshTokenFile:  raw.RefreshTokenFile,
			OpenShiftGraphURL: raw.OpenShiftGraphURL,
			OpenShiftTimeout:  openshiftTimeout,
		},
	}, nil
}

func parseDuration(name, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func parsePositiveDuration(name, value string, fallback time.Duration) (time.Duration, error) {
	parsed, err := parseDuration(name, value, fallback)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be greater than 0", name)
	}
	return parsed, nil
}

func expandSources(raw fileConfig, platform string) ([]catalog.Source, error) {
	if len(raw.Channels) == 0 && len(raw.Sources) == 0 {
		return nil, errors.New("at least one channel or source is required")
	}
	if len(raw.Channels) == 0 && len(raw.Catalogs) > 0 {
		return nil, errors.New("catalogs requires at least one channel")
	}

	channels := make([]string, len(raw.Channels))
	seenChannels := make(map[string]bool, len(raw.Channels))
	for i, channel := range raw.Channels {
		canonical := string(channel)
		if seenChannels[canonical] {
			return nil, fmt.Errorf("duplicate catalog channel %q", canonical)
		}
		seenChannels[canonical] = true
		channels[i] = canonical
	}

	definitions, err := selectCatalogs(raw.Catalogs)
	if err != nil {
		return nil, err
	}
	sources := make([]catalog.Source, 0, len(channels)*len(definitions)+len(raw.Sources))
	positions := make(map[string]int, cap(sources))
	for _, channel := range channels {
		for _, definition := range definitions {
			source := catalog.Source{
				ID:       definition.Name + "-" + channel,
				Image:    definition.Image + ":" + channel,
				Platform: platform,
			}
			positions[source.ID] = len(sources)
			sources = append(sources, source)
		}
	}

	seenExplicit := make(map[string]bool, len(raw.Sources))
	for _, source := range raw.Sources {
		if source.ID == "" || source.Image == "" {
			return nil, errors.New("every source requires id and image")
		}
		if seenExplicit[source.ID] {
			return nil, fmt.Errorf("duplicate explicit source id %q", source.ID)
		}
		seenExplicit[source.ID] = true
		if source.Platform == "" {
			source.Platform = platform
		}
		if err := catalog.ValidatePlatform(source.Platform); err != nil {
			return nil, fmt.Errorf("source %q platform: %w", source.ID, err)
		}
		if position, exists := positions[source.ID]; exists {
			sources[position] = source
			continue
		}
		positions[source.ID] = len(sources)
		sources = append(sources, source)
	}
	return sources, nil
}

func selectCatalogs(names []string) ([]catalogDefinition, error) {
	if len(names) == 0 {
		return builtInCatalogs, nil
	}
	byName := make(map[string]catalogDefinition, len(builtInCatalogs))
	for _, definition := range builtInCatalogs {
		byName[definition.Name] = definition
	}
	selected := make([]catalogDefinition, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return nil, fmt.Errorf("duplicate catalog %q", name)
		}
		definition, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("unknown catalog %q: expected redhat, certified, or community", name)
		}
		seen[name] = true
		selected = append(selected, definition)
	}
	return selected, nil
}
