package service

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

func TestLoadedCatalogDeprecationsReachHTTP(t *testing.T) {
	source := catalog.Source{ID: "community-v4.22", Image: "registry.example/catalog:v4.22"}
	svc := New(Config{Sources: []catalog.Source{source}, MaxSnapshotAge: time.Hour})
	configs := catalogFixture("2.0.0")
	configs["next.json"] = &fstest.MapFile{Data: []byte(`
{"schema":"olm.channel","package":"example","name":"next","entries":[{"name":"v3","replaces":"v1"}]}
{"schema":"olm.bundle","package":"example","name":"v3","properties":[{"type":"olm.package","value":{"version":"3.0.0"}}]}
`)}
	configs["deprecations.json"] = &fstest.MapFile{Data: []byte(`{"schema":"olm.deprecations","package":"example","entries":[
{"reference":{"schema":"olm.bundle","name":"v2"},"message":"Retired"},
{"reference":{"schema":"olm.channel","name":"next"},"message":"Retired"}
]}`)}
	svc.readCatalog = func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
		return reader.ReadFS(ctx, source, configs)
	}
	if err := svc.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	type item struct {
		Name       string
		Deprecated bool
	}
	for _, tc := range []struct {
		path              string
		channels, bundles map[string]bool
	}{
		{"/v2/catalogs/community/4.22/packages/example?include=channels,bundles", map[string]bool{"stable": false, "next": true}, map[string]bool{"v1": false, "v2": true, "v3": false}},
		{"/v2/sources/community-v4.22/packages/example?include=channels,bundles", map[string]bool{"stable": false, "next": true}, map[string]bool{"v1": false, "v2": true, "v3": false}},
		{"/v1/catalogs/community-v4.22/packages/example/channels", map[string]bool{"stable": false, "next": true}, nil},
		{"/v1/catalogs/community-v4.22/packages/example/bundles", nil, map[string]bool{"v1": false, "v2": true, "v3": false}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res := getSnapshotResponse(svc, tc.path)
			var body struct{ Channels, Bundles []item }
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", res.Code, res.Body.String())
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			flags := func(items []item) map[string]bool {
				if items == nil {
					return nil
				}
				result := map[string]bool{}
				for _, v := range items {
					result[v.Name] = v.Deprecated
				}
				return result
			}
			if !reflect.DeepEqual(flags(body.Channels), tc.channels) || !reflect.DeepEqual(flags(body.Bundles), tc.bundles) {
				t.Fatalf("unexpected deprecation flags: %s", res.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/v1/catalogs/community-v4.22/packages/example/updates?currentVersion=1.0.0", []string{"1.0.0"}},
		{"/v2/catalogs/community/4.22/packages/example/updates?currentVersion=1.0.0", []string{"1.0.0"}},
		{"/v2/sources/community-v4.22/packages/example/updates?currentVersion=1.0.0", []string{"1.0.0"}},
		{"/v1/catalogs/community-v4.22/packages/example/channel-updates?currentChannel=stable&currentVersion=1.0.0", []string{"stable"}},
		{"/v1/catalogs/community-v4.22/packages/example/channel-releases?currentChannel=stable&currentVersion=1.0.0", []string{}},
		{"/v2/catalogs/community/4.22/packages/example/channel-updates?currentChannel=stable&currentVersion=1.0.0", []string{}},
		{"/v2/sources/community-v4.22/packages/example/channel-updates?currentChannel=stable&currentVersion=1.0.0", []string{}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res := getSnapshotResponse(svc, tc.path)
			var body struct{ Releases []release }
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", res.Code, res.Body.String())
			}
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			versions := make([]string, len(body.Releases))
			for i, r := range body.Releases {
				versions[i] = r.Version
			}
			if !reflect.DeepEqual(versions, tc.want) {
				t.Fatalf("releases = %v, want %v", versions, tc.want)
			}
		})
	}
}
