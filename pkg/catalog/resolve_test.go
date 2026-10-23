package catalog

import (
	"reflect"
	"sort"
	"testing"
)

func TestRangeMatchesOLM(t *testing.T) {
	for _, tc := range []struct {
		expr, version string
		want          bool
	}{
		{">=1.0.0 <2.0.0", "0.9.9", false},
		{">=1.0.0 <2.0.0", "1.0.0-rc.1", false},
		{">=1.0.0 <2.0.0", "1.0.0", true},
		{">=1.0.0 <2.0.0", "1.5.0-beta.1", true},
		{">=1.0.0 <2.0.0", "2.0.0-rc.1", true},
		{">=1.0.0 <2.0.0", "2.0.0", false},
		{">1.0.0 <=2.0.0", "1.0.0", false},
		{">1.0.0 <=2.0.0", "2.0.0", true},
		{">=1.0.0-0 <2.0.0-0", "1.0.0-rc.1", true},
		{">=1.0.0-0 <2.0.0-0", "2.0.0-0", false},
		{">=1.0.0-0 <2.0.0-0", "2.0.0-rc.1", false},
		{"=1.2.3+build.1", "1.2.3+other", true},
		{"!=1.2.3", "1.2.3+other", false},
		{">=1.0.0 <1.2.0 || >=1.3.0 <2.0.0", "1.1.0", true},
		{">=1.0.0 <1.2.0 || >=1.3.0 <2.0.0", "1.2.0", false},
		{">=1.0.0 <1.2.0 || >=1.3.0 <2.0.0", "1.3.0", true},
		{"  >= 1.0.0  < 2.0.0  ", "1.1.0", true},
		{"1.x", "1.2.3", true},
		{"1.2.x", "1.2.3", true},
		{"1.2.x", "1.3.0", false},
		{"1.0.0 - 2.0.0", "1.5.0", false},
		{"^1.0.0", "1.1.0", false},
		{"~1.0.0", "1.0.1", false},
		{">=1.0.0, <2.0.0", "1.1.0", false},
		{">=1.0", "1.1.0", false},
		{"1.*", "1.1.0", false},
		{"1.X", "1.1.0", false},
		{">=1.0.0||<2.0.0", "1.1.0", false},
		{">=1.0.0 || || <2.0.0", "0.9.0", false},
		{">=1.0.0 || || <2.0.0", "1.1.0", false},
		{">=1.0.0 || x || <2.0.0", "0.9.0", false},
		{">=1.0.0 || x || <2.0.0", "1.1.0", false},
		{">=1.0.0 || | || <2.0.0", "0.9.0", false},
		{">=1.0.0 || | || <2.0.0", "1.1.0", false},
		{"", "1.0.0", false},
		{" ", "1.0.0", false},
		{">=1.0.0", "", false},
		{">=1.0.0", "v1.2.3", false},
		{">=1.0.0", "1.2", false},
		{">=1.0.0", "01.2.3", false},
		{">=1.0.0", "1.2.3-01", false},
		{">=1.0.0", " 1.2.3 ", false},
	} {
		t.Run(tc.expr+"/"+tc.version, func(t *testing.T) {
			if got := rangeMatches(tc.expr, tc.version); got != tc.want {
				t.Fatalf("rangeMatches(%q, %q) = %t, want %t", tc.expr, tc.version, got, tc.want)
			}
		})
	}
}

func TestSkipRangeGraphQueries(t *testing.T) {
	for _, tc := range []struct {
		name, expr, current string
		matches             bool
	}{
		{"lower boundary", ">=1.0.0 <2.0.0", "1.0.0", true},
		{"before lower boundary", ">=1.0.0 <2.0.0", "1.0.0-rc.1", false},
		{"prerelease", ">=1.0.0 <2.0.0", "1.5.0-beta.1", true},
		{"before upper boundary", ">=1.0.0 <2.0.0", "2.0.0-rc.1", true},
		{"upper boundary", ">=1.0.0 <2.0.0", "2.0.0", false},
		{"OR second branch", ">=1.0.0 <1.2.0 || >=1.3.0 <2.0.0", "1.3.0", true},
		{"OR gap", ">=1.0.0 <1.2.0 || >=1.3.0 <2.0.0", "1.2.0", false},
		{"unsupported caret", "^1.0.0", "1.1.0", false},
		{"unsafe OR", ">=1.0.0 || || <2.0.0", "0.9.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Package{DefaultChannel: "stable", Bundles: map[string]*Bundle{
				"current": {Name: "current", Version: tc.current},
				"first":   {Name: "first", Version: "3.0.0"},
				"head":    {Name: "head", Version: "4.0.0"},
			}, Channels: map[string]*Channel{
				"stable": {Name: "stable", Entries: []Entry{{Name: "current"}, {Name: "first", SkipRange: tc.expr}, {Name: "head", Replaces: "first"}}},
				"next":   {Name: "next", Entries: []Entry{{Name: "first", SkipRange: tc.expr}, {Name: "head", Replaces: "first"}}},
			}}
			for _, mode := range []string{"direct", "reachable"} {
				want := []string{tc.current}
				if tc.matches {
					want = append(want, "3.0.0")
					if mode == "reachable" {
						want = append(want, "4.0.0")
					}
				}
				got, err := p.VersionUpdates(UpdateRequest{CurrentVersion: tc.current, Mode: mode})
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("VersionUpdates(%s) = %v, %v; want %v", mode, got, err, want)
				}
			}
			for _, selection := range []string{"next", "reachable"} {
				req := ChannelUpdateRequest{CurrentChannel: "stable", CurrentBundle: "current", Selection: selection}
				want := []ChannelRelease{{Channel: "stable", Bundle: "current"}}
				wantChannels := []string{"stable"}
				if tc.matches {
					want = append(want, ChannelRelease{Channel: "next", Bundle: "head"})
					wantChannels = append(wantChannels, "next")
				}
				got, err := p.ChannelReleases(req)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("ChannelReleases(%s) = %v, %v; want %v", selection, got, err, want)
				}
				channels, err := p.ChannelUpdates(req)
				if err != nil || !reflect.DeepEqual(channels, wantChannels) {
					t.Fatalf("ChannelUpdates(%s) = %v, %v; want %v", selection, channels, err, wantChannels)
				}
			}
			wantHeads := []*Bundle{p.Bundles["head"]}
			if !tc.matches {
				wantHeads = append([]*Bundle{p.Bundles["current"]}, wantHeads...)
			}
			if got := p.ChannelHeads("stable"); !reflect.DeepEqual(got, wantHeads) {
				t.Fatalf("ChannelHeads = %v, want %v", got, wantHeads)
			}
		})
	}
}

