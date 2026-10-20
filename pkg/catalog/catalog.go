// Package catalog reads file-based operator catalogs without invoking opm.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/pkg/containertools"
	"github.com/operator-framework/operator-registry/pkg/image"
	"github.com/operator-framework/operator-registry/pkg/image/containersimageregistry"
	"go.podman.io/image/v5/types"
)

// Source identifies a catalog image. ID is a caller-defined identifier.
type Source struct {
	ID       string `yaml:"id" json:"id"`
	Image    string `yaml:"image" json:"image"`
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty"`
}

// Snapshot is intentionally compact: it contains graph metadata, not rendered CSVs.
type Snapshot struct {
	Source      Source              `json:"source"`
	GeneratedAt time.Time           `json:"generatedAt"`
	Packages    map[string]*Package `json:"packages"`
}

// Package contains the channels and bundle metadata for an operator.
type Package struct {
	Name           string              `json:"name"`
	DefaultChannel string              `json:"defaultChannel"`
	Channels       map[string]*Channel `json:"channels"`
	Bundles        map[string]*Bundle  `json:"bundles"`
}

// Channel describes a named update graph within a package.
// Deprecated reflects an explicit olm.channel reference in olm.deprecations.
type Channel struct {
	Name       string  `json:"name"`
	Entries    []Entry `json:"entries"`
	Deprecated bool    `json:"deprecated"`
}

// Entry describes a bundle and the update edges that lead to it.
type Entry struct {
	Name      string   `json:"name"`
	Replaces  string   `json:"replaces,omitempty"`
	Skips     []string `json:"skips,omitempty"`
	SkipRange string   `json:"skipRange,omitempty"`
}

// Bundle contains the release metadata retained from an OLM bundle.
// Deprecated reflects an explicit olm.bundle reference in olm.deprecations.
type Bundle struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Image      string `json:"image,omitempty"`
	Deprecated bool   `json:"deprecated"`
}

// Reader loads catalog metadata from an OCI image or a file-based catalog.
// Its zero value uses the default image signature policy and two parsing workers.
// Deprecation records are applied after all metadata is loaded, independent of
// record or file order. Only channel and bundle deprecation flags are retained;
// package-level notices and deprecation messages are not exposed.
type Reader struct {
	SignaturePolicy  string
	ParseConcurrency int
}

// Read pulls, unpacks, and streams FBC metadata. It deliberately avoids action.Render,
// which constructs a complete DeclarativeConfig including large bundle objects.
func (r Reader) Read(ctx context.Context, source Source) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source.ID == "" || source.Image == "" {
		return nil, fmt.Errorf("catalog source requires id and image")
	}
	sys := &types.SystemContext{}
	if r.SignaturePolicy != "" {
		sys.SignaturePolicyPath = r.SignaturePolicy
	}
	if source.Platform != "" {
		osChoice, architectureChoice, variantChoice, err := parsePlatform(source.Platform)
		if err != nil {
			return nil, err
		}
		sys.OSChoice = osChoice
		sys.ArchitectureChoice = architectureChoice
		sys.VariantChoice = variantChoice
	}
	registry, err := containersimageregistry.New(sys)
	if err != nil {
		return nil, fmt.Errorf("create image registry: %w", err)
	}
	defer func() { _ = registry.Destroy() }() // Best-effort cleanup after catalog acquisition.

	ref := image.SimpleReference(source.Image)
	if err := registry.Pull(ctx, ref); err != nil {
		return nil, fmt.Errorf("pull %q: %w", source.Image, err)
	}
	labels, err := registry.Labels(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("labels %q: %w", source.Image, err)
	}
	configs, ok := labels[containertools.ConfigsLocationLabel]
	if !ok {
		return nil, fmt.Errorf("%q is not a file-based catalog image", source.Image)
	}
	root, err := os.MkdirTemp("", "olm-catalog-")
	if err != nil {
		return nil, fmt.Errorf("create unpack directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }() // Best-effort cleanup of the unpacked image.
	if err := registry.Unpack(ctx, ref, root); err != nil {
		return nil, fmt.Errorf("unpack %q: %w", source.Image, err)
	}
	return r.readExtracted(ctx, source, root, configs)
}

// readExtracted confines config resolution and file reads to the unpacked image.
func (r Reader) readExtracted(ctx context.Context, source Source, root, configs string) (*Snapshot, error) {
	// Absolute-looking config paths are relative to the image filesystem.
	configPath := filepath.Clean(strings.TrimLeft(configs, "/"))
	if configPath == "." || !filepath.IsLocal(configPath) {
		return nil, fmt.Errorf("catalog config path: path traversal is not allowed")
	}
	imageRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open extraction root: %w", err)
	}
	defer func() { _ = imageRoot.Close() }()
	// Guard before Sub: fs.Sub lacks StatFS, so fs.Stat falls back to Open.
	configFS, err := fs.Sub(regularCatalogFS{imageRoot.FS()}, filepath.ToSlash(configPath))
	if err != nil {
		return nil, fmt.Errorf("catalog config path: %w", err)
	}

	return r.readFS(ctx, source, configFS)
}

