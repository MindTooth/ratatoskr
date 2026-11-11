package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MindTooth/ratatoskr/internal/service"
	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

func TestExampleConfig(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("config.example.yaml: %v", err)
	}
}

func TestMaxSnapshotAge(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{want: service.DefaultMaxSnapshotAge},
		{value: "12h", want: 12 * time.Hour},
		{value: "0s", invalid: true},
		{value: "-1h", invalid: true},
		{value: "tomorrow", invalid: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			data := "channels: [v4.22]\n"
			if tc.value != "" {
				data += "maxSnapshotAge: " + tc.value + "\n"
			}
			cfg, err := Parse([]byte(data))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "maxSnapshotAge") {
					t.Fatalf("invalid freshness policy accepted: %v", err)
				}
			} else if err != nil || cfg.Service.MaxSnapshotAge != tc.want {
				t.Fatalf("policy = %s, error = %v; want %s", cfg.Service.MaxSnapshotAge, err, tc.want)
			}
		})
	}
}

func TestMaxSnapshotAgeExceedsRefreshWindow(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		invalid        bool
	}{
		{name: "below default window", settings: "maxSnapshotAge: 6h", invalid: true},
		{name: "equal default window", settings: "maxSnapshotAge: 6h30m", invalid: true},
		{name: "above default window", settings: "maxSnapshotAge: 6h30m1ns"},
		{name: "equal custom window", settings: "refreshInterval: 1h\nrefreshTimeout: 1h\nmaxSnapshotAge: 2h", invalid: true},
		{name: "above custom window", settings: "refreshInterval: 1h\nrefreshTimeout: 1h\nmaxSnapshotAge: 2h1ns"},
		{name: "timeout exceeds age", settings: "refreshInterval: 1h\nrefreshTimeout: 3h\nmaxSnapshotAge: 2h", invalid: true},
		{name: "default age below custom window", settings: "refreshInterval: 24h", invalid: true},
		{name: "zero interval uses default", settings: "refreshInterval: 0s\nmaxSnapshotAge: 6h30m", invalid: true},
		{name: "negative interval uses default", settings: "refreshInterval: -1h\nmaxSnapshotAge: 6h30m", invalid: true},
		{name: "overflowing refresh window", settings: "refreshInterval: 2562047h\nrefreshTimeout: 2562047h", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("channels: [v4.22]\n" + tc.settings))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "maxSnapshotAge must exceed refreshInterval + refreshTimeout") {
					t.Fatalf("invalid refresh window accepted: %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid refresh window rejected: %v", err)
			}
		})
	}
}

func TestParseExpandsChannelWithDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`channels: ["4.22"]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []catalog.Source{
		{ID: "redhat-v4.22", Image: "registry.redhat.io/redhat/redhat-operator-index:v4.22", Platform: DefaultPlatform},
		{ID: "certified-v4.22", Image: "registry.redhat.io/redhat/certified-operator-index:v4.22", Platform: DefaultPlatform},
		{ID: "community-v4.22", Image: "registry.redhat.io/redhat/community-operator-index:v4.22", Platform: DefaultPlatform},
	}
	if len(cfg.Service.Sources) != len(want) {
		t.Fatalf("sources = %#v, want %#v", cfg.Service.Sources, want)
	}
	for i := range want {
		if cfg.Service.Sources[i] != want[i] {
			t.Errorf("sources[%d] = %#v, want %#v", i, cfg.Service.Sources[i], want[i])
		}
	}
	if cfg.Service.RefreshInterval != DefaultRefreshInterval || cfg.Service.RefreshTimeout != DefaultRefreshTimeout || cfg.Service.OpenShiftTimeout != DefaultOpenShiftTimeout || cfg.Service.ParseConcurrency != DefaultParseConcurrency {
		t.Fatalf("unexpected defaults: %#v", cfg.Service)
	}
}

func TestParseSelectsCatalogsAndCanonicalizesChannels(t *testing.T) {
	cfg, err := Parse([]byte(`
platform: linux/arm64
channels: [v4.22, "4.23"]
catalogs: [community, redhat]
`))
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"community-v4.22", "redhat-v4.22", "community-v4.23", "redhat-v4.23"}
	if len(cfg.Service.Sources) != len(wantIDs) {
		t.Fatalf("sources = %#v", cfg.Service.Sources)
	}
	for i, id := range wantIDs {
		if source := cfg.Service.Sources[i]; source.ID != id || source.Platform != "linux/arm64" {
			t.Errorf("sources[%d] = %#v, want id %q on linux/arm64", i, source, id)
		}
	}
}

func TestParseAcceptsStringChannelForms(t *testing.T) {
	for _, channels := range []string{`["v4.22"]`, `['4.22']`, `[!!str 4.22]`, "\n  - |-\n    4.22"} {
		t.Run(channels, func(t *testing.T) {
			cfg, err := Parse([]byte("catalogs: [redhat]\nchannels: " + channels))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Service.Sources) != 1 || cfg.Service.Sources[0].ID != "redhat-v4.22" {
				t.Fatalf("sources = %#v, want only redhat-v4.22", cfg.Service.Sources)
			}
		})
	}
}

func TestExplicitSourcesReplaceOrExtendGeneratedSources(t *testing.T) {
	cfg, err := Parse([]byte(`
