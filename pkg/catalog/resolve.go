package catalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	blang "github.com/blang/semver/v4"
)

// ErrNoUpdatePath means that the installed release has no entry in the
// requested channel. It is a valid catalog result, not a failed query.
var ErrNoUpdatePath = errors.New("current release is not present in channel")

// UpdateRequest selects graph successors of a known bundle or version.
// An empty Channel selects the package default; Mode defaults to direct.
type UpdateRequest struct {
	CurrentVersion string
	CurrentBundle  string
	Channel        string
	Mode           string // direct or reachable
}

// ChannelUpdateRequest validates a manual Subscription channel switch against
// the target channel graph. Channel names are not themselves graph edges.
type ChannelUpdateRequest struct {
	CurrentChannel string
	CurrentVersion string
	CurrentBundle  string
	Selection      string // next or reachable
}

// ChannelRelease is a graph-proven channel transition together with the
// terminal bundle that represents the selected target state. Bundle is an
// opaque catalog bundle name; callers must persist it when they want a later
// request to be validated against the same graph state.
type ChannelRelease struct {
	Channel string
	Bundle  string
}

// VersionUpdates returns the current version plus only declared graph successors.
func (p *Package) VersionUpdates(req UpdateRequest) ([]string, error) {
	channel := req.Channel
	if channel == "" {
		channel = p.DefaultChannel
	}
	ch := p.Channels[channel]
	if ch == nil {
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
	current, err := p.currentBundle(ch, req.CurrentBundle, req.CurrentVersion)
	if err != nil {
		if errors.Is(err, ErrNoUpdatePath) {
			return []string{}, nil
		}
		return nil, err
	}
	seen := map[string]bool{current.Name: true}
	queue := []string{current.Name}
	versions := map[string]bool{}
	if current.Version != "" {
		versions[current.Version] = true
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, next := range p.successors(ch, name, bundleVersion(p.Bundles[name])) {
			if !seen[next.Name] {
				seen[next.Name] = true
				if !next.Deprecated && next.Version != "" {
					versions[next.Version] = true
				}
				if req.Mode == "reachable" {
					queue = append(queue, next.Name)
				}
			}
		}
		if req.Mode != "reachable" {
			break
		}
	}
	out := make([]string, 0, len(versions))
	for v := range versions {
		out = append(out, v)
	}
	sortVersions(out)
	return out, nil
}

// ChannelUpdates returns the current channel and only channels whose graph has
// an entry that accepts the installed bundle. It refuses to guess the bundle.
func (p *Package) ChannelUpdates(req ChannelUpdateRequest) ([]string, error) {
	releases, err := p.ChannelReleases(req)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(releases))
	for i, release := range releases {
		out[i] = release.Channel
	}
	return out, nil
}

// ChannelReleases returns graph-proven channel transitions and the catalog
// bundle to persist as the next state token. It never infers an installed
// bundle from a channel name.
func (p *Package) ChannelReleases(req ChannelUpdateRequest) ([]ChannelRelease, error) {
	currentChannel := p.Channels[req.CurrentChannel]
	if currentChannel == nil {
		return nil, fmt.Errorf("unknown current channel %q", req.CurrentChannel)
	}
	installed, err := p.currentBundle(currentChannel, req.CurrentBundle, req.CurrentVersion)
	if err != nil {
		if errors.Is(err, ErrNoUpdatePath) {
			return []ChannelRelease{}, nil
		}
		return nil, err
	}
	out := []ChannelRelease{{Channel: req.CurrentChannel, Bundle: installed.Name}}
	for name, target := range p.Channels {
		if name == req.CurrentChannel || target.Deprecated {
			continue
		}
		heads := p.channelUpgradeHeads(target, installed)
		if len(heads) == 0 {
			continue
		}
		out = append(out, ChannelRelease{Channel: name, Bundle: heads[len(heads)-1].Name})
	}
	sort.Slice(out[1:], func(i, j int) bool {
		return channelCandidateLess(req.CurrentChannel, out[1:][i].Channel, out[1:][j].Channel)
	})
	if req.Selection == "next" && len(out) > 1 {
		out = append(out[:1], nextChannelRelease(req.CurrentChannel, out[1:])...)
	}
	return out, nil
}

