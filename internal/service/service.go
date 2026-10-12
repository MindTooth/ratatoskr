package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MindTooth/ratatoskr/internal/openshift"
	"github.com/MindTooth/ratatoskr/pkg/catalog"
)

type Config struct {
	Sources           []catalog.Source `yaml:"sources"`
	RefreshInterval   time.Duration    `yaml:"refreshInterval"`
	RefreshTimeout    time.Duration    `yaml:"refreshTimeout"`
	MaxSnapshotAge    time.Duration    `yaml:"maxSnapshotAge"`
	SignaturePolicy   string           `yaml:"signaturePolicy"`
	ParseConcurrency  int              `yaml:"parseConcurrency"`
	RefreshTokenFile  string           `yaml:"refreshTokenFile"`
	OpenShiftGraphURL string           `yaml:"openshiftGraphURL"`
	OpenShiftTimeout  time.Duration    `yaml:"openshiftTimeout"`
}

type SourceStatus struct {
	Source             catalog.Source `json:"source"`
	Available          bool           `json:"available"`
	Refreshing         bool           `json:"refreshing"`
	LastAttempt        time.Time      `json:"lastAttempt,omitempty"`
	LastSuccess        time.Time      `json:"lastSuccess,omitempty"`
	LastError          string         `json:"lastError,omitempty"`
	PackageCount       int            `json:"packageCount"`
	Stale              bool           `json:"stale"`
	SnapshotAgeSeconds float64        `json:"snapshotAgeSeconds"`
}

type Service struct {
	cfg                 Config
	mu                  sync.RWMutex
	snapshots           map[string]*catalog.Snapshot
	statuses            map[string]SourceStatus
	queued              map[string]bool
	running             map[string]bool
	refreshSem          chan struct{}
	catalogGeneration   uint64
	httpClient          *http.Client
	runContext          context.Context
	configChanged       chan struct{}
	now                 func() time.Time
	readCatalog         func(context.Context, catalog.Source, catalog.Reader) (*catalog.Snapshot, error)
	openShiftSnapshots  map[openshift.UpdateRequest]openShiftSnapshot
	openShiftInFlight   map[openshift.UpdateRequest]*openShiftLookup
	openShiftAttempt    uint64
	openShiftGeneration uint64
}

// DefaultMaxSnapshotAge bounds how long an authoritative snapshot may be served.
const DefaultMaxSnapshotAge = 24 * time.Hour

