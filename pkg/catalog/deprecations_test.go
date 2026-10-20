package catalog_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

const deprecationMetadata = `
{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.channel","package":"example","name":"stable","entries":[{"name":"v1"},{"name":"v2","replaces":"v1"},{"name":"v3","replaces":"v2"}]}
{"schema":"olm.channel","package":"example","name":"healthy","entries":[{"name":"v2","replaces":"v1"},{"name":"v3","replaces":"v2"}]}
{"schema":"olm.channel","package":"example","name":"next","entries":[{"name":"v1"},{"name":"v4","replaces":"v1"}]}
{"schema":"olm.channel","package":"example","name":"retired","entries":[{"name":"v5","replaces":"v1"}]}
{"schema":"olm.bundle","package":"example","name":"v1","properties":[{"type":"olm.package","value":{"version":"1.0.0"}}]}
{"schema":"olm.bundle","package":"example","name":"v2","properties":[{"type":"olm.package","value":{"version":"2.0.0"}}]}
{"schema":"olm.bundle","package":"example","name":"v3","properties":[{"type":"olm.package","value":{"version":"3.0.0"}}]}
{"schema":"olm.bundle","package":"example","name":"v4","properties":[{"type":"olm.package","value":{"version":"4.0.0"}}]}
{"schema":"olm.bundle","package":"example","name":"v5","properties":[{"type":"olm.package","value":{"version":"5.0.0"}}]}
`

const deprecationRecord = `{"schema":"olm.deprecations","package":"example","entries":[
{"reference":{"schema":"olm.package"},"message":"Package notice"},
{"reference":{"schema":"olm.bundle","name":"v2"},"message":"Use v3"},
{"reference":{"schema":"olm.bundle","name":"v5"},"message":"Retired"},
{"reference":{"schema":"olm.channel","name":"next"},"message":"Use healthy"}
]}`

func TestReadFSDeprecations(t *testing.T) {
	other := strings.ReplaceAll(deprecationMetadata, `"example"`, `"other"`)
	for _, concurrency := range []int{1, 4} {
		for _, layout := range []string{"before", "after", "first file", "last file", "yaml"} {
			t.Run(fmt.Sprintf("workers=%d/%s", concurrency, layout), func(t *testing.T) {
				configs := fstest.MapFS{"other/catalog.json": {Data: []byte(other)}}
				switch layout {
				case "before":
					configs["catalog.json"] = &fstest.MapFile{Data: []byte(deprecationRecord + "\n" + deprecationMetadata)}
				case "after":
					configs["catalog.json"] = &fstest.MapFile{Data: []byte(deprecationMetadata + "\n" + deprecationRecord)}
				default:
					configs["m-metadata.json"] = &fstest.MapFile{Data: []byte(deprecationMetadata)}
					name, record := "a-deprecations.json", deprecationRecord
					if layout == "last file" {
						name = "z-deprecations.json"
					}
					if layout == "yaml" {
						name = "deprecations.yaml"
						record = `schema: olm.deprecations
package: example
entries:
- reference:
    schema: olm.package
  message: Package notice
- reference:
    schema: olm.bundle
    name: v2
  message: Use v3
- reference:
    schema: olm.bundle
    name: v5
  message: Retired
- reference:
    schema: olm.channel
    name: next
  message: Use healthy
`
					}
					configs[name] = &fstest.MapFile{Data: []byte(record)}
				}
				snapshot, err := (catalog.Reader{ParseConcurrency: concurrency}).ReadFS(context.Background(), catalog.Source{}, configs)
				if err != nil {
					t.Fatal(err)
				}
				p := snapshot.Packages["example"]
				for name, ch := range p.Channels {
					if ch.Deprecated != (name == "next") {
						t.Errorf("channel %s deprecated = %t", name, ch.Deprecated)
					}
				}
				for name, bundle := range p.Bundles {
					if bundle.Deprecated != (name == "v2" || name == "v5") {
						t.Errorf("bundle %s deprecated = %t", name, bundle.Deprecated)
					}
				}
				for _, ch := range snapshot.Packages["other"].Channels {
					if ch.Deprecated {
						t.Error("deprecation crossed package boundary")
					}
				}
				for _, bundle := range snapshot.Packages["other"].Bundles {
					if bundle.Deprecated {
						t.Error("deprecation crossed package boundary")
					}
				}
				for _, tc := range []struct {
					current, mode string
					want          []string
				}{
					{"1.0.0", "direct", []string{"1.0.0"}},
					{"1.0.0", "reachable", []string{"1.0.0", "3.0.0"}},
					{"2.0.0", "direct", []string{"2.0.0", "3.0.0"}},
				} {
					got, err := p.VersionUpdates(catalog.UpdateRequest{CurrentVersion: tc.current, Mode: tc.mode})
					if err != nil || !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("VersionUpdates(%+v) = %v, %v", tc, got, err)
					}
				}
				req := catalog.ChannelUpdateRequest{CurrentChannel: "stable", CurrentBundle: "v1"}
				got, err := p.ChannelReleases(req)
				want := []catalog.ChannelRelease{{Channel: "stable", Bundle: "v1"}, {Channel: "healthy", Bundle: "v3"}}
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("ChannelReleases = %v, %v; want %v", got, err, want)
				}
				channels, err := p.ChannelUpdates(req)
				if err != nil || !reflect.DeepEqual(channels, []string{"stable", "healthy"}) {
					t.Fatalf("ChannelUpdates = %v, %v", channels, err)
				}
				req.CurrentChannel = "next"
				got, err = p.ChannelReleases(req)
				if err != nil || len(got) == 0 || got[0] != (catalog.ChannelRelease{Channel: "next", Bundle: "v1"}) {
					t.Fatalf("deprecated current channel lost: %v, %v", got, err)
				}
				heads := p.ChannelHeads("retired")
				if len(heads) != 1 || heads[0].Name != "v5" || !heads[0].Deprecated {
					t.Fatalf("deprecated head lost: %v", heads)
				}
				if len(p.Channels["stable"].Entries) != 3 || len(p.Channels["healthy"].Entries) != 2 {
					t.Fatal("deprecations changed graph entries")
				}
			})
		}
	}
}