channels: ["4.22"]
sources:
  - id: community-v4.22
    image: mirror.example/community:v4.22
    platform: linux/arm64
  - id: private
    image: registry.example/private:latest
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Service.Sources) != 4 {
		t.Fatalf("sources = %#v", cfg.Service.Sources)
	}
	override := cfg.Service.Sources[2]
	if override.Image != "mirror.example/community:v4.22" || override.Platform != "linux/arm64" {
		t.Fatalf("override = %#v", override)
	}
	custom := cfg.Service.Sources[3]
	if custom.ID != "private" || custom.Platform != DefaultPlatform {
		t.Fatalf("custom source = %#v", custom)
	}
}

func TestLegacySourcesRemainValidAndInheritGlobalPlatform(t *testing.T) {
	cfg, err := Parse([]byte(`
platform: linux/arm64/v8
refreshInterval: 1h
refreshTimeout: 15s
debug: true
openshiftGraphURL: https://graph.example.test/api
openshiftTimeout: 45s
sources:
  - id: legacy
    image: registry.example/catalog:latest
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Service.Sources) != 1 || cfg.Service.Sources[0].Platform != "linux/arm64/v8" {
		t.Fatalf("sources = %#v", cfg.Service.Sources)
	}
	if cfg.Service.RefreshInterval != time.Hour {
		t.Fatalf("refresh interval = %s", cfg.Service.RefreshInterval)
	}
	if cfg.Service.RefreshTimeout != 15*time.Second {
		t.Fatalf("refresh timeout = %s", cfg.Service.RefreshTimeout)
	}
	if !cfg.Debug || cfg.Service.OpenShiftGraphURL != "https://graph.example.test/api" || cfg.Service.OpenShiftTimeout != 45*time.Second {
		t.Fatalf("runtime settings = %#v", cfg)
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		message string
	}{
		{name: "empty", yaml: `{}`, message: "at least one channel or source"},
		{name: "unknown field", yaml: "channels: [v4.22]\nchanels: [v4.23]", message: "field chanels not found"},
		{name: "unquoted channel", yaml: `channels: [4.22]`, message: "quote versions"},
		{name: "patch channel", yaml: `channels: [v4.22.1]`, message: "expected major.minor"},
		{name: "duplicate normalized channel", yaml: `channels: [v4.22, "4.22"]`, message: `duplicate catalog channel "v4.22"`},
		{name: "catalogs without channel", yaml: "catalogs: [redhat]\nsources:\n  - id: x\n    image: example/x", message: "catalogs requires"},
		{name: "duplicate catalog", yaml: "channels: [v4.22]\ncatalogs: [redhat, redhat]", message: `duplicate catalog "redhat"`},
		{name: "unknown catalog", yaml: "channels: [v4.22]\ncatalogs: [marketplace]", message: `unknown catalog "marketplace"`},
		{name: "invalid global platform", yaml: "platform: amd64\nchannels: [v4.22]", message: "expected os/architecture"},
		{name: "invalid source platform", yaml: "sources:\n  - id: x\n    image: example/x\n    platform: linux", message: `source "x" platform`},
		{name: "missing source image", yaml: "sources:\n  - id: x", message: "requires id and image"},
		{name: "duplicate explicit source", yaml: "sources:\n  - {id: x, image: example/x}\n  - {id: x, image: example/y}", message: `duplicate explicit source id "x"`},
		{name: "negative concurrency", yaml: "parseConcurrency: -1\nchannels: [v4.22]", message: "at least 1"},
		{name: "bad duration", yaml: "refreshTimeout: later\nchannels: [v4.22]", message: "refreshTimeout"},
		{name: "zero refresh timeout", yaml: "refreshTimeout: 0s\nchannels: [v4.22]", message: "refreshTimeout must be greater than 0"},
		{name: "negative refresh timeout", yaml: "refreshTimeout: -1s\nchannels: [v4.22]", message: "refreshTimeout must be greater than 0"},
		{name: "multiple documents", yaml: "channels: [v4.22]\n---\nchannels: [v4.23]", message: "multiple YAML documents"},
		{name: "trailing empty document", yaml: "channels: [v4.22]\n---\n", message: "multiple YAML documents"},
		{name: "invalid trailing document", yaml: "channels: [v4.22]\n---\n{", message: "decode configuration"},
		{name: "empty input", yaml: "", message: "decode configuration"},
		{name: "comment-only input", yaml: "# nothing here\n", message: "decode configuration"},
		{name: "syntax error", yaml: "channels: [v4.22", message: "decode configuration"},
		{name: "duplicate key", yaml: "channels: [v4.22]\nchannels: [v4.23]", message: `mapping key "channels" already defined`},
		{name: "unknown source field", yaml: "sources:\n  - {id: x, image: example/x, bogus: 1}", message: "field bogus not found"},
		{name: "boolean channel", yaml: `channels: [true]`, message: "quote versions"},
		{name: "mapping channel", yaml: `channels: [{a: b}]`, message: "quote versions"},
		{name: "scalar channels", yaml: `channels: v4.22`, message: "decode configuration"},
		{name: "non-integer concurrency", yaml: "parseConcurrency: abc\nchannels: [v4.22]", message: "`abc`"},
		{name: "top-level sequence", yaml: "- channels: [v4.22]", message: "!!seq"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want message containing %q", err, test.message)
			}
		})
	}
}
