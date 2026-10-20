package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		want     [3]string
		wantErr  bool
	}{
		{name: "os and architecture", platform: "linux/amd64", want: [3]string{"linux", "amd64", ""}},
		{name: "with variant", platform: "linux/arm64/v8", want: [3]string{"linux", "arm64", "v8"}},
		{name: "missing os", platform: "/amd64", wantErr: true},
		{name: "missing architecture", platform: "linux/", wantErr: true},
		{name: "too many parts", platform: "linux/arm64/v8/extra", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			osChoice, architectureChoice, variantChoice, err := parsePlatform(tt.platform)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePlatform(%q) error = %v, wantErr %t", tt.platform, err, tt.wantErr)
			}
			if got := [3]string{osChoice, architectureChoice, variantChoice}; !tt.wantErr && got != tt.want {
				t.Fatalf("parsePlatform(%q) = %q, want %q", tt.platform, got, tt.want)
			}
		})
	}
}

func TestReadExtracted(t *testing.T) {
	const metadata = `{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.channel","package":"example","name":"stable","entries":[]}`
	for _, tc := range []struct {
		name    string
		configs string
		files   []string
		links   map[string]string
		wantErr bool
	}{
		{name: "absolute image path", configs: "/configs", files: []string{"configs/catalog.json"}},
		{name: "relative image path", configs: "configs", files: []string{"configs/catalog.json"}},
		{name: "config directory link within image", configs: "/configs", files: []string{"data/catalog.json"}, links: map[string]string{"configs": "data"}},
		{name: "ancestor link within image", configs: "/catalog/configs", files: []string{"data/configs/catalog.json"}, links: map[string]string{"catalog": "data"}},
		{name: "file link within image", configs: "/configs", files: []string{"data/catalog.json"}, links: map[string]string{"configs/catalog.json": "../data/catalog.json"}},
		{name: "config directory escape", configs: "/configs", links: map[string]string{"configs": "../outside"}, wantErr: true},
		{name: "ancestor directory escape", configs: "/catalog/configs", links: map[string]string{"catalog": "../outside"}, wantErr: true},
		{name: "catalog file escape", configs: "/configs", links: map[string]string{"configs/catalog.json": "../../outside/catalog.json"}, wantErr: true},
		{name: "nested catalog file escape", configs: "/configs", links: map[string]string{"configs/operator/catalog.json": "../../../outside/catalog.json"}, wantErr: true},
		{name: "absolute link escape", configs: "/configs", links: map[string]string{"configs": "$outside"}, wantErr: true},
		{name: "traversal", configs: "/../outside", wantErr: true},
		{name: "empty path", wantErr: true},
		{name: "image root", configs: "/", wantErr: true},
		{name: "missing directory", configs: "/missing", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "image")
			outside := filepath.Join(parent, "outside")
			write := func(name string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(metadata), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(outside, "catalog.json"))
			write(filepath.Join(outside, "configs", "catalog.json"))
			for _, name := range tc.files {
				write(filepath.Join(root, name))
			}
			for name, target := range tc.links {
				link := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
					t.Fatal(err)
				}
				if target == "$outside" {
					target = outside
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			source := Source{ID: "local", Image: "example/catalog"}
			snapshot, err := (Reader{}).readExtracted(context.Background(), source, root, tc.configs)
			if tc.wantErr {
				if err == nil || snapshot != nil {
					t.Fatalf("readExtracted() = (%#v, %v), want nil snapshot and error", snapshot, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Source != source || len(snapshot.Packages) != 1 || snapshot.Packages["example"] == nil || snapshot.Packages["example"].Channels["stable"] == nil {
				t.Fatalf("unexpected snapshot: %#v", snapshot)
			}
		})
	}
}

func TestAddChannelMetaUsesNameField(t *testing.T) {
	snapshot := &Snapshot{Packages: map[string]*Package{}}
	if err := addMeta(snapshot, "olm.channel", []byte(`{"package":"strimzi-kafka-operator","name":"stable","entries":[{"name":"strimzi-cluster-operator.v0.47.0"}]}`)); err != nil {
		t.Fatal(err)
	}
	channel := snapshot.Packages["strimzi-kafka-operator"].Channels["stable"]
	if channel == nil || channel.Name != "stable" {
		t.Fatalf("channel = %#v, want stable", channel)
	}
}

func TestReadExtractedDeprecations(t *testing.T) {
	root := t.TempDir()
	configs := filepath.Join(root, "configs")
	if err := os.Mkdir(configs, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"metadata.json": `{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.channel","package":"example","name":"stable","entries":[{"name":"v1"}]}
{"schema":"olm.bundle","package":"example","name":"v1","properties":[{"type":"olm.package","value":{"version":"1.0.0"}}]}`,
		"deprecations.json": `{"schema":"olm.deprecations","package":"example","entries":[{"reference":{"schema":"olm.channel","name":"stable"},"message":"notice"},{"reference":{"schema":"olm.bundle","name":"v1"},"message":"notice"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(configs, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := (Reader{}).readExtracted(context.Background(), Source{ID: "local"}, root, "/configs")
	if err != nil {
		t.Fatal(err)
	}
	p := snapshot.Packages["example"]
	if !p.Channels["stable"].Deprecated || !p.Bundles["v1"].Deprecated {
		t.Fatal("extracted catalog lost deprecation flags")
	}
}
