package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MindTooth/ratatoskr/internal/openshift"
	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

func catalogFixture(version string) fstest.MapFS {
	return fstest.MapFS{"catalog.json": {Data: []byte(fmt.Sprintf(`
{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.channel","package":"example","name":"stable","entries":[{"name":"v1"},{"name":"v2","replaces":"v1"}]}
{"schema":"olm.bundle","package":"example","name":"v1","properties":[{"type":"olm.package","value":{"version":"1.0.0"}}]}
{"schema":"olm.bundle","package":"example","name":"v2","properties":[{"type":"olm.package","value":{"version":%q}}]}
`, version))}}
}

func getSnapshotResponse(svc *Service, route string) *httptest.ResponseRecorder {
	res := httptest.NewRecorder()
	svc.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, route, nil))
	return res
}

func requireUnavailable(t *testing.T, res *httptest.ResponseRecorder) {
	t.Helper()
	if res.Code != http.StatusServiceUnavailable || strings.Contains(res.Body.String(), `"releases"`) || res.Header().Get("X-Ratatoskr-Generated-At") != "" {
		t.Fatalf("want unavailable without datasource data, got %d: %s", res.Code, res.Body.String())
	}
}

func TestCatalogLastKnownGoodLifecycle(t *testing.T) {
	source := catalog.Source{ID: "community-v4.22", Image: "registry.example/catalog:v4.22"}
	cfg := Config{Sources: []catalog.Source{source}, MaxSnapshotAge: time.Hour}
	svc := New(cfg)
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	configs := catalogFixture("1.1.0")
	svc.readCatalog = func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
		return reader.ReadFS(ctx, source, configs)
	}
	routes := []string{
		"/v1/catalogs/community-v4.22/packages",
		"/v1/catalogs/community-v4.22/packages/example/updates?currentVersion=1.0.0",
		"/v1/catalogs/community-v4.22/packages/example/channel-updates?currentChannel=stable&currentVersion=1.0.0",
		"/v1/catalogs/community-v4.22/packages/example/channel-releases?currentChannel=stable&currentVersion=1.0.0",
		"/v2/catalogs/community/4.22/packages/example/updates?currentVersion=1.0.0",
		"/v2/catalogs/community/4.22/packages/example/channel-updates?currentChannel=stable&currentVersion=1.0.0",
		"/v2/sources/community-v4.22/packages/example/updates?currentVersion=1.0.0",
		"/v2/sources/community-v4.22/packages/example/channel-updates?currentChannel=stable&currentVersion=1.0.0",
		"/readyz",
	}
	for _, route := range routes {
		requireUnavailable(t, getSnapshotResponse(svc, route))
	}
	if err := svc.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	generatedAt := now
	previous := svc.snapshots[source.ID]
	bodies := make(map[string]string)
	for _, route := range routes {
		res := getSnapshotResponse(svc, route)
		if res.Code != http.StatusOK {
			t.Fatalf("successful refresh: %s: %d %s", route, res.Code, res.Body.String())
		}
		bodies[route] = res.Body.String()
	}
	now = now.Add(30 * time.Minute)
	for _, failure := range []string{"partial parse", "missing bundle", "invalid deprecation", "special file", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			switch failure {
			case "special file":
				configs = fstest.MapFS{"catalog.json": {Mode: fs.ModeNamedPipe}}
			case "partial parse":
				configs = catalogFixture("1.2.0")
				configs["catalog.json"].Data = append(configs["catalog.json"].Data, []byte("\n{")...)
			case "invalid deprecation":
				configs = catalogFixture("1.2.0")
				configs["deprecations.json"] = &fstest.MapFile{Data: []byte(`{"schema":"olm.deprecations","package":"example","entries":[{"reference":{"schema":"olm.bundle","name":"missing"},"message":"notice"}]}`)}
			case "missing bundle":
				configs = fstest.MapFS{"catalog.json": {Data: []byte(`{"schema":"olm.package","name":"example","defaultChannel":"stable"}
{"schema":"olm.channel","package":"example","name":"stable","entries":[{"name":"missing"}]}`)}}
			case "timeout":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			if err := svc.Refresh(ctx, source); err == nil {
				t.Fatal("refresh should fail")
			}
			if svc.snapshots[source.ID] != previous || svc.statuses[source.ID].LastSuccess != generatedAt {
				t.Fatal("failed refresh replaced data or renewed its age")
			}
			if len(svc.refreshSem) != 0 {
				t.Fatal("failed refresh retained the shared refresh slot")
			}
			for _, route := range routes {
				res := getSnapshotResponse(svc, route)
				if res.Code != http.StatusOK || res.Body.String() != bodies[route] {
					t.Fatalf("failed refresh exposed different data: %s: %d %s", route, res.Code, res.Body.String())
				}
			}
		})
	}
	res := getSnapshotResponse(svc, routes[1])
	if res.Header().Get("X-Ratatoskr-Generated-At") != generatedAt.Format(time.RFC3339Nano) || res.Header().Get("X-Ratatoskr-Snapshot-Age-Seconds") != "1800.000" {
		t.Fatalf("unexpected freshness: %v", res.Header())
	}
	now = generatedAt.Add(time.Hour)
	if res := getSnapshotResponse(svc, routes[1]); res.Code != http.StatusOK {
		t.Fatal("snapshot should be acceptable at the exact age limit")
	}
	now = now.Add(time.Nanosecond)
	for _, route := range routes {
		requireUnavailable(t, getSnapshotResponse(svc, route))
	}
	for _, route := range []string{"/v1/catalogs/community-v4.22/status", "/v2/catalogs/community/4.22", "/v2/sources/community-v4.22"} {
		var status SourceStatus
		res := getSnapshotResponse(svc, route)
		if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil || status.Available || !status.Stale || status.LastSuccess != generatedAt || status.SnapshotAgeSeconds < 3600 || status.LastError == "" {
			t.Fatalf("unexpected expired status: %+v, %v", status, err)
		}
	}
	for _, route := range []string{"/v1/catalogs", "/v2/catalogs", "/v2/sources"} {
		res := getSnapshotResponse(svc, route)
		if !strings.Contains(res.Body.String(), `"available":false`) || !strings.Contains(res.Body.String(), `"stale":true`) {
			t.Fatalf("listing has stale availability: %s", res.Body.String())
		}
	}
	// Policy reload re-evaluates retained data without requiring new upstream work.
	cfg.MaxSnapshotAge = 2 * time.Hour
	svc.Reload(cfg)
	if res := getSnapshotResponse(svc, routes[1]); res.Code != http.StatusOK {
		t.Fatal("policy reload did not restore acceptable snapshot")
	}
	configs = catalogFixture("1.2.0")
	if err := svc.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	res = getSnapshotResponse(svc, routes[1])
	if !strings.Contains(res.Body.String(), "1.2.0") || svc.statuses[source.ID].LastError != "" || svc.statuses[source.ID].LastSuccess != now {
		t.Fatalf("recovery did not publish new data: %s", res.Body.String())
	}
	// A complete empty catalog is authoritative and may replace nonempty data.
	configs = fstest.MapFS{"empty": {Mode: 0}}
	if err := svc.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if res := getSnapshotResponse(svc, routes[0]); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"packages":[]`) {
		t.Fatalf("complete empty refresh failed: %d %s", res.Code, res.Body.String())
	}
	restarted := New(cfg)
	for _, route := range routes {
		requireUnavailable(t, getSnapshotResponse(restarted, route))
	}
}

func TestCatalogReplacementIsAtomicDuringReadsAndReload(t *testing.T) {
	source := catalog.Source{ID: "test", Image: "registry.example/catalog"}
	cfg := Config{Sources: []catalog.Source{source}}
	svc := New(cfg)
	svc.readCatalog = func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
		return reader.ReadFS(ctx, source, catalogFixture("1.1.0"))
	}
	if err := svc.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	svc.readCatalog = func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
		snapshot, err := reader.ReadFS(ctx, source, catalogFixture("1.2.0"))
		close(started)
		<-finish
		return snapshot, err
	}
	done := make(chan error, 1)
	go func() { done <- svc.Refresh(context.Background(), source) }()
	<-started
	// Public refresh calls share the same serialization as queued refreshes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Refresh(ctx, source); !errors.Is(err, context.Canceled) {
		t.Fatalf("concurrent canceled refresh: %v", err)
	}
	route := "/v2/sources/test/packages/example/updates?currentVersion=1.0.0"
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 20 {
				res := getSnapshotResponse(svc, route)
				if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "1.1.0") || strings.Contains(res.Body.String(), "1.2.0") {
					t.Errorf("in-progress refresh exposed partial data: %d %s", res.Code, res.Body.String())
				}
				for _, listing := range []string{"/v1/catalogs", "/v2/catalogs", "/v2/sources", "/v2/sources/test", "/readyz"} {
					getSnapshotResponse(svc, listing)
				}
			}
		})
	}
	for range 20 {
		svc.Reload(cfg)
	}
	readers.Wait()
	for range 8 {
		readers.Go(func() {
			for range 40 {
				res := getSnapshotResponse(svc, route)
				old, next := strings.Contains(res.Body.String(), "1.1.0"), strings.Contains(res.Body.String(), "1.2.0")
				if res.Code != http.StatusOK || old == next || !strings.Contains(res.Body.String(), "1.0.0") {
					t.Errorf("replacement exposed mixed or partial data: %d %s", res.Code, res.Body.String())
				}
			}
		})
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	res := getSnapshotResponse(svc, route)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "1.2.0") || strings.Contains(res.Body.String(), "1.1.0") {
		t.Fatalf("replacement did not expose new complete snapshot: %d %s", res.Code, res.Body.String())
	}
}

func TestCanceledCatalogRefreshCannotPublish(t *testing.T) {
	source := catalog.Source{ID: "test", Image: "registry.example/catalog"}
	svc := New(Config{Sources: []catalog.Source{source}})
	ctx, cancel := context.WithCancel(context.Background())
	svc.readCatalog = func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
		snapshot, err := reader.ReadFS(ctx, source, catalogFixture("1.1.0"))
		cancel()
		return snapshot, err
	}
	if err := svc.Refresh(ctx, source); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh returned %v", err)
	}
	requireUnavailable(t, getSnapshotResponse(svc, "/v2/sources/test/packages"))
}

func TestOpenShiftOptionalAdvisoryTimeoutPreservesCompleteDiscovery(t *testing.T) {
	svc := New(Config{OpenShiftGraphURL: "https://graph.example.test", OpenShiftTimeout: 20 * time.Millisecond})
	svc.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "graph.example.test" {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		body := `{"nodes":[{"version":"4.22.9"},{"version":"4.22.11","metadata":{"url":"https://access.redhat.com/errata/RHSA-2026:57365"}}],"edges":[[0,1]]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	res := getSnapshotResponse(svc, openShiftLKGRoute)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "4.22.11") || len(svc.openShiftSnapshots) != 1 {
		t.Fatalf("optional enrichment failure rejected complete data: %d %s", res.Code, res.Body.String())
	}
}