func TestVersionUpdatesOnlyReturnsDeclaredPath(t *testing.T) {
	p := &Package{Name: "gitops", DefaultChannel: "stable", Bundles: map[string]*Bundle{
		"v1": {Name: "v1", Version: "1.0.0"}, "v2": {Name: "v2", Version: "1.1.0"}, "v3": {Name: "v3", Version: "9.0.0"},
	}, Channels: map[string]*Channel{"stable": {Name: "stable", Entries: []Entry{{Name: "v1"}, {Name: "v2", Replaces: "v1"}, {Name: "v3"}}}}}
	got, err := p.VersionUpdates(UpdateRequest{CurrentVersion: "1.0.0", Mode: "reachable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "1.0.0" || got[1] != "1.1.0" {
		t.Fatalf("got %#v", got)
	}
}

func TestSkipRange(t *testing.T) {
	p := &Package{Name: "gitops", DefaultChannel: "stable", Bundles: map[string]*Bundle{"v1": {Name: "v1", Version: "1.0.0"}, "v2": {Name: "v2", Version: "2.0.0"}}, Channels: map[string]*Channel{"stable": {Name: "stable", Entries: []Entry{{Name: "v1"}, {Name: "v2", SkipRange: ">=1.0.0 <2.0.0"}}}}}
	got, err := p.VersionUpdates(UpdateRequest{CurrentVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
}

func TestSkipRangeIncludesPrerelease(t *testing.T) {
	p := &Package{Name: "gitops", DefaultChannel: "stable", Bundles: map[string]*Bundle{
		"v1": {Name: "v1", Version: "1.1.0-rc.1"},
		"v2": {Name: "v2", Version: "2.0.0"},
	}, Channels: map[string]*Channel{"stable": {Name: "stable", Entries: []Entry{{Name: "v1"}, {Name: "v2", SkipRange: ">=1.0.0 <2.0.0"}}}}}
	got, err := p.VersionUpdates(UpdateRequest{CurrentVersion: "1.1.0-rc.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
}

func TestVersionUpdatesReturnsEmptyForVersionOutsideChannel(t *testing.T) {
	p := &Package{Name: "strimzi", Bundles: map[string]*Bundle{
		"v1": {Name: "v1", Version: "1.0.0"},
	}, Channels: map[string]*Channel{
		"stable": {Name: "stable", Entries: []Entry{{Name: "v1"}}},
	}}
	got, err := p.VersionUpdates(UpdateRequest{Channel: "stable", CurrentVersion: "0.47.0", Mode: "reachable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %#v, want no releases", got)
	}
}

func TestVersionUpdatesRequireCurrentState(t *testing.T) {
	p := &Package{Name: "gitops", DefaultChannel: "stable", Bundles: map[string]*Bundle{
		"v1": {Name: "v1", Version: "1.0.0"},
	}, Channels: map[string]*Channel{
		"stable": {Name: "stable", Entries: []Entry{{Name: "v1"}}},
	}}
	if _, err := p.VersionUpdates(UpdateRequest{}); err == nil {
		t.Fatal("expected current state error")
	}
}

func TestChannelUpdatesRequireCrossChannelEdge(t *testing.T) {
	p := &Package{Name: "gitops", DefaultChannel: "stable-1.20", Bundles: map[string]*Bundle{
		"v120": {Name: "v120", Version: "1.20.4"}, "v121": {Name: "v121", Version: "1.21.0"}, "v999": {Name: "v999", Version: "9.0.0"},
	}, Channels: map[string]*Channel{
		"stable-1.20": {Name: "stable-1.20", Entries: []Entry{{Name: "v120"}}},
		"stable-1.21": {Name: "stable-1.21", Entries: []Entry{{Name: "v121", Replaces: "v120"}}},
		"stable-9":    {Name: "stable-9", Entries: []Entry{{Name: "v999"}}},
	}}
	got, err := p.ChannelUpdates(ChannelUpdateRequest{CurrentChannel: "stable-1.20", CurrentVersion: "1.20.4", Selection: "reachable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "stable-1.20" || got[1] != "stable-1.21" {
		t.Fatalf("got %#v", got)
	}
}

func TestChannelLessUsesSemanticSuffix(t *testing.T) {
	channels := []string{"gitops-1.10", "gitops-1.6", "gitops-1.21", "latest", "strimzi-0.10.x", "strimzi-0.6.x"}
	sort.Slice(channels, func(i, j int) bool { return ChannelLess(channels[i], channels[j]) })
	want := []string{"gitops-1.6", "gitops-1.10", "gitops-1.21", "latest", "strimzi-0.6.x", "strimzi-0.10.x"}
	for i := range want {
		if channels[i] != want[i] {
			t.Fatalf("channels = %#v, want %#v", channels, want)
		}
	}
}

func TestBundleLessOrdersSemanticAndInvalidVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b *Bundle
		want bool
	}{
		{name: "semantic versions", a: &Bundle{Version: "1.2.0"}, b: &Bundle{Version: "1.10.0"}, want: true},
		{name: "invalid before valid", a: &Bundle{Version: "latest"}, b: &Bundle{Version: "1.0.0"}, want: true},
		{name: "valid after invalid", a: &Bundle{Version: "1.0.0"}, b: &Bundle{Version: "latest"}},
		{name: "invalid name tie break", a: &Bundle{Name: "a", Version: "latest"}, b: &Bundle{Name: "b", Version: "latest"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bundleLess(tc.a, tc.b); got != tc.want {
				t.Fatalf("bundleLess(%+v, %+v) = %t, want %t", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestChannelUpdatesNextPrefersSameVersionedFamily(t *testing.T) {
	p := &Package{Name: "strimzi", Bundles: map[string]*Bundle{
		"v47":    {Name: "v47", Version: "0.47.0"},
		"v48":    {Name: "v48", Version: "0.48.0"},
		"stable": {Name: "stable", Version: "0.51.0"},
	}, Channels: map[string]*Channel{
		"strimzi-0.47.x": {Name: "strimzi-0.47.x", Entries: []Entry{{Name: "v47"}}},
		"strimzi-0.48.x": {Name: "strimzi-0.48.x", Entries: []Entry{{Name: "v48", Replaces: "v47"}}},
		"stable":         {Name: "stable", Entries: []Entry{{Name: "stable", Replaces: "v47"}}},
	}}
	got, err := p.ChannelUpdates(ChannelUpdateRequest{CurrentChannel: "strimzi-0.47.x", CurrentVersion: "0.47.0", Selection: "next"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"strimzi-0.47.x", "strimzi-0.48.x"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestChannelReleasesExposeGraphProvenTargetBundle(t *testing.T) {
	p := &Package{Name: "gitops", Bundles: map[string]*Bundle{
		"v120": {Name: "v120", Version: "1.20.4"},
		"v121": {Name: "v121", Version: "1.21.2"},
	}, Channels: map[string]*Channel{
		"gitops-1.20": {Name: "gitops-1.20", Entries: []Entry{{Name: "v120"}}},
		"gitops-1.21": {Name: "gitops-1.21", Entries: []Entry{{Name: "v121", Replaces: "v120"}}},
	}}
	got, err := p.ChannelReleases(ChannelUpdateRequest{CurrentChannel: "gitops-1.20", CurrentBundle: "v120", Selection: "next"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ChannelRelease{{Channel: "gitops-1.20", Bundle: "v120"}, {Channel: "gitops-1.21", Bundle: "v121"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestChannelUpdatesNextDoesNotMoveToOlderSameFamilyChannel(t *testing.T) {
	p := &Package{Name: "strimzi", Bundles: map[string]*Bundle{
		"v43": {Name: "v43", Version: "0.43.0"},
		"v44": {Name: "v44", Version: "0.44.0"},
		"v46": {Name: "v46", Version: "0.46.0"},
	}, Channels: map[string]*Channel{
		"strimzi-0.43.x": {Name: "strimzi-0.43.x", Entries: []Entry{{Name: "v43"}}},
		"strimzi-0.44.x": {Name: "strimzi-0.44.x", Entries: []Entry{{Name: "v44", Replaces: "v43"}}},
		"strimzi-0.45.x": {Name: "strimzi-0.45.x", Entries: []Entry{{Name: "v43"}}},
		"strimzi-0.46.x": {Name: "strimzi-0.46.x", Entries: []Entry{{Name: "v46", Replaces: "v43"}}},
	}}
	got, err := p.ChannelUpdates(ChannelUpdateRequest{CurrentChannel: "strimzi-0.45.x", CurrentBundle: "v43", Selection: "next"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"strimzi-0.45.x", "strimzi-0.46.x"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