// ReadFS reads an unpacked file-based catalog rooted at configs. Source is
// retained as snapshot metadata; it is not pulled or validated. Parsing uses
// the same concurrency and normalization as Read, without registry access.
// Callers are responsible for confining access through the supplied filesystem.
// Catalog files must be regular files; directories and links to regular files are allowed.
// The filesystem must implement fs.StatFS and report metadata without opening files.
func (r Reader) ReadFS(ctx context.Context, source Source, configs fs.FS) (*Snapshot, error) {
	if configs != nil {
		configs = regularCatalogFS{configs}
	}
	return r.readFS(ctx, source, configs)
}

// readFS parses a filesystem whose Open already rejects special files.
func (r Reader) readFS(ctx context.Context, source Source, configs fs.FS) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &Snapshot{Source: source, GeneratedAt: time.Now().UTC(), Packages: map[string]*Package{}}
	var mu sync.Mutex
	var deprecations []declcfg.Deprecation
	concurrency := r.ParseConcurrency
	if concurrency < 1 {
		concurrency = 2
	}
	err := declcfg.WalkMetasFS(ctx, configs, func(_ string, meta *declcfg.Meta, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		mu.Lock()
		defer mu.Unlock()
		if meta.Schema == declcfg.SchemaDeprecation {
			var record declcfg.Deprecation
			if err := json.Unmarshal(meta.Blob, &record); err != nil {
				return fmt.Errorf("parse deprecation: %w", err)
			}
			deprecations = append(deprecations, record)
			return nil
		}
		return addMeta(s, meta.Schema, meta.Blob)
	}, declcfg.WithConcurrency(concurrency))
	if err != nil {
		return nil, fmt.Errorf("read FBC: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read FBC: %w", err)
	}
	// Apply to the final objects: later metadata must not overwrite flags.
	if err := applyDeprecations(s, deprecations); err != nil {
		return nil, fmt.Errorf("read FBC: %w", err)
	}
	for _, p := range s.Packages {
		if p.DefaultChannel == "" || p.Channels[p.DefaultChannel] == nil {
			return nil, fmt.Errorf("read FBC: package %q has no default channel metadata", p.Name)
		}
		for _, ch := range p.Channels {
			for _, entry := range ch.Entries {
				if p.Bundles[entry.Name] == nil {
					return nil, fmt.Errorf("read FBC: channel %q in package %q references missing bundle %q", ch.Name, p.Name, entry.Name)
				}
			}
			sort.Slice(ch.Entries, func(i, j int) bool { return ch.Entries[i].Name < ch.Entries[j].Name })
		}
	}
	return s, nil
}

// applyDeprecations resolves package-scoped references after all files are loaded.
// Match Operator Framework reference validation without rendering full bundles.
func applyDeprecations(s *Snapshot, records []declcfg.Deprecation) error {
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Package == "" {
			return fmt.Errorf("package name must be set for deprecation record")
		}
		if s.Packages[record.Package] == nil {
			return fmt.Errorf("cannot apply deprecations to an unknown package %q", record.Package)
		}
		if seen[record.Package] {
			return fmt.Errorf("expected a maximum of one deprecation per package: %q", record.Package)
		}
		seen[record.Package] = true
	}
	for _, record := range records {
		p := s.Packages[record.Package]
		references := make(map[declcfg.PackageScopedReference]bool, len(record.Entries))
		for _, entry := range record.Entries {
			ref := entry.Reference
			if references[ref] {
				return fmt.Errorf("duplicate deprecation entry %q %q for package %q", ref.Schema, ref.Name, record.Package)
			}
			references[ref] = true
			switch ref.Schema {
			case declcfg.SchemaBundle:
				b := p.Bundles[ref.Name]
				if b == nil {
					return fmt.Errorf("cannot deprecate bundle %q for package %q: bundle not found", ref.Name, record.Package)
				}
				b.Deprecated = true
			case declcfg.SchemaChannel:
				ch := p.Channels[ref.Name]
				if ch == nil {
					return fmt.Errorf("cannot deprecate channel %q for package %q: channel not found", ref.Name, record.Package)
				}
				ch.Deprecated = true
			case declcfg.SchemaPackage:
				if ref.Name != "" {
					return fmt.Errorf("package name must be empty for deprecated package %q (specified %q)", record.Package, ref.Name)
				}
				// The compact public model retains only channel and bundle flags.
				// Package deprecation does not imply deprecation of its children.
			default:
				return fmt.Errorf("cannot deprecate object %q %q for package %q: unsupported reference schema", ref.Schema, ref.Name, record.Package)
			}
		}
	}
	return nil
}

