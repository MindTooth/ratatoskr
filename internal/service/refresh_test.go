package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

func TestCatalogReloadDuringRefresh(t *testing.T) {
	source := catalog.Source{ID: "test", Image: "registry.example/catalog"}
	base := Config{Sources: []catalog.Source{source}, SignaturePolicy: "old-policy", ParseConcurrency: 2, RefreshInterval: time.Hour}
	policy, parsing, unrelated, removed := base, base, base, base
	policy.SignaturePolicy = "new-policy"
	parsing.ParseConcurrency = 4
	unrelated.MaxSnapshotAge = 2 * time.Hour
	removed.Sources = nil
	readError := errors.New("read failed")
	for _, tc := range []struct {
		name       string
		configs    []Config
		wantRetry  bool
		oldError   error
		retryError error
		cancel     bool
	}{
		{name: "signature policy", configs: []Config{policy}, wantRetry: true},
		{name: "obsolete failure", configs: []Config{policy}, wantRetry: true, oldError: readError},
		{name: "retry failure", configs: []Config{policy}, wantRetry: true, retryError: readError},
		{name: "policy changes back", configs: []Config{policy, base}, wantRetry: true},
		{name: "repeated reloads coalesce", configs: []Config{policy, policy, policy}, wantRetry: true},
		{name: "parse concurrency", configs: []Config{parsing}, wantRetry: true},
		{name: "unchanged config", configs: []Config{base}},
		{name: "unrelated config", configs: []Config{unrelated}},
		{name: "removed source", configs: []Config{removed}},
		{name: "canceled after reload", configs: []Config{policy}, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			svc := New(base)
			svc.runContext = ctx
			previous := &catalog.Snapshot{Source: source, GeneratedAt: time.Now().Add(-time.Minute)}
			svc.snapshots[source.ID] = previous
			svc.statuses[source.ID] = SourceStatus{Source: source, Available: true, LastSuccess: previous.GeneratedAt, LastError: "previous failure"}
			started := make(chan catalog.Reader, 2)
			type result struct {
				snap *catalog.Snapshot
				err  error
			}
			finish := make(chan result, 2)
			svc.readCatalog = func(ctx context.Context, _ catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
				select {
				case started <- reader:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				select {
				case result := <-finish:
					return result.snap, result.err
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if state, found := svc.QueueRefresh(source.ID); !found || state != "queued" {
				t.Fatalf("initial queue = %q, %t", state, found)
			}
			reader := awaitCatalogReader(t, started)
			if reader.SignaturePolicy != base.SignaturePolicy || reader.ParseConcurrency != base.ParseConcurrency {
				t.Fatalf("initial reader = %+v", reader)
			}
			for range 20 {
				if state, found := svc.QueueRefresh(source.ID); !found || state != "running" {
					t.Fatalf("duplicate queue = %q, %t", state, found)
				}
			}
			for _, cfg := range tc.configs {
				svc.Reload(cfg)
			}
			if tc.cancel {
				cancel()
			}
			old := &catalog.Snapshot{Source: source}
			finish <- result{snap: old, err: tc.oldError}
			next := &catalog.Snapshot{Source: source}
			if tc.wantRetry {
				reader = awaitCatalogReader(t, started)
				cfg := tc.configs[len(tc.configs)-1]
				if reader.SignaturePolicy != cfg.SignaturePolicy || reader.ParseConcurrency != cfg.ParseConcurrency {
					t.Fatalf("retry reader = %+v, want policy %q, concurrency %d", reader, cfg.SignaturePolicy, cfg.ParseConcurrency)
				}
				svc.mu.RLock()
				snap, status := svc.snapshots[source.ID], svc.statuses[source.ID]
				svc.mu.RUnlock()
				if snap != previous || status.LastSuccess != previous.GeneratedAt || status.LastError != "previous failure" || !status.Refreshing || !old.GeneratedAt.IsZero() {
					t.Fatalf("obsolete attempt changed retained data or status: %+v", status)
				}
				finish <- result{snap: next, err: tc.retryError}
			}
			lockCatalogRefresh(t, svc)
			defer func() { <-svc.refreshSem }()
			svc.mu.RLock()
			defer svc.mu.RUnlock()
			if len(svc.running) != 0 || len(svc.queued) != 0 || svc.statuses[source.ID].Refreshing {
				t.Fatalf("refresh work did not finish: running=%v queued=%v", svc.running, svc.queued)
			}
			select {
			case reader := <-started:
				t.Fatalf("unexpected extra read: %+v", reader)
			default:
			}
			want := old
			if tc.wantRetry {
				want = next
			}
			if tc.retryError != nil || tc.cancel {
				want = previous
			}
			if len(tc.configs[len(tc.configs)-1].Sources) == 0 {
				want = nil
			}
			if svc.snapshots[source.ID] != want {
				t.Fatal("refresh published the wrong snapshot")
			}
			if want != nil && svc.statuses[source.ID].LastSuccess != want.GeneratedAt {
				t.Fatal("refresh changed the retained snapshot age")
			}
			if tc.retryError != nil && svc.statuses[source.ID].LastError != tc.retryError.Error() {
				t.Fatalf("retry failure status = %+v", svc.statuses[source.ID])
			}
		})
	}
}

func TestPolicyReloadDuringSynchronousRefresh(t *testing.T) {
	source := catalog.Source{ID: "test", Image: "registry.example/catalog"}
	cfg := Config{Sources: []catalog.Source{source}, SignaturePolicy: "old-policy"}
	svc := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc.runContext = ctx
	started, finish := make(chan catalog.Reader, 1), make(chan struct{})
	svc.readCatalog = func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
		started <- reader
		select {
		case <-finish:
			return &catalog.Snapshot{Source: source}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- svc.Refresh(ctx, source) }()
	awaitCatalogReader(t, started)
	cfg.SignaturePolicy = "new-policy"
	svc.Reload(cfg)
	finish <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reader := awaitCatalogReader(t, started); reader.SignaturePolicy != cfg.SignaturePolicy {
		t.Fatalf("queued reader = %+v", reader)
	}
	svc.mu.RLock()
	snap := svc.snapshots[source.ID]
	svc.mu.RUnlock()
	if snap != nil {
		t.Fatal("obsolete synchronous refresh published a snapshot")
	}
	finish <- struct{}{}
	lockCatalogRefresh(t, svc)
	defer func() { <-svc.refreshSem }()
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	if svc.snapshots[source.ID] == nil {
		t.Fatal("new-policy refresh did not publish")
	}
}

func awaitCatalogReader(t *testing.T, started <-chan catalog.Reader) catalog.Reader {
	t.Helper()
	select {
	case reader := <-started:
		return reader
	case <-time.After(5 * time.Second):
		t.Fatal("catalog read did not start")
		return catalog.Reader{}
	}
}

func lockCatalogRefresh(t *testing.T, svc *Service) {
	t.Helper()
	select {
	case svc.refreshSem <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("catalog refresh did not release its slot")
	}
}