func TestReadFSDeprecationErrors(t *testing.T) {
	entry := func(schema, name string) string {
		return fmt.Sprintf(`{"reference":{"schema":%q,"name":%q},"message":"notice"}`, schema, name)
	}
	record := func(pkg, entries string) string {
		return fmt.Sprintf(`{"schema":"olm.deprecations","package":%q,"entries":[%s]}`, pkg, entries)
	}
	for _, tc := range []struct{ name, input, want string }{
		{"invalid entries type", `{"schema":"olm.deprecations","package":"example","entries":"invalid"}`, "parse deprecation"},
		{"missing package", record("", entry("olm.bundle", "v2")), "package name"},
		{"unknown package", record("missing", entry("olm.bundle", "v2")), "unknown package"},
		{"missing reference schema", record("example", entry("", "v2")), "schema"},
		{"unknown reference schema", record("example", entry("custom", "v2")), "schema"},
		{"missing bundle name", record("example", entry("olm.bundle", "")), "bundle"},
		{"unknown bundle", record("example", entry("olm.bundle", "missing")), "bundle"},
		{"missing channel name", record("example", entry("olm.channel", "")), "channel"},
		{"unknown channel", record("example", entry("olm.channel", "missing")), "channel"},
		{"named package reference", record("example", entry("olm.package", "example")), "package name must be empty"},
		{"duplicate reference", record("example", entry("olm.bundle", "v2")+","+entry("olm.bundle", "v2")), "duplicate deprecation entry"},
		{"duplicate records", deprecationRecord + "\n" + deprecationRecord, "one deprecation per package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configs := fstest.MapFS{"metadata.json": {Data: []byte(deprecationMetadata)}, "deprecations.json": {Data: []byte(tc.input)}}
			snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), catalog.Source{}, configs)
			if snapshot != nil || err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "read FBC") {
				t.Fatalf("ReadFS = %v, %v; want nil and %q error", snapshot, err, tc.want)
			}
		})
	}
}

func TestReadFSPackageDeprecationDoesNotCascade(t *testing.T) {
	configs := fstest.MapFS{"catalog.json": {Data: []byte(deprecationMetadata + `{"schema":"olm.deprecations","package":"example","entries":[{"reference":{"schema":"olm.package"},"message":"notice"}]}`)}}
	snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), catalog.Source{}, configs)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range snapshot.Packages["example"].Channels {
		if ch.Deprecated {
			t.Fatal("package deprecation cascaded to channel")
		}
	}
	for _, b := range snapshot.Packages["example"].Bundles {
		if b.Deprecated {
			t.Fatal("package deprecation cascaded to bundle")
		}
	}
}

func TestReadFSDuplicateDeprecationsAcrossFiles(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		configs := fstest.MapFS{
			"metadata.json":       {Data: []byte(deprecationMetadata)},
			"a-deprecations.json": {Data: []byte(deprecationRecord)},
			"z-deprecations.json": {Data: []byte(deprecationRecord)},
		}
		snapshot, err := (catalog.Reader{ParseConcurrency: concurrency}).ReadFS(context.Background(), catalog.Source{}, configs)
		if snapshot != nil || err == nil || !strings.Contains(err.Error(), "one deprecation per package") {
			t.Fatalf("workers=%d: ReadFS = %v, %v", concurrency, snapshot, err)
		}
	}
}

func TestReadFSDeprecationsForMultiplePackages(t *testing.T) {
	configs := fstest.MapFS{
		"example.json":            {Data: []byte(deprecationMetadata + deprecationRecord)},
		"other.json":              {Data: []byte(strings.ReplaceAll(deprecationMetadata, `"example"`, `"other"`))},
		"other-deprecations.json": {Data: []byte(`{"schema":"olm.deprecations","package":"other","entries":[{"reference":{"schema":"olm.bundle","name":"v3"},"message":"notice"}]}`)},
	}
	snapshot, err := (catalog.Reader{ParseConcurrency: 4}).ReadFS(context.Background(), catalog.Source{}, configs)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Packages["example"].Bundles["v2"].Deprecated || snapshot.Packages["example"].Bundles["v3"].Deprecated || snapshot.Packages["other"].Bundles["v2"].Deprecated || !snapshot.Packages["other"].Bundles["v3"].Deprecated {
		t.Fatal("package-scoped deprecations were mixed")
	}
}