type regularCatalogFS struct{ fs.FS }

func (f regularCatalogFS) Open(name string) (fs.File, error) {
	statter, ok := f.FS.(fs.StatFS)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("catalog filesystem must implement fs.StatFS")}
	}
	// Stat follows confined symlinks without opening a FIFO or device for reading.
	info, err := statter.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("catalog file is not regular: %s", info.Mode().Type())}
	}
	return f.FS.Open(name)
}

// parsePlatform splits an OCI platform into OS, architecture, and optional variant.
func parsePlatform(value string) (osChoice, architectureChoice, variantChoice string, err error) {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
		return "", "", "", fmt.Errorf("invalid platform %q: expected os/architecture[/variant]", value)
	}
	return parts[0], parts[1], strings.Join(parts[2:], ""), nil
}

// ValidatePlatform checks an OCI platform in os/architecture[/variant] form.
func ValidatePlatform(value string) error {
	_, _, _, err := parsePlatform(value)
	return err
}

type rawPackage struct {
	Name           string `json:"name"`
	DefaultChannel string `json:"defaultChannel"`
}
type rawChannel struct {
	Package string  `json:"package"`
	Name    string  `json:"name"`
	Entries []Entry `json:"entries"`
}
type rawBundle struct {
	Name       string        `json:"name"`
	Package    string        `json:"package"`
	Image      string        `json:"image"`
	Properties []rawProperty `json:"properties"`
}
type rawProperty struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// addMeta merges supported FBC records into the snapshot, ignoring unknown schemas.
func addMeta(s *Snapshot, schema string, blob []byte) error {
	switch schema {
	case "olm.package":
		var v rawPackage
		if err := json.Unmarshal(blob, &v); err != nil {
			return err
		}
		if v.Name == "" {
			return fmt.Errorf("package metadata has no name")
		}
		p := ensurePackage(s, v.Name)
		p.DefaultChannel = v.DefaultChannel
	case "olm.channel":
		var v rawChannel
		if err := json.Unmarshal(blob, &v); err != nil {
			return err
		}
		if v.Package == "" || v.Name == "" {
			return fmt.Errorf("channel metadata is incomplete")
		}
		p := ensurePackage(s, v.Package)
		p.Channels[v.Name] = &Channel{Name: v.Name, Entries: v.Entries}
	case "olm.bundle":
		var v rawBundle
		if err := json.Unmarshal(blob, &v); err != nil {
			return err
		}
		if v.Package == "" || v.Name == "" {
			return fmt.Errorf("bundle metadata is incomplete")
		}
		p := ensurePackage(s, v.Package)
		version, err := packageVersion(v.Properties)
		if err != nil {
			return fmt.Errorf("bundle %q: %w", v.Name, err)
		}
		p.Bundles[v.Name] = &Bundle{Name: v.Name, Version: version, Image: v.Image}
	case "":
		return fmt.Errorf("catalog metadata has no schema")
	}
	return nil
}

// ensurePackage returns the named package, creating its maps for out-of-order records.
func ensurePackage(s *Snapshot, name string) *Package {
	if p := s.Packages[name]; p != nil {
		return p
	}
	p := &Package{Name: name, Channels: map[string]*Channel{}, Bundles: map[string]*Bundle{}}
	s.Packages[name] = p
	return p
}

// packageVersion requires a version in the bundle's olm.package property.
func packageVersion(props []rawProperty) (string, error) {
	for _, p := range props {
		if p.Type != "olm.package" {
			continue
		}
		var v struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(p.Value, &v); err != nil {
			return "", fmt.Errorf("decode olm.package property: %w", err)
		}
		if v.Version != "" {
			return v.Version, nil
		}
		return "", fmt.Errorf("olm.package property has no version")
	}
	return "", fmt.Errorf("olm.package property is missing")
}