const openShiftLKGRoute = "/v1/openshift-releases/stable-4.22/updates?currentVersion=4.22.9"
const openShiftLKGGraph = `{"nodes":[{"version":"4.22.9"},{"version":"4.22.11"}],"edges":[[0,1]]}`

func TestOpenShiftLastKnownGoodLifecycle(t *testing.T) {
	svc := New(Config{MaxSnapshotAge: time.Hour})
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	mode := "graph failure"
	svc.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, openShiftLKGGraph
		isGraph := req.URL.Hostname() == "api.openshift.com"
		if mode == "graph failure" {
			return nil, io.ErrUnexpectedEOF
		}
		if mode == "timeout" {
			return nil, context.DeadlineExceeded
		}
		if !isGraph {
			body = `{"tags":[{"name":"4.22.10","phase":"Accepted"}]}`
			if mode == "partial discovery" {
				body = `{"tags":null}`
			}
		} else if mode == "recovery" || mode == "partial discovery" {
			body = strings.ReplaceAll(openShiftLKGGraph, "4.22.11", "4.22.12")
		} else if mode == "empty" {
			body = `{"nodes":[],"edges":[]}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	requireUnavailable(t, getSnapshotResponse(svc, openShiftLKGRoute))
	mode = "success"
	previous := getSnapshotResponse(svc, openShiftLKGRoute)
	if previous.Code != http.StatusOK || !strings.Contains(previous.Body.String(), "4.22.10") || !strings.Contains(previous.Body.String(), `"isDeprecated":true`) {
		t.Fatalf("complete discovery failed: %d %s", previous.Code, previous.Body.String())
	}
	generatedAt := now
	now = now.Add(30 * time.Minute)
	for _, failure := range []string{"graph failure", "partial discovery", "timeout"} {
		mode = failure
		res := getSnapshotResponse(svc, openShiftLKGRoute)
		if res.Code != http.StatusOK || res.Body.String() != previous.Body.String() || res.Header().Get("X-Ratatoskr-Generated-At") != generatedAt.Format(time.RFC3339Nano) || res.Header().Get("X-Ratatoskr-Snapshot-Age-Seconds") != "1800.000" {
			t.Fatalf("%s replaced or renewed data: %d %s %v", failure, res.Code, res.Body.String(), res.Header())
		}
	}
	mode = "graph failure"
	for _, route := range []string{
		strings.ReplaceAll(openShiftLKGRoute, "stable-4.22", "fast-4.22"),
		openShiftLKGRoute + "&arch=amd64",
		strings.ReplaceAll(openShiftLKGRoute, "currentVersion=4.22.9", "currentVersion=4.22.10"),
		openShiftLKGRoute + "&lag=1",
	} {
		requireUnavailable(t, getSnapshotResponse(svc, route))
	}
	now = generatedAt.Add(time.Hour)
	if res := getSnapshotResponse(svc, openShiftLKGRoute); res.Code != http.StatusOK {
		t.Fatal("snapshot should be acceptable at its age limit")
	}
	now = now.Add(time.Nanosecond)
	requireUnavailable(t, getSnapshotResponse(svc, openShiftLKGRoute))
	mode = "recovery"
	res := getSnapshotResponse(svc, openShiftLKGRoute)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "4.22.12") || strings.Contains(res.Body.String(), "4.22.11") {
		t.Fatalf("recovery failed: %d %s", res.Code, res.Body.String())
	}
	mode = "empty"
	res = getSnapshotResponse(svc, openShiftLKGRoute)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"releases":[]`) {
		t.Fatalf("authoritative empty refresh failed: %d %s", res.Code, res.Body.String())
	}
	mode = "graph failure"
	if res := getSnapshotResponse(svc, openShiftLKGRoute); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"releases":[]`) {
		t.Fatal("successful empty dataset was not retained")
	}
	restarted := New(Config{})
	restarted.httpClient = svc.httpClient
	requireUnavailable(t, getSnapshotResponse(restarted, openShiftLKGRoute))
}

func TestOpenShiftConcurrentReplacementAndConfigChange(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprintf("reload=%t", reload), func(t *testing.T) {
			svc := New(Config{OpenShiftGraphURL: "https://graph.example.test"})
			var calls atomic.Int32
			started, finish := make(chan struct{}), make(chan struct{})
			svc.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				call := calls.Add(1)
				if call == 1 {
					close(started)
					<-finish
				}
				body := openShiftLKGGraph
				if call == 2 {
					body = strings.ReplaceAll(body, "4.22.11", "4.22.12")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- getSnapshotResponse(svc, openShiftLKGRoute) }()
			<-started
			res := getSnapshotResponse(svc, openShiftLKGRoute)
			if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "4.22.12") {
				t.Fatalf("second lookup failed: %d %s", res.Code, res.Body.String())
			}
			if reload {
				svc.Reload(Config{OpenShiftGraphURL: "https://other.example.test"})
				svc.Reload(Config{OpenShiftGraphURL: "https://graph.example.test"})
			}
			close(finish)
			res = <-done
			if reload {
				requireUnavailable(t, res)
				if len(svc.openShiftSnapshots) != 0 {
					t.Fatal("request from old configuration republished data")
				}
			} else if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "4.22.12") || strings.Contains(res.Body.String(), "4.22.11") {
				t.Fatalf("older request overwrote latest dataset: %d %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestOpenShiftRetentionIsBounded(t *testing.T) {
	svc := New(Config{OpenShiftGraphURL: "https://graph.example.test"})
	client := openshift.Client{GraphURL: svc.cfg.OpenShiftGraphURL, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"nodes":[],"edges":[]}`)), Header: make(http.Header)}, nil
	})}}
	for i := range maxOpenShiftSnapshots + 1 {
		_, _, err := svc.openShiftUpdates(context.Background(), client, openshift.UpdateRequest{Channel: "stable-4.22", Architecture: "multi", CurrentVersion: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(svc.openShiftSnapshots) != maxOpenShiftSnapshots {
		t.Fatalf("retained %d results", len(svc.openShiftSnapshots))
	}
	if _, found := svc.openShiftSnapshots[openshift.UpdateRequest{Channel: "stable-4.22", Architecture: "multi", CurrentVersion: "0"}]; found {
		t.Fatal("oldest snapshot was not evicted")
	}
}

func TestOpenShiftEvictionCannotRepublishOlderLookup(t *testing.T) {
	svc := New(Config{OpenShiftGraphURL: "https://graph.example.test"})
	var calls atomic.Int32
	var fail atomic.Bool
	started, finish := make(chan struct{}), make(chan struct{})
	svc.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail.Load() {
			return nil, io.ErrUnexpectedEOF
		}
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-finish
		}
		body := openShiftLKGGraph
		if call == 2 {
			body = strings.ReplaceAll(body, "4.22.11", "4.22.12")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- getSnapshotResponse(svc, openShiftLKGRoute) }()
	<-started
	newer := getSnapshotResponse(svc, openShiftLKGRoute)
	if newer.Code != http.StatusOK || !strings.Contains(newer.Body.String(), "4.22.12") {
		t.Fatalf("newer lookup failed: %d %s", newer.Code, newer.Body.String())
	}
	// Distinct successful lookups evict the newer payload while the older
	// same-key lookup is still running.
	for i := range maxOpenShiftSnapshots {
		route := strings.ReplaceAll(openShiftLKGRoute, "currentVersion=4.22.9", fmt.Sprintf("currentVersion=absent-%d", i))
		if res := getSnapshotResponse(svc, route); res.Code != http.StatusOK {
			t.Fatalf("lookup churn failed: %d %s", res.Code, res.Body.String())
		}
	}
	close(finish)
	requireUnavailable(t, <-done)
	fail.Store(true)
	requireUnavailable(t, getSnapshotResponse(svc, openShiftLKGRoute))
	if len(svc.openShiftInFlight) != 0 {
		t.Fatal("completed lookups retained ordering metadata")
	}
}