var catalogVersionPattern = regexp.MustCompile(`^v?([1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func New(cfg Config) *Service {
	return &Service{
		cfg:           cfg,
		snapshots:     map[string]*catalog.Snapshot{},
		statuses:      map[string]SourceStatus{},
		queued:        map[string]bool{},
		running:       map[string]bool{},
		refreshSem:    make(chan struct{}, 1),
		httpClient:    http.DefaultClient,
		configChanged: make(chan struct{}, 1),
		now:           time.Now,
		readCatalog: func(ctx context.Context, source catalog.Source, reader catalog.Reader) (*catalog.Snapshot, error) {
			return reader.Read(ctx, source)
		},
		openShiftSnapshots: make(map[openshift.UpdateRequest]openShiftSnapshot),
		openShiftInFlight:  make(map[openshift.UpdateRequest]*openShiftLookup),
	}
}

func (s *Service) Refresh(ctx context.Context, source catalog.Source) error {
	select {
	case s.refreshSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.refreshSem }()
	_, err := s.refresh(ctx, source)
	return err
}

// refresh builds a private snapshot and publishes it only after a complete read.
// Callers hold refreshSem so an older catalog refresh cannot replace a newer one.
func (s *Service) refresh(ctx context.Context, source catalog.Source) (uint64, error) {
	slog.Debug("refresh catalog", "source", source.ID, "image", source.Image)
	s.markAttempt(source)
	s.mu.RLock()
	r := catalog.Reader{SignaturePolicy: s.cfg.SignaturePolicy, ParseConcurrency: s.cfg.ParseConcurrency}
	generation := s.catalogGeneration
	s.mu.RUnlock()
	var snap *catalog.Snapshot
	err := ctx.Err()
	if err == nil {
		snap, err = s.readCatalog(ctx, source, r)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		s.markFailure(source, generation, err)
		return generation, err
	}
	s.mu.Lock()
	if generation == s.catalogGeneration && s.currentSourceLocked(source) {
		snap.GeneratedAt = s.now().UTC()
		s.snapshots[source.ID] = snap
		s.statuses[source.ID] = SourceStatus{Source: source, Available: true, Refreshing: s.running[source.ID] || s.queued[source.ID], LastAttempt: s.statuses[source.ID].LastAttempt, LastSuccess: snap.GeneratedAt, PackageCount: len(snap.Packages)}
	}
	s.mu.Unlock()
	slog.Debug("catalog refreshed", "source", source.ID, "packages", len(snap.Packages), "generatedAt", snap.GeneratedAt)
	return generation, nil
}

func (s *Service) markAttempt(source catalog.Source) {
	s.mu.Lock()
	if !s.currentSourceLocked(source) {
		s.mu.Unlock()
		return
	}
	status := s.statuses[source.ID]
	status.Source, status.LastAttempt, status.Refreshing = source, s.now().UTC(), s.running[source.ID] || s.queued[source.ID]
	s.statuses[source.ID] = status
	s.mu.Unlock()
}

func (s *Service) markFailure(source catalog.Source, generation uint64, err error) {
	s.mu.Lock()
	if generation != s.catalogGeneration || !s.currentSourceLocked(source) {
		s.mu.Unlock()
		return
	}
	status := s.statuses[source.ID]
	status.Source, status.LastError, status.Refreshing = source, err.Error(), s.running[source.ID] || s.queued[source.ID]
	status.Available = s.snapshots[source.ID] != nil
	s.statuses[source.ID] = status
	s.mu.Unlock()
}

func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.runContext = ctx
	s.mu.Unlock()
	s.queueAll()
	for {
		timer := time.NewTimer(s.refreshInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.queueAll()
		case <-s.configChanged:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func (s *Service) refreshInterval() time.Duration {
	s.mu.RLock()
	interval := s.cfg.RefreshInterval
	s.mu.RUnlock()
	if interval <= 0 {
		return 6 * time.Hour
	}
	return interval
}

// Reload atomically applies a validated configuration. Catalogs that were
// removed or changed are not served until a refresh of their new source wins.
func (s *Service) Reload(cfg Config) {
	s.mu.Lock()
	previous := s.cfg
	changed := sourceChanges(previous, cfg)
	refreshAll := previous.SignaturePolicy != cfg.SignaturePolicy || previous.ParseConcurrency != cfg.ParseConcurrency
	if refreshAll {
		s.catalogGeneration++
	}
	s.cfg = cfg
	if previous.OpenShiftGraphURL != cfg.OpenShiftGraphURL {
		clear(s.openShiftSnapshots)
		s.openShiftGeneration++
	}
	for id, snap := range s.snapshots {
		if !s.currentSourceLocked(snap.Source) {
			delete(s.snapshots, id)
		}
	}
	for id, status := range s.statuses {
		if !s.currentSourceLocked(status.Source) {
			delete(s.statuses, id)
		}
	}
	s.mu.Unlock()

	s.notifyConfigChanged()
	if refreshAll {
		s.queueAll()
		return
	}
	for _, id := range changed {
		s.QueueRefresh(id)
	}
}

func sourceChanges(previous, next Config) []string {
	old := sourcesByID(previous.Sources)
	changed := make([]string, 0, len(next.Sources))
	for _, source := range next.Sources {
		if existing, ok := old[source.ID]; !ok || existing != source {
			changed = append(changed, source.ID)
		}
	}
	return changed
}

func sourcesByID(sources []catalog.Source) map[string]catalog.Source {
	result := make(map[string]catalog.Source, len(sources))
	for _, source := range sources {
		result[source.ID] = source
	}
	return result
}

func (s *Service) currentSourceLocked(source catalog.Source) bool {
	for _, configured := range s.cfg.Sources {
		if configured == source {
			return true
		}
	}
	return false
}

func (s *Service) sourceLocked(id string) (catalog.Source, bool) {
	for _, source := range s.cfg.Sources {
		if source.ID == id {
			return source, true
		}
	}
	return catalog.Source{}, false
}

func (s *Service) queueAll() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.cfg.Sources))
	for _, source := range s.cfg.Sources {
		ids = append(ids, source.ID)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		s.QueueRefresh(id)
	}
}

// QueueRefresh starts a background refresh unless this source is already
// queued or running. Its returned state is suitable for a 202 API response.
func (s *Service) QueueRefresh(id string) (state string, found bool) {
	s.mu.Lock()
	source, found := s.sourceLocked(id)
	if !found {
		s.mu.Unlock()
		return "", false
	}
	if s.running[id] {
		s.mu.Unlock()
		return "running", true
	}
	if s.queued[id] {
		s.mu.Unlock()
		return "queued", true
	}
	s.queued[id] = true
	status := s.statuses[id]
	status.Source, status.Refreshing = source, true
	s.statuses[id] = status
	ctx := s.runContext
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Unlock()
	go s.refreshQueued(ctx, id)
	return "queued", true
}

func (s *Service) refreshQueued(parent context.Context, id string) {
	select {
	case s.refreshSem <- struct{}{}:
	case <-parent.Done():
		s.finishQueued(id)
		return
	}
	defer func() { <-s.refreshSem }()

	s.mu.Lock()
	delete(s.queued, id)
	source, found := s.sourceLocked(id)
	if found {
		s.running[id] = true
	}
	s.mu.Unlock()
	if !found {
		s.finishQueued(id)
		return
	}

	s.mu.RLock()
	timeout := s.cfg.RefreshTimeout
	s.mu.RUnlock()
	ctx, cancel := parent, func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	generation, err := s.refresh(ctx, source)
	if err != nil {
		slog.Error("refresh catalog", "source", source.ID, "error", err)
	}
	cancel()

	s.mu.Lock()
	delete(s.running, id)
	if status, ok := s.statuses[id]; ok {
		status.Refreshing = false
		s.statuses[id] = status
	}
	changed := generation != s.catalogGeneration || !s.currentSourceLocked(source)
	s.mu.Unlock()
	if changed && parent.Err() == nil {
		s.QueueRefresh(id)
	}
}

func (s *Service) finishQueued(id string) {
	s.mu.Lock()
	delete(s.queued, id)
	delete(s.running, id)
	if status, ok := s.statuses[id]; ok {
		status.Refreshing = false
		s.statuses[id] = status
	}
	s.mu.Unlock()
}

func (s *Service) notifyConfigChanged() {
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/v1/refresh", s.refreshAllHTTP)
	mux.HandleFunc("/v1/catalogs", s.catalog)
	mux.HandleFunc("/v1/catalogs/", s.catalog)
	mux.HandleFunc("/v1/openshift-releases/", s.openshiftReleases)
	mux.HandleFunc("/v2/catalogs", s.catalogV2)
	mux.HandleFunc("/v2/catalogs/", s.catalogV2)
	mux.HandleFunc("/v2/sources", s.sourceV2)
	mux.HandleFunc("/v2/sources/", s.sourceV2)
	return accessLog(mux)
}

func (s *Service) openshiftReleases(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(path.Clean(r.URL.Path), "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "openshift-releases" || parts[3] != "updates" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lag := 0
	if value := r.URL.Query().Get("lag"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			http.Error(w, "lag must be a non-negative integer", http.StatusBadRequest)
			return
		}
		lag = parsed
	}
	architecture := r.URL.Query().Get("arch")
	if architecture == "" {
		architecture = "multi"
	}
	currentVersion := r.URL.Query().Get("currentVersion")
	if currentVersion == "" {
		http.Error(w, "currentVersion is required", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	graphURL, timeout, httpClient := s.cfg.OpenShiftGraphURL, s.cfg.OpenShiftTimeout, s.httpClient
	s.mu.RUnlock()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	client := openshift.Client{GraphURL: graphURL, HTTPClient: httpClient}
	values, generatedAt, err := s.openShiftUpdates(ctx, client, openshift.UpdateRequest{
		Channel:        parts[2],
		Architecture:   architecture,
		CurrentVersion: currentVersion,
		Lag:            lag,
	})
	if err != nil {
		slog.Error("resolve OpenShift releases", "channel", parts[2], "architecture", architecture, "error", err)
		http.Error(w, "OpenShift release data is unavailable", http.StatusServiceUnavailable)
		return
	}
	setSnapshotHeaders(w, generatedAt, s.now())

	changelogURL := ""
	if len(values) > 1 {
		changelogURL = client.ReleaseComparisonURL(architecture, currentVersion, values[len(values)-1].Version)
	}
	writeJSON(w, http.StatusOK, struct {
		Releases     []openshift.Release `json:"releases"`
		SourceURL    string              `json:"sourceUrl"`
		Homepage     string              `json:"homepage"`
		ChangelogURL string              `json:"changelogUrl,omitempty"`
	}{
		Releases:     values,
		SourceURL:    "https://" + architecture + ".ocp.releases.ci.openshift.org",
		Homepage:     "https://openshift.com",
		ChangelogURL: changelogURL,
	})
}

func (s *Service) ready(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	ready := len(s.cfg.Sources) > 0
	for _, source := range s.cfg.Sources {
		snap := s.snapshots[source.ID]
		if snap == nil || snap.Source != source || !s.snapshotAcceptableLocked(snap.GeneratedAt) {
			ready = false
			break
		}
	}
	s.mu.RUnlock()
	if !ready {
		http.Error(w, "not all configured catalogs have acceptable snapshots", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) catalog(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(path.Clean(r.URL.Path), "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" || parts[1] != "catalogs" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 4 && parts[3] == "refresh" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.hasSource(parts[2]) {
			http.NotFound(w, r)
			return
		}
		if !s.authorizeRefresh(w, r) {
			return
		}
		s.refreshHTTP(w, r, parts[2])
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(parts) == 2 {
		s.listCatalogs(w)
		return
	}
	sourceID := parts[2]
	if len(parts) == 4 && parts[3] == "status" {
		s.catalogStatus(w, sourceID)
		return
	}
	if len(parts) == 4 && parts[3] == "packages" {
		s.listPackages(w, r, sourceID)
		return
	}
	if len(parts) != 6 || parts[3] != "packages" {
		http.NotFound(w, r)
		return
	}
	snap := s.snapshot(w, sourceID)
	if snap == nil {
		return
	}
	pkg := snap.Packages[parts[4]]
	if pkg == nil {
		http.NotFound(w, r)
		return
	}
	switch parts[5] {
	case "updates", "channel-updates":
		s.renovate(w, r, pkg, parts[5])
	case "channel-releases":
		s.channelReleases(w, r, pkg)
	case "channels":
		s.channels(w, r, pkg)
	case "bundles":
		s.bundles(w, r, pkg)
	case "graph":
		s.graph(w, r, pkg)
	case "resolve":
		s.resolve(w, r, pkg)
	default:
		http.NotFound(w, r)
	}
}

// catalogV2 exposes generated catalog IDs as catalog/version pairs. It is an
// additive API: v1 remains available for existing Renovate configurations.
func (s *Service) catalogV2(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(path.Clean(r.URL.Path), "/"), "/")
	if len(parts) < 2 || parts[0] != "v2" || parts[1] != "catalogs" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listCatalogsV2(w)
		return
	}
	if len(parts) == 3 && parts[2] == "refresh" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.refreshAllHTTP(w, r)
		return
	}
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	sourceID, ok := catalogSourceID(parts[2], parts[3])
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.sourceV2Resource(w, r, sourceID, parts[4:])
}

// sourceV2 retains an exact-source escape hatch for configured source IDs.
// The package operations mirror catalogV2.
func (s *Service) sourceV2(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(path.Clean(r.URL.EscapedPath()), "/"), "/")
	if len(parts) < 2 || parts[0] != "v2" || parts[1] != "sources" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listSourcesV2(w)
		return
	}
	sourceID, err := url.PathUnescape(parts[2])
	if err != nil {
		http.Error(w, "invalid source ID", http.StatusBadRequest)
		return
	}
	s.sourceV2Resource(w, r, sourceID, parts[3:])
}

func (s *Service) sourceV2Resource(w http.ResponseWriter, r *http.Request, sourceID string, route []string) {
	if len(route) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.catalogStatus(w, sourceID)
		return
	}
	if len(route) == 1 && route[0] == "refresh" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.refreshOneHTTP(w, r, sourceID)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if len(route) == 1 && route[0] == "packages" {
		s.listPackages(w, r, sourceID)
		return
	}
	if len(route) < 2 || route[0] != "packages" {
		http.NotFound(w, r)
		return
	}
	s.packageV2(w, r, sourceID, route[1], route[2:])
}

func (s *Service) refreshOneHTTP(w http.ResponseWriter, r *http.Request, sourceID string) {
	if !s.hasSource(sourceID) {
		http.NotFound(w, r)
		return
	}
	if !s.authorizeRefresh(w, r) {
		return
	}
	s.refreshHTTP(w, r, sourceID)
}

func (s *Service) packageV2(w http.ResponseWriter, r *http.Request, sourceID, packageName string, action []string) {
	snap := s.snapshot(w, sourceID)
	if snap == nil {
		return
	}
	pkg := snap.Packages[packageName]
	if pkg == nil {
		http.NotFound(w, r)
		return
	}
	if len(action) == 0 {
		s.packageDetails(w, r, pkg)
		return
	}
	if len(action) != 1 {
		http.NotFound(w, r)
		return
	}
	switch action[0] {
	case "updates":
		s.versionUpdatesV2(w, r, pkg)
	case "channel-updates":
		s.channelReleases(w, r, pkg)
	default:
		http.NotFound(w, r)
	}
}

func catalogSourceID(name, version string) (string, bool) {
	if name == "" {
		return "", false
	}
	match := catalogVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return "", false
	}
	return name + "-v" + match[1] + "." + match[2], true
}

func catalogReference(sourceID string) (name, version string, ok bool) {
	index := strings.LastIndex(sourceID, "-v")
	if index <= 0 {
		return "", "", false
	}
	match := catalogVersionPattern.FindStringSubmatch(sourceID[index+1:])
	if match == nil {
		return "", "", false
	}
	return sourceID[:index], match[1] + "." + match[2], true
}

func methodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Service) refreshAllHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeRefresh(w, r) {
		return
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.cfg.Sources))
	for _, source := range s.cfg.Sources {
		ids = append(ids, source.ID)
	}
	s.mu.RUnlock()
	type accepted struct {
		Source string `json:"source"`
		State  string `json:"state"`
	}
	results := make([]accepted, 0, len(ids))
	for _, id := range ids {
		state, _ := s.QueueRefresh(id)
		results = append(results, accepted{Source: id, State: state})
	}
	writeJSON(w, http.StatusAccepted, struct {
		Accepted bool       `json:"accepted"`
		Sources  []accepted `json:"sources"`
	}{Accepted: true, Sources: results})
}

// authorizeRefresh keeps the expensive refresh-control endpoints disabled
// unless an administrator configures a token file. The token is read for each
// request so a mounted Secret can rotate without restarting the service.
func (s *Service) authorizeRefresh(w http.ResponseWriter, r *http.Request) bool {
	s.mu.RLock()
	tokenFile := s.cfg.RefreshTokenFile
	s.mu.RUnlock()
	if tokenFile == "" {
		http.NotFound(w, r)
		return false
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		slog.Error("read refresh token", "error", err)
		http.Error(w, "refresh authentication is unavailable", http.StatusServiceUnavailable)
		return false
	}
	token = []byte(strings.TrimSpace(string(token)))
	if len(token) == 0 {
		slog.Error("read refresh token", "error", "token file is empty")
		http.Error(w, "refresh authentication is unavailable", http.StatusServiceUnavailable)
		return false
	}
	scheme, credential, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || credential == "" || !constantTimeEqual(token, []byte(credential)) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="refresh"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}

func constantTimeEqual(expected, actual []byte) bool {
	expectedHash := sha256.Sum256(expected)
	actualHash := sha256.Sum256(actual)
	return subtle.ConstantTimeCompare(expectedHash[:], actualHash[:]) == 1
}

func (s *Service) refreshHTTP(w http.ResponseWriter, _ *http.Request, sourceID string) {
	state, found := s.QueueRefresh(sourceID)
	if !found {
		http.Error(w, "catalog is not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Accepted bool   `json:"accepted"`
		Source   string `json:"source"`
		State    string `json:"state"`
	}{Accepted: true, Source: sourceID, State: state})
}

func (s *Service) hasSource(id string) bool {
	s.mu.RLock()
	_, found := s.sourceLocked(id)
	s.mu.RUnlock()
	return found
}

func (s *Service) snapshot(w http.ResponseWriter, sourceID string) *catalog.Snapshot {
	s.mu.RLock()
	snap := s.snapshots[sourceID]
	if snap != nil && !s.snapshotAcceptableLocked(snap.GeneratedAt) {
		snap = nil
	}
	s.mu.RUnlock()
	if snap == nil {
		http.Error(w, "catalog has no acceptable snapshot", http.StatusServiceUnavailable)
	} else {
		setSnapshotHeaders(w, snap.GeneratedAt, s.now())
	}
	return snap
}

func (s *Service) listCatalogs(w http.ResponseWriter) {
	s.mu.RLock()
	statuses := make([]SourceStatus, 0, len(s.cfg.Sources))
	for _, source := range s.cfg.Sources {
		status, ok := s.statuses[source.ID]
		if !ok {
			status.Source = source
		}
		statuses = append(statuses, s.freshStatusLocked(status))
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, struct {
		Catalogs []SourceStatus `json:"catalogs"`
	}{statuses})
}

func (s *Service) listCatalogsV2(w http.ResponseWriter) {
	type item struct {
		Catalog string `json:"catalog"`
		Version string `json:"version"`
		SourceStatus
	}
	s.mu.RLock()
	items := make([]item, 0, len(s.cfg.Sources))
	for _, source := range s.cfg.Sources {
		name, version, ok := catalogReference(source.ID)
		if !ok {
			continue
		}
		status, ok := s.statuses[source.ID]
		if !ok {
			status.Source = source
		}
		items = append(items, item{Catalog: name, Version: version, SourceStatus: s.freshStatusLocked(status)})
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, struct {
		Catalogs []item `json:"catalogs"`
	}{Catalogs: items})
}

func (s *Service) listSourcesV2(w http.ResponseWriter) {
	s.mu.RLock()
	statuses := make([]SourceStatus, 0, len(s.cfg.Sources))
	for _, source := range s.cfg.Sources {
		status, ok := s.statuses[source.ID]
		if !ok {
			status.Source = source
		}
		statuses = append(statuses, s.freshStatusLocked(status))
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, struct {
		Sources []SourceStatus `json:"sources"`
	}{Sources: statuses})
}

func (s *Service) catalogStatus(w http.ResponseWriter, sourceID string) {
	s.mu.RLock()
	status, ok := s.statuses[sourceID]
	if !ok {
		if source, found := s.sourceLocked(sourceID); found {
			status, ok = SourceStatus{Source: source}, true
		}
	}
	status = s.freshStatusLocked(status)
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "catalog is not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Service) listPackages(w http.ResponseWriter, r *http.Request, sourceID string) {
	snap := s.snapshot(w, sourceID)
	if snap == nil {
		return
	}
	prefix, limit := r.URL.Query().Get("prefix"), queryLimit(r, 100, 1000)
	type item struct {
		Name           string `json:"name"`
		DefaultChannel string `json:"defaultChannel"`
		Channels       int    `json:"channels"`
		Bundles        int    `json:"bundles"`
	}
	items := make([]item, 0, limit)
	for _, name := range sortedPackageNames(snap.Packages) {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		p := snap.Packages[name]
		items = append(items, item{Name: p.Name, DefaultChannel: p.DefaultChannel, Channels: len(p.Channels), Bundles: len(p.Bundles)})
		if len(items) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Packages []item `json:"packages"`
		Limit    int    `json:"limit"`
	}{items, limit})
}

func (s *Service) packageDetails(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	include, err := packageIncludes(r.URL.Query().Get("include"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type bundle struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Image      string `json:"image,omitempty"`
		Deprecated bool   `json:"deprecated"`
	}
	type channel struct {
		Name       string    `json:"name"`
		Deprecated bool      `json:"deprecated"`
		Entries    int       `json:"entries"`
		Heads      []release `json:"heads"`
	}
	type graphChannel struct {
		Name    string          `json:"name"`
		Entries []catalog.Entry `json:"entries"`
	}
	response := struct {
		Name           string         `json:"name"`
		DefaultChannel string         `json:"defaultChannel"`
		ChannelCount   int            `json:"channelCount"`
		BundleCount    int            `json:"bundleCount"`
		Channels       []channel      `json:"channels,omitempty"`
		Bundles        []bundle       `json:"bundles,omitempty"`
		Graph          []graphChannel `json:"graph,omitempty"`
	}{
		Name:           p.Name,
		DefaultChannel: p.DefaultChannel,
		ChannelCount:   len(p.Channels),
		BundleCount:    len(p.Bundles),
	}
	channelNames := sortedChannelNames(p.Channels)
	if include["channels"] {
		response.Channels = make([]channel, 0, len(channelNames))
		for _, name := range channelNames {
			ch := p.Channels[name]
			heads := p.ChannelHeads(name)
			view := channel{Name: name, Deprecated: ch.Deprecated, Entries: len(ch.Entries), Heads: make([]release, len(heads))}
			for i, head := range heads {
				view.Heads[i] = release{Bundle: head.Name, Version: head.Version}
			}
			response.Channels = append(response.Channels, view)
		}
	}
	if include["bundles"] {
		names := make([]string, 0, len(p.Bundles))
		for name := range p.Bundles {
			names = append(names, name)
		}
		sort.Strings(names)
		response.Bundles = make([]bundle, 0, len(names))
		for _, name := range names {
			item := p.Bundles[name]
			response.Bundles = append(response.Bundles, bundle{Name: item.Name, Version: item.Version, Image: item.Image, Deprecated: item.Deprecated})
		}
	}
	if include["graph"] {
		response.Graph = make([]graphChannel, 0, len(channelNames))
		for _, name := range channelNames {
			response.Graph = append(response.Graph, graphChannel{Name: name, Entries: p.Channels[name].Entries})
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func packageIncludes(value string) (map[string]bool, error) {
	include := map[string]bool{}
	if value == "" {
		return include, nil
	}
	for _, name := range strings.Split(value, ",") {
		if name != "channels" && name != "bundles" && name != "graph" {
			return nil, errors.New("include must contain only channels, bundles, or graph")
		}
		include[name] = true
	}
	return include, nil
}

func (s *Service) renovate(w http.ResponseWriter, r *http.Request, p *catalog.Package, action string) {
	var values []string
	var err error
	if action == "channel-updates" {
		values, err = p.ChannelUpdates(catalog.ChannelUpdateRequest{CurrentChannel: r.URL.Query().Get("currentChannel"), CurrentVersion: r.URL.Query().Get("currentVersion"), CurrentBundle: r.URL.Query().Get("currentBundle"), Selection: r.URL.Query().Get("selection")})
	} else {
		values, err = p.VersionUpdates(catalog.UpdateRequest{CurrentVersion: r.URL.Query().Get("currentVersion"), CurrentBundle: r.URL.Query().Get("currentBundle"), Channel: r.URL.Query().Get("channel"), Mode: r.URL.Query().Get("mode")})
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Releases []release `json:"releases"`
	}{releases(values)})
}

func (s *Service) versionUpdatesV2(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	values, err := p.VersionUpdates(catalog.UpdateRequest{
		CurrentVersion: r.URL.Query().Get("currentVersion"),
		CurrentBundle:  r.URL.Query().Get("currentBundle"),
		Channel:        r.URL.Query().Get("operatorChannel"),
		Mode:           r.URL.Query().Get("mode"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Releases []release `json:"releases"`
	}{releases(values)})
}

type release struct {
	Version string `json:"version"`
	Bundle  string `json:"bundle,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

func releases(values []string) []release {
	out := make([]release, len(values))
	for i, value := range values {
		out[i].Version = value
	}
	return out
}

// channelReleases is the Renovate-oriented, graph-safe channel endpoint. Its
// digest field carries the opaque target bundle name so Renovate can persist it
// as currentDigest and supply it on the next lookup.
func (s *Service) channelReleases(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	values, err := p.ChannelReleases(catalog.ChannelUpdateRequest{
		CurrentChannel: r.URL.Query().Get("currentChannel"),
		CurrentVersion: r.URL.Query().Get("currentVersion"),
		CurrentBundle:  r.URL.Query().Get("currentBundle"),
		Selection:      r.URL.Query().Get("selection"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	// The first item represents the installed state. A datasource should expose
	// only update candidates; Renovate already knows its current value.
	out := make([]release, 0, max(len(values)-1, 0))
	for _, value := range values[min(1, len(values)):] {
		out = append(out, release{Version: value.Channel, Digest: value.Bundle})
	}
	writeJSON(w, http.StatusOK, struct {
		Releases []release `json:"releases"`
	}{out})
}

func (s *Service) channels(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	type channel struct {
		Name       string          `json:"name"`
		Deprecated bool            `json:"deprecated"`
		Entries    int             `json:"entries"`
		Heads      []release       `json:"heads"`
		Graph      []catalog.Entry `json:"graph,omitempty"`
	}
	includeEntries := strings.Contains(r.URL.Query().Get("include"), "entries")
	names, out := sortedChannelNames(p.Channels), make([]channel, 0, len(p.Channels))
	for _, name := range names {
		ch := p.Channels[name]
		heads := p.ChannelHeads(name)
		view := channel{Name: name, Deprecated: ch.Deprecated, Entries: len(ch.Entries), Heads: make([]release, len(heads))}
		for i, head := range heads {
			view.Heads[i] = release{Bundle: head.Name, Version: head.Version}
		}
		if includeEntries {
			view.Graph = ch.Entries
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, struct {
		Package        string    `json:"package"`
		DefaultChannel string    `json:"defaultChannel"`
		Channels       []channel `json:"channels"`
	}{p.Name, p.DefaultChannel, out})
}

func (s *Service) bundles(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	channel := r.URL.Query().Get("channel")
	allowed := map[string]bool{}
	if channel != "" {
		ch := p.Channels[channel]
		if ch == nil {
			http.Error(w, "unknown channel", http.StatusNotFound)
			return
		}
		for _, entry := range ch.Entries {
			allowed[entry.Name] = true
		}
	}
	type bundle struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Image      string `json:"image,omitempty"`
		Deprecated bool   `json:"deprecated"`
	}
	names := make([]string, 0, len(p.Bundles))
	for name := range p.Bundles {
		if channel == "" || allowed[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]bundle, 0, len(names))
	for _, name := range names {
		b := p.Bundles[name]
		out = append(out, bundle{Name: b.Name, Version: b.Version, Image: b.Image, Deprecated: b.Deprecated})
	}
	writeJSON(w, http.StatusOK, struct {
		Package string   `json:"package"`
		Bundles []bundle `json:"bundles"`
	}{p.Name, out})
}

func (s *Service) graph(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	requested := r.URL.Query().Get("channel")
	names := sortedChannelNames(p.Channels)
	type graphChannel struct {
		Name    string          `json:"name"`
		Entries []catalog.Entry `json:"entries"`
	}
	out := []graphChannel{}
	for _, name := range names {
		if requested != "" && name != requested {
			continue
		}
		out = append(out, graphChannel{Name: name, Entries: p.Channels[name].Entries})
	}
	if requested != "" && len(out) == 0 {
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Package  string         `json:"package"`
		Channels []graphChannel `json:"channels"`
	}{p.Name, out})
}

func (s *Service) resolve(w http.ResponseWriter, r *http.Request, p *catalog.Package) {
	var candidates []string
	var err error
	kind := r.URL.Query().Get("kind")
	if kind == "channel" {
		candidates, err = p.ChannelUpdates(catalog.ChannelUpdateRequest{CurrentChannel: r.URL.Query().Get("currentChannel"), CurrentVersion: r.URL.Query().Get("currentVersion"), CurrentBundle: r.URL.Query().Get("currentBundle"), Selection: r.URL.Query().Get("selection")})
	} else {
		candidates, err = p.VersionUpdates(catalog.UpdateRequest{CurrentVersion: r.URL.Query().Get("currentVersion"), CurrentBundle: r.URL.Query().Get("currentBundle"), Channel: r.URL.Query().Get("channel"), Mode: r.URL.Query().Get("mode")})
	}
	response := struct {
		Kind       string   `json:"kind"`
		Candidates []string `json:"candidates"`
		Valid      bool     `json:"valid"`
		Reason     string   `json:"reason,omitempty"`
	}{Kind: kind, Candidates: candidates, Valid: err == nil && len(candidates) > 0}
	if err != nil {
		response.Reason = err.Error()
	} else if len(candidates) == 0 {
		response.Reason = "no graph-valid update path"
	}
	writeJSON(w, http.StatusOK, response)
}

func queryLimit(r *http.Request, fallback, maximum int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value < 1 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
func sortedPackageNames(packages map[string]*catalog.Package) []string {
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func sortedChannelNames(channels map[string]*catalog.Channel) []string {
	names := make([]string, 0, len(channels))
	for name := range channels {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return catalog.ChannelLess(names[i], names[j]) })
	return names
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", recorder.statusCode(), "bytes", recorder.bytes, "duration", time.Since(started), "remoteAddr", r.RemoteAddr}
		if slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			attrs = append(attrs, "query", r.URL.RawQuery, "userAgent", r.UserAgent())
		}
		slog.InfoContext(r.Context(), "http request", attrs...)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status, bytes int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}
func (w *responseRecorder) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
