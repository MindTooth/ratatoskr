package openshift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/blang/semver/v4"
)

const (
	DefaultGraphURL             = "https://api.openshift.com/api/upgrades_info/v1/graph"
	DefaultReleaseControllerURL = "https://multi.ocp.releases.ci.openshift.org"
	maxGraphBytes               = 32 << 20
	manifestRefKey              = "io.openshift.upgrades.graph.release.manifestref"
)

var ErrCurrentVersionNotFound = errors.New("current release is not present in channel")

type Client struct {
	GraphURL             string
	ErrataURL            string
	ReleaseControllerURL string
	HTTPClient           *http.Client
	ChangelogCache       *ChangelogCache
}

type UpdateRequest struct {
	Channel        string
	Architecture   string
	CurrentVersion string
	Lag            int
}

type Release struct {
	Version          string `json:"version"`
	IsDeprecated     bool   `json:"isDeprecated,omitempty"`
	ChangelogContent string `json:"changelogContent,omitempty"`
	ChangelogURL     string `json:"changelogUrl,omitempty"`
	Digest           string `json:"digest,omitempty"`
}

type graph struct {
	Nodes []node   `json:"nodes"`
	Edges [][]*int `json:"edges"`
}

type node struct {
	Version  string            `json:"version"`
	Metadata map[string]string `json:"metadata"`
}

// Updates returns the installed release and its direct, unconditional graph
// successors. Conditional edges require cluster state and are deliberately not
// considered by this datasource.
func (c Client) Updates(ctx context.Context, req UpdateRequest) ([]Release, error) {
	if strings.TrimSpace(req.Channel) == "" {
		return nil, errors.New("channel is required")
	}
	if strings.TrimSpace(req.Architecture) == "" {
		return nil, errors.New("architecture is required")
	}
	if strings.TrimSpace(req.CurrentVersion) == "" {
		return nil, errors.New("currentVersion is required")
	}
	if req.Lag < 0 {
		return nil, errors.New("lag must not be negative")
	}

	graphURL := c.GraphURL
	if graphURL == "" {
		graphURL = DefaultGraphURL
	}
	u, err := url.Parse(graphURL)
	if err != nil {
		return nil, fmt.Errorf("parse graph URL: %w", err)
	}
	q := u.Query()
	q.Set("channel", req.Channel)
	q.Set("arch", req.Architecture)
	u.RawQuery = q.Encode()

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create graph request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	res, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenShift update graph: %w", err)
	}
	defer func() { _ = res.Body.Close() }() // Closing a response body does not affect the read result.
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("fetch OpenShift update graph: unexpected HTTP status %s", res.Status)
	}

	var g graph
	if err := decodeReleaseData(res.Body, maxGraphBytes, &g); err != nil {
		return nil, fmt.Errorf("decode OpenShift update graph: %w", err)
	}
	if g.Nodes == nil || g.Edges == nil {
		return nil, fmt.Errorf("decode OpenShift update graph: nodes or edges are missing or null")
	}
	versions := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.Version == "" {
			return nil, fmt.Errorf("decode OpenShift update graph: node version is missing")
		}
		if versions[n.Version] {
			return nil, fmt.Errorf("decode OpenShift update graph: duplicate node version %q", n.Version)
		}
		versions[n.Version] = true
	}
	for _, edge := range g.Edges {
		if len(edge) != 2 || edge[0] == nil || edge[1] == nil {
			return nil, fmt.Errorf("decode OpenShift update graph: edge must contain two indexes")
		}
		if *edge[0] < 0 || *edge[0] >= len(g.Nodes) || *edge[1] < 0 || *edge[1] >= len(g.Nodes) {
			return nil, fmt.Errorf("decode OpenShift update graph: edge index is out of range")
		}
	}

	current := -1
	for i := range g.Nodes {
		if g.Nodes[i].Version == req.CurrentVersion {
			current = i
			break
		}
	}
	if current < 0 {
		return nil, ErrCurrentVersionNotFound
	}

	targetIndexes := map[int]bool{}
	for _, edge := range g.Edges {
		if *edge[0] == current && *edge[1] != current {
			targetIndexes[*edge[1]] = true
		}
	}

	targets := make([]Release, 0, len(targetIndexes))
	for index := range targetIndexes {
		targets = append(targets, releaseFromNode(g.Nodes[index]))
	}
	sortReleases(targets)
	if req.Lag >= len(targets) {
		targets = targets[:0]
	} else if req.Lag > 0 {
		targets = targets[:len(targets)-req.Lag]
	}

	out := append([]Release{releaseFromNode(g.Nodes[current])}, targets...)
	advisoryURLs := make(map[string]string, len(g.Nodes))
	for _, node := range g.Nodes {
		if _, ok := advisoryIDFromURL(node.Metadata["url"]); ok {
			advisoryURLs[node.Version] = node.Metadata["url"]
		}
	}
	out, err = c.addReleaseStreamHistory(ctx, req.Architecture, req.CurrentVersion, advisoryURLs, out)
	if err != nil {
		return nil, err
	}
	sortReleases(out)
	c.enrichChangelogs(ctx, out[1:])
	return out, nil
}

// decodeReleaseData requires one complete JSON document within the size limit.
// Reading the full body also catches transport failures after a valid JSON prefix.
func decodeReleaseData(body io.Reader, limit int64, value any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("response exceeds %d bytes", limit)
	}
	return json.Unmarshal(data, value)
}

func releaseFromNode(n node) Release {
	return Release{
		Version:      n.Version,
		ChangelogURL: n.Metadata["url"],
		Digest:       n.Metadata[manifestRefKey],
	}
}

func sortReleases(releases []Release) {
	sort.SliceStable(releases, func(i, j int) bool {
		left, leftErr := semver.Parse(releases[i].Version)
		right, rightErr := semver.Parse(releases[j].Version)
		if leftErr == nil && rightErr == nil {
			return left.LT(right)
		}
		return releases[i].Version < releases[j].Version
	})
}
