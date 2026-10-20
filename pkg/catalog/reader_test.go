package catalog_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

// TestReadFSAndResolve verifies catalog normalization and graph queries through the
// public API without registry, server, or cluster access.
func TestReadFSAndResolve(t *testing.T) {
	configs := fstest.MapFS{
		"operator/catalog.json": {Data: []byte(`
{"schema":"olm.channel","package":"example","name":"stable","entries":[{"name":"example.v1.1.0","replaces":"example.v1.0.0"},{"name":"example.v1.0.0"}]}
{"schema":"olm.bundle","package":"example","name":"example.v1.1.0","image":"registry.example/operator:1.1.0","properties":[{"type":"olm.package","value":{"packageName":"example","version":"1.1.0"}}]}
{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.bundle","package":"example","name":"example.v1.0.0","properties":[{"type":"olm.package","value":{"packageName":"example","version":"1.0.0"}}]}
{"schema":"olm.channel","package":"example","name":"next","entries":[{"name":"example.v2.0.0","replaces":"example.v1.1.0"}]}
{"schema":"olm.bundle","package":"example","name":"example.v2.0.0","properties":[{"type":"olm.package","value":{"packageName":"example","version":"2.0.0"}}]}
`)},
	}
	source := catalog.Source{ID: "local"}
	snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), source, configs)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != source || snapshot.GeneratedAt.IsZero() || len(snapshot.Packages) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	p := snapshot.Packages["example"]
	if p == nil || p.Name != "example" || p.DefaultChannel != "stable" {
		t.Fatalf("unexpected package: %#v", p)
	}
	if got := p.Channels["stable"].Entries[0].Name; got != "example.v1.0.0" {
		t.Fatalf("first entry = %q, want normalized name order", got)
	}
	versions, err := p.VersionUpdates(catalog.UpdateRequest{CurrentVersion: "1.0.0", Mode: "reachable"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.0.0", "1.1.0"}; !reflect.DeepEqual(versions, want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
	heads := p.ChannelHeads("stable")
	if len(heads) != 1 || heads[0].Version != "1.1.0" || heads[0].Image != "registry.example/operator:1.1.0" {
		t.Fatalf("unexpected heads: %#v", heads)
	}
	releases, err := p.ChannelReleases(catalog.ChannelUpdateRequest{CurrentChannel: "stable", CurrentVersion: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []catalog.ChannelRelease{{Channel: "stable", Bundle: "example.v1.1.0"}, {Channel: "next", Bundle: "example.v2.0.0"}}
	if !reflect.DeepEqual(releases, want) {
		t.Fatalf("channel releases = %v, want %v", releases, want)
	}
}

// TestReadFSErrors verifies that malformed catalog input returns an error without a
// partial snapshot.
func TestReadFSErrors(t *testing.T) {
	for name, configs := range map[string]fs.FS{
		"nil filesystem":           nil,
		"named pipe":               fstest.MapFS{"catalog.json": {Mode: fs.ModeNamedPipe}},
		"socket":                   fstest.MapFS{"catalog.json": {Mode: fs.ModeSocket}},
		"block device":             fstest.MapFS{"catalog.json": {Mode: fs.ModeDevice}},
		"character device":         fstest.MapFS{"catalog.json": {Mode: fs.ModeDevice | fs.ModeCharDevice}},
		"irregular file":           fstest.MapFS{"catalog.json": {Mode: fs.ModeIrregular}},
		"malformed JSON":           fstest.MapFS{"catalog.json": {Data: []byte(`{`)}},
		"invalid metadata":         fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"olm.package"}`)}},
		"missing schema":           fstest.MapFS{"catalog.json": {Data: []byte(`{"name":"example"}`)}},
		"missing version property": fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"olm.bundle","package":"example","name":"example.v1"}`)}},
		"partial parse": fstest.MapFS{"catalog.json": {Data: []byte(`
{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{`)}},
		"missing filesystem":      os.DirFS(filepath.Join(t.TempDir(), "missing")),
		"missing default channel": fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"olm.package","name":"example","defaultChannel":"stable"}`)}},
		"missing bundle": fstest.MapFS{"catalog.json": {Data: []byte(`
{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.channel","package":"example","name":"stable","entries":[{"name":"example.v1"}]}
`)}},
		"missing version":          fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"olm.bundle","package":"example","name":"example.v1","properties":[{"type":"olm.package","value":{}}]}`)}},
		"invalid version property": fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"olm.bundle","package":"example","name":"example.v1","properties":[{"type":"olm.package","value":"invalid"}]}`)}},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), catalog.Source{}, configs)
			if err == nil || snapshot != nil {
				t.Fatalf("ReadFS() = (%#v, %v), want nil snapshot and error", snapshot, err)
			}
		})
	}
}

func TestReadFSAllowsEmptyCatalog(t *testing.T) {
	configs := fstest.MapFS{"empty": {Mode: fs.ModeDir}}
	snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), catalog.Source{}, configs)
	if err != nil || snapshot == nil || len(snapshot.Packages) != 0 {
		t.Fatalf("ReadFS() = (%#v, %v), want a successful empty catalog", snapshot, err)
	}
}

type openOnlyFS struct {
	fs.FS
	opens int
}

func (f *openOnlyFS) Open(name string) (fs.File, error) {
	f.opens++
	return f.FS.Open(name)
}

func TestReadFSRequiresStatFS(t *testing.T) {
	configs := fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"unknown"}`)}}
	openOnly := &openOnlyFS{FS: configs}
	snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), catalog.Source{}, openOnly)
	if snapshot != nil || err == nil || !strings.Contains(err.Error(), "catalog filesystem must implement fs.StatFS") {
		t.Fatalf("ReadFS() = (%#v, %v), want nil snapshot and StatFS requirement error", snapshot, err)
	}
	if openOnly.opens != 0 {
		t.Fatalf("Open called %d times, want rejection before any open", openOnly.opens)
	}
	withStat := struct{ fs.StatFS }{configs}
	if snapshot, err := (catalog.Reader{}).ReadFS(context.Background(), catalog.Source{}, withStat); err != nil || snapshot == nil {
		t.Fatalf("ReadFS() = (%#v, %v), want regular-file read with StatFS preserved", snapshot, err)
	}
}

func TestReadFSRejectsCanceledLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := (catalog.Reader{}).ReadFS(ctx, catalog.Source{}, fstest.MapFS{"empty": {Mode: fs.ModeDir}})
	if !errors.Is(err, context.Canceled) || snapshot != nil {
		t.Fatalf("ReadFS() = (%#v, %v), want nil snapshot and context.Canceled", snapshot, err)
	}
}

func TestReadRejectsInvalidInputBeforeRegistryAccess(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		source catalog.Source
	}{
		{name: "canceled context", ctx: canceled, source: catalog.Source{ID: "catalog", Image: "example/catalog"}},
		{name: "missing source ID", ctx: context.Background(), source: catalog.Source{Image: "example/catalog"}},
		{name: "missing image", ctx: context.Background(), source: catalog.Source{ID: "catalog"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if snapshot, err := (catalog.Reader{}).Read(tc.ctx, tc.source); err == nil || snapshot != nil {
				t.Fatalf("Read() = (%#v, %v), want nil snapshot and error", snapshot, err)
			}
		})
	}
}

// cancelOnOpenFS cancels after the walker commits to parsing a catalog file.
// A file can contain many buffered records, so the callback must check context.
type cancelOnOpenFS struct {
	fstest.MapFS
	cancel context.CancelFunc
}

func (f cancelOnOpenFS) Open(name string) (fs.File, error) {
	file, err := f.MapFS.Open(name)
	if err == nil && name == "catalog.json" {
		f.cancel()
	}
	return file, err
}

func TestReadFSStopsParsingCanceledFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configs := cancelOnOpenFS{
		MapFS: fstest.MapFS{"catalog.json": {Data: []byte(`
{"schema":"olm.deprecations","package":"example","entries":[]}
{"schema":"olm.channel"}
`)}},
		cancel: cancel,
	}
	snapshot, err := (catalog.Reader{ParseConcurrency: 1}).ReadFS(ctx, catalog.Source{}, configs)
	if snapshot != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadFS = %v, %v; want nil and context.Canceled", snapshot, err)
	}
}