// ChannelHeads returns every terminal bundle reachable in a channel graph.
// Multiple heads are possible in malformed or intentionally branched catalogs.
func (p *Package) ChannelHeads(name string) []*Bundle {
	ch := p.Channels[name]
	if ch == nil {
		return nil
	}
	out := make([]*Bundle, 0, len(ch.Entries))
	for _, entry := range ch.Entries {
		bundle := p.Bundles[entry.Name]
		if bundle != nil && len(p.successors(ch, entry.Name, bundle.Version)) == 0 {
			out = append(out, bundle)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// channelUpgradeHeads follows accepted update edges to non-deprecated terminal
// bundles, ordered by version and name.
func (p *Package) channelUpgradeHeads(target *Channel, installed *Bundle) []*Bundle {
	queue := []string{}
	for _, entry := range target.Entries {
		if entry.Name == installed.Name || entry.Replaces == installed.Name || contains(entry.Skips, installed.Name) || rangeMatches(entry.SkipRange, installed.Version) {
			queue = append(queue, entry.Name)
		}
	}
	seen, heads := map[string]bool{}, []*Bundle{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		next := p.successors(target, name, bundleVersion(p.Bundles[name]))
		if len(next) == 0 { // a terminal node is the reachable channel head
			if bundle := p.Bundles[name]; bundle != nil && !bundle.Deprecated {
				heads = append(heads, bundle)
			}
			continue
		}
		for _, bundle := range next {
			queue = append(queue, bundle.Name)
		}
	}
	sort.Slice(heads, func(i, j int) bool { return bundleLess(heads[i], heads[j]) })
	return heads
}

// nextChannelRelease selects the next higher version in the current channel family,
// falling back to the first different channel.
func nextChannelRelease(current string, candidates []ChannelRelease) []ChannelRelease {
	prefix, currentVersion, currentHasVersion := channelSuffix(current)
	if currentHasVersion {
		for _, candidate := range candidates {
			candidatePrefix, candidateVersion, candidateHasVersion := channelSuffix(candidate.Channel)
			if candidateHasVersion && candidatePrefix == prefix && currentVersion.LessThan(candidateVersion) {
				return []ChannelRelease{candidate}
			}
		}
	}
	for _, candidate := range candidates {
		if candidate.Channel != current {
			return []ChannelRelease{candidate}
		}
	}
	return nil
}

// bundleLess orders bundles by strict semantic version, placing invalid versions
// first and breaking ties by name.
func bundleLess(a, b *Bundle) bool {
	av, aerr := semver.StrictNewVersion(a.Version)
	bv, berr := semver.StrictNewVersion(b.Version)
	if aerr == nil && berr == nil && !av.Equal(bv) {
		return av.LessThan(bv)
	}
	if aerr == nil && berr != nil {
		return false
	}
	if aerr != nil && berr == nil {
		return true
	}
	return a.Name < b.Name
}

// currentBundle resolves an explicit bundle or an unambiguous version within the
// channel; an explicit bundle takes precedence.
func (p *Package) currentBundle(ch *Channel, name, version string) (*Bundle, error) {
	if name == "" && version == "" {
		return nil, errors.New("currentBundle or currentVersion is required")
	}
	if name != "" {
		b := p.Bundles[name]
		if b == nil {
			return nil, fmt.Errorf("unknown current bundle %q", name)
		}
		if !channelContains(ch, name) {
			return nil, fmt.Errorf("%w: bundle %q in channel %q", ErrNoUpdatePath, name, ch.Name)
		}
		return b, nil
	}
	var found *Bundle
	for _, e := range ch.Entries {
		b := p.Bundles[e.Name]
		if b != nil && b.Version == version {
			if found != nil {
				return nil, fmt.Errorf("current version %q is ambiguous; provide currentBundle", version)
			}
			found = b
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: version %q in channel %q", ErrNoUpdatePath, version, ch.Name)
	}
	return found, nil
}

// channelContains reports whether the channel declares an entry for the bundle.
func channelContains(ch *Channel, bundle string) bool {
	for _, entry := range ch.Entries {
		if entry.Name == bundle {
			return true
		}
	}
	return false
}

// successors returns bundles accepted by replaces, skips, or skipRange edges from
// the current bundle.
func (p *Package) successors(ch *Channel, name, version string) []*Bundle {
	var out []*Bundle
	for _, e := range ch.Entries {
		matches := e.Replaces == name || contains(e.Skips, name) || rangeMatches(e.SkipRange, version)
		if !matches {
			continue
		}
		if b := p.Bundles[e.Name]; b != nil {
			out = append(out, b)
		}
	}
	return out
}

// bundleVersion returns the bundle version, or an empty string for a missing bundle.
func bundleVersion(b *Bundle) string {
	if b == nil {
		return ""
	}
	return b.Version
}

// contains reports whether the slice includes the requested string.
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// rangeMatches checks an OLM skip range against a strict semantic version, including
// prereleases; invalid inputs do not match.
func rangeMatches(expr, version string) bool {
	if expr == "" || version == "" {
		return false
	}
	v, err := blang.Parse(version)
	if err != nil {
		return false
	}
	r, err := parseSkipRange(expr)
	if err != nil {
		return false
	}
	return r(v)
}

// parseSkipRange uses OLM's parser without allowing unsafe empty OR branches.
func parseSkipRange(expr string) (blang.Range, error) {
	r, err := blang.ParseRange(expr)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("skipRange has no constraints")
	}
	// blang v4.0.0 accepts empty or token-dropped OR branches whose predicates panic.
	if strings.Contains(expr, "||") {
		for _, clause := range strings.Split(expr, "||") {
			part, err := blang.ParseRange(clause)
			if err != nil {
				return nil, fmt.Errorf("invalid skipRange OR clause: %w", err)
			}
			if part == nil {
				return nil, errors.New("skipRange OR clause has no constraints")
			}
		}
	}
	return r, nil
}

// sortVersions sorts semantic versions in ascending order, followed by invalid
// versions in lexical order.
func sortVersions(xs []string) {
	sort.Slice(xs, func(i, j int) bool {
		a, ea := semver.StrictNewVersion(xs[i])
		b, eb := semver.StrictNewVersion(xs[j])
		if ea == nil && eb == nil {
			return a.LessThan(b)
		}
		if ea == nil {
			return true
		}
		if eb == nil {
			return false
		}
		return xs[i] < xs[j]
	})
}

// channelLess compares version suffixes within a channel family and otherwise uses lexical order.
func channelLess(a, b string) bool {
	pa, va, oka := channelSuffix(a)
	pb, vb, okb := channelSuffix(b)
	if oka && okb && pa == pb {
		return va.LessThan(vb)
	}
	return a < b
}

// channelCandidateLess places the current channel family first, then applies channel ordering.
func channelCandidateLess(current, a, b string) bool {
	prefix, _, hasVersion := channelSuffix(current)
	if hasVersion {
		prefixA, _, hasVersionA := channelSuffix(a)
		prefixB, _, hasVersionB := channelSuffix(b)
		sameFamilyA := hasVersionA && prefixA == prefix
		sameFamilyB := hasVersionB && prefixB == prefix
		if sameFamilyA != sameFamilyB {
			return sameFamilyA
		}
	}
	return channelLess(a, b)
}

// ChannelLess orders channel families by a trailing semantic version and
// falls back to lexical ordering for names without comparable suffixes.
func ChannelLess(a, b string) bool { return channelLess(a, b) }

// channelSuffix extracts a version after the final hyphen and the channel family preceding it.
func channelSuffix(s string) (string, *semver.Version, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '-' {
			continue
		}
		v, ok := parseChannelVersion(s[i+1:])
		if ok {
			return s[:i], v, true
		}
		break
	}
	return "", nil, false
}

// parseChannelVersion normalizes abbreviated and wildcard channel versions to a
// strict semantic version.
func parseChannelVersion(value string) (*semver.Version, bool) {
	if version, err := semver.StrictNewVersion(value); err == nil {
		return version, true
	}
	parts := strings.Split(strings.ToLower(value), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return nil, false
	}
	for i, part := range parts {
		if part == "x" || part == "*" {
			parts[i] = "0"
		}
	}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	version, err := semver.StrictNewVersion(strings.Join(parts, "."))
	return version, err == nil
}
