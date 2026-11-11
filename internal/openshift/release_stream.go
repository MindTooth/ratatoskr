package openshift

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/blang/semver/v4"
)

const maxReleaseControllerBytes = 8 << 20

type releaseControllerTags struct {
	Tags []releaseControllerTag `json:"tags"`
}

type releaseControllerTag struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

type releaseStreamRelease struct {
	version string
}

// addReleaseStreamHistory adds accepted releases that Cincinnati omits as
// deprecated changelog-only entries. Graph releases stay eligible and retain
// their graph metadata; advisory content is fetched separately for all entries.
func (c Client) addReleaseStreamHistory(ctx context.Context, architecture, currentVersion string, advisoryURLs map[string]string, releases []Release) ([]Release, error) {
	if len(releases) < 2 {
		return releases, nil
	}
	// A graph override is often a private or test graph. It does not imply that
	// the public release-controller stream describes the same releases.
	if c.GraphURL != "" && c.GraphURL != DefaultGraphURL && c.ReleaseControllerURL == "" {
		return releases, nil
	}

	ctx, cancel := context.WithTimeout(ctx, changelogFetchTimeout)
	defer cancel()
	baseURL, stream, ok := c.releaseControllerSource(architecture)
	if !ok {
		return releases, nil
	}
	tags, err := c.fetchReleaseControllerTags(ctx, baseURL, stream)
	if err != nil {
		return nil, err
	}

	limit := latestVersion(releases)
	entries := streamReleasesBetween(tags, currentVersion, limit)
	if len(entries) == 0 {
		return releases, nil
	}

	positions := make(map[string]int, len(releases))
	for i := range releases {
		positions[releases[i].Version] = i
	}
	for _, entry := range entries {
		if _, found := positions[entry.version]; found {
			continue
		}
		releases = append(releases, Release{
			Version:      entry.version,
			IsDeprecated: true,
			ChangelogURL: advisoryURLs[entry.version],
		})
	}
	return releases, nil
}

func (c Client) releaseControllerSource(architecture string) (baseURL, stream string, ok bool) {
	baseURL = strings.TrimRight(c.ReleaseControllerURL, "/")
	if baseURL == "" {
		baseURL = DefaultReleaseControllerURL
	}
	switch architecture {
	case "multi":
		return baseURL, "4-stable-multi", true
	case "amd64":
		if c.ReleaseControllerURL == "" {
			baseURL = "https://amd64.ocp.releases.ci.openshift.org"
		}
		return baseURL, "4-stable", true
	case "arm64":
		if c.ReleaseControllerURL == "" {
			baseURL = "https://arm64.ocp.releases.ci.openshift.org"
		}
		return baseURL, "4-stable-arm64", true
	case "ppc64le":
		if c.ReleaseControllerURL == "" {
			baseURL = "https://ppc64le.ocp.releases.ci.openshift.org"
		}
		return baseURL, "4-stable-ppc64le", true
	case "s390x":
		if c.ReleaseControllerURL == "" {
			baseURL = "https://s390x.ocp.releases.ci.openshift.org"
		}
		return baseURL, "4-stable-s390x", true
	default:
		return "", "", false
	}
}

// ReleaseComparisonURL links the installed release to the highest returned
// update without embedding release-controller changelog content.
func (c Client) ReleaseComparisonURL(architecture, from, to string) string {
	if c.GraphURL != "" && c.GraphURL != DefaultGraphURL && c.ReleaseControllerURL == "" {
		return ""
	}
	if from == "" || to == "" || from == to {
		return ""
	}
	baseURL, stream, ok := c.releaseControllerSource(architecture)
	if !ok {
		return ""
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/releasestream/" + url.PathEscape(stream) + "/release/" + url.PathEscape(to))
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("from", from)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c Client) fetchReleaseControllerTags(ctx context.Context, baseURL, stream string) ([]releaseControllerTag, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/releasestream/" + url.PathEscape(stream) + "/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create OpenShift release stream request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenShift release stream: %w", err)
	}
	defer func() { _ = res.Body.Close() }() // Closing a response body does not affect the read result.
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch OpenShift release stream: unexpected HTTP status %s", res.Status)
	}
	var payload releaseControllerTags
	if err := decodeReleaseData(res.Body, maxReleaseControllerBytes, &payload); err != nil {
		return nil, fmt.Errorf("decode OpenShift release stream: %w", err)
	}
	if payload.Tags == nil {
		return nil, fmt.Errorf("decode OpenShift release stream: tags are missing or null")
	}
	for _, tag := range payload.Tags {
		if tag.Name == "" || tag.Phase == "" {
			return nil, fmt.Errorf("decode OpenShift release stream: tag metadata is incomplete")
		}
	}
	return payload.Tags, nil
}

func streamReleasesBetween(tags []releaseControllerTag, from, to string) []releaseStreamRelease {
	fromVersion, fromErr := semver.Parse(from)
	toVersion, toErr := semver.Parse(to)
	if fromErr != nil || toErr != nil || !fromVersion.LT(toVersion) {
		return nil
	}
	accepted := make([]string, 0, len(tags))
	for _, tag := range tags {
		version, err := semver.Parse(tag.Name)
		if tag.Phase == "Accepted" && err == nil && len(version.Pre) == 0 {
			accepted = append(accepted, tag.Name)
		}
	}
	sort.Slice(accepted, func(i, j int) bool {
		left, _ := semver.Parse(accepted[i])
		right, _ := semver.Parse(accepted[j])
		return left.LT(right)
	})

	entries := make([]releaseStreamRelease, 0, len(accepted))
	for _, tag := range accepted {
		version, _ := semver.Parse(tag)
		if fromVersion.LT(version) && !toVersion.LT(version) {
			entries = append(entries, releaseStreamRelease{version: tag})
		}
	}
	return entries
}

func latestVersion(releases []Release) string {
	latest := releases[0].Version
	for _, release := range releases[1:] {
		left, leftErr := semver.Parse(latest)
		right, rightErr := semver.Parse(release.Version)
		if leftErr == nil && rightErr == nil && left.LT(right) {
			latest = release.Version
		}
	}
	return latest
}
