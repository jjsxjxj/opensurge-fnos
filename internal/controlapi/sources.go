package controlapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"open-mihomo-gateway/internal/config"
	"open-mihomo-gateway/internal/mihomo"
	"open-mihomo-gateway/internal/runtime"

	"gopkg.in/yaml.v3"
)

const (
	maxSourceSize   = 10 << 20
	sourceUserAgent = "clash.meta"
)

// importURL fetches an HTTPS subscription and stores it as a new draft. The
// identity is derived from the request name and origin.
func (s *Server) importURL(ctx context.Context, req SourceImportRequest) (Source, error) {
	return s.importURLInto(ctx, sourceID(req.Name, redactURL(req.URL)), req)
}

// importURLInto is importURL with an explicit source identity. Reusing the
// existing ID lets an edit replace a source's subscription in place instead of
// appending a second library entry for the same profile.
func (s *Server) importURLInto(ctx context.Context, id string, req SourceImportRequest) (Source, error) {
	parsed, err := url.Parse(req.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return Source{}, fmt.Errorf("source URL must be an absolute HTTPS URL")
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			if next.URL.Scheme != "https" {
				return fmt.Errorf("redirected source must remain HTTPS")
			}
			return nil
		},
		Transport: &http.Transport{DialContext: safeDialContext},
	}
	httpReq, err := newSourceRequest(ctx, req.URL)
	if err != nil {
		return Source{}, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Source{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Source{}, fmt.Errorf("source returned %s", resp.Status)
	}
	before, _ := s.store.Sources()
	source, err := s.importReaderWithID(id, req.Name, req.Kind, redactURL(req.URL), io.LimitReader(resp.Body, maxSourceSize+1))
	if err != nil {
		return Source{}, err
	}
	if err := s.credentials.Put(ctx, source.ID, req.URL); err != nil {
		_ = s.store.SaveSources(before)
		// Only drop the snapshot this call created. A re-import that produced the
		// same digest reuses the file the restored record still points at, so
		// removing it unconditionally would strand that record.
		if !snapshotReferenced(before, source.SnapshotPath) {
			_ = os.Remove(source.SnapshotPath)
		}
		return Source{}, err
	}
	return source, nil
}

func newSourceRequest(ctx context.Context, sourceURL string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	// Subscription services commonly use the client User-Agent to choose the
	// response format. Identify the requested format as mihomo/Clash Meta while
	// keeping OpenSurge as the product identity everywhere else.
	request.Header.Set("User-Agent", sourceUserAgent)
	return request, nil
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return nil, fmt.Errorf("source URL resolves to a private or local address")
		}
	}
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

func redactURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "invalid-url"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// sourceID derives the stable identity of an imported source from its name and
// origin. A fresh import of the same name/origin pair resolves to the same
// record, which is how repeated imports of one subscription accumulate versions
// instead of duplicating library entries.
func sourceID(name, origin string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + origin))
	return hex.EncodeToString(sum[:8])
}

func (s *Server) importReader(name, kind, origin string, reader io.Reader) (Source, error) {
	return s.importReaderWithID(sourceID(name, origin), name, kind, origin, reader)
}

// importReaderWithID stores a document under an explicit source identity. Edits
// pass the existing ID so that renaming a source or replacing its content stays
// an in-place update: the record keeps its version history and its
// desired/applied state instead of being recreated as a second entry.
func (s *Server) importReaderWithID(id, name, kind, origin string, reader io.Reader) (Source, error) {
	if strings.TrimSpace(name) == "" {
		name = "Imported profile"
	}
	if kind == "" {
		kind = "mihomo_profile"
	}
	if kind != "mihomo_profile" && kind != "rule_provider" {
		return Source{}, fmt.Errorf("unsupported source kind %q", kind)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return Source{}, err
	}
	if len(data) == 0 {
		return Source{}, fmt.Errorf("source is empty")
	}
	if len(data) > maxSourceSize {
		return Source{}, fmt.Errorf("source exceeds 10 MiB limit")
	}
	inventory, err := inspectSource(data, kind)
	valid := err == nil
	validation := "structural validation passed; apply runs mihomo engine validation"
	if err != nil {
		validation = err.Error()
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	dir := filepath.Join(s.store.Dir(), "sources", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Source{}, err
	}
	path := filepath.Join(dir, digest+".yaml")
	if err := writeAtomic(path, data, 0o600); err != nil {
		return Source{}, err
	}
	source := Source{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Name:          name,
		Kind:          kind,
		Origin:        origin,
		SnapshotPath:  path,
		Digest:        digest,
		Size:          int64(len(data)),
		Valid:         valid,
		Validation:    validation,
		Inventory:     inventory,
		ImportedAt:    time.Now().UTC(),
		Versions:      []SourceVersion{},
		Diff:          emptySourceDiff(),
	}
	if strings.HasPrefix(origin, "https://") {
		source.Origin = redactURL(origin)
	}
	sources, loadErr := s.store.Sources()
	if loadErr != nil {
		return Source{}, loadErr
	}
	sources = s.decorateSourceStates(sources)
	for i := range sources {
		if sources[i].ID == source.ID {
			previous := sources[i]
			if previous.Digest == source.Digest {
				source.Versions = append([]SourceVersion{}, previous.Versions...)
				source.Desired = previous.Desired
				source.Applied = previous.Applied
				source.Diff.PreviousDigest = previous.Digest
			} else {
				source.Versions = append(append([]SourceVersion{}, previous.Versions...), sourceVersion(previous))
				source.Diff = diffInventory(previous.Digest, previous.Inventory, source.Inventory)
			}
			sources[i] = source
			return source, s.store.SaveSources(sources)
		}
	}
	sources = append(sources, source)
	return source, s.store.SaveSources(sources)
}

func sourceVersion(source Source) SourceVersion {
	return SourceVersion{Digest: source.Digest, Size: source.Size, Valid: source.Valid, Validation: source.Validation, Inventory: source.Inventory, ImportedAt: source.ImportedAt, Desired: source.Desired, Applied: source.Applied, SnapshotPath: source.SnapshotPath}
}

func (s *Server) decorateSourceStates(sources []Source) []Source {
	desired, applied := s.profileDigests()
	result := append([]Source(nil), sources...)
	for i := range result {
		result[i].Versions = append([]SourceVersion(nil), result[i].Versions...)
		result[i].Desired = desired != "" && result[i].Digest == desired
		result[i].Applied = applied != "" && result[i].Digest == applied
		for j := range result[i].Versions {
			result[i].Versions[j].Desired = desired != "" && result[i].Versions[j].Digest == desired
			result[i].Versions[j].Applied = applied != "" && result[i].Versions[j].Digest == applied
		}
	}
	return result
}

func (s *Server) profileDigests() (string, string) {
	cfg, err := config.LoadRuntime(s.configPath)
	if err != nil {
		return "", ""
	}
	desired, _ := config.MihomoProfileDigest(cfg)
	applied := ""
	if state, exists, _ := runtime.LoadState(runtime.NewPaths(cfg).StateFile); exists {
		applied = state.ProfileDigest
	}
	return desired, applied
}

func emptySourceDiff() SourceDiff {
	return SourceDiff{ProxiesAdded: []string{}, ProxiesRemoved: []string{}, GroupsAdded: []string{}, GroupsRemoved: []string{}, ProxyProvidersAdded: []string{}, ProxyProvidersRemoved: []string{}, RuleProvidersAdded: []string{}, RuleProvidersRemoved: []string{}}
}

func emptyInventory() Inventory {
	return Inventory{
		Proxies:        []string{},
		ProxyProviders: []string{},
		ProxyGroups:    []string{},
		RuleProviders:  []string{},
		Warnings:       []string{},
	}
}

func normalizeInventory(inventory Inventory) Inventory {
	if inventory.Proxies == nil {
		inventory.Proxies = []string{}
	}
	if inventory.ProxyProviders == nil {
		inventory.ProxyProviders = []string{}
	}
	if inventory.ProxyGroups == nil {
		inventory.ProxyGroups = []string{}
	}
	if inventory.RuleProviders == nil {
		inventory.RuleProviders = []string{}
	}
	if inventory.Warnings == nil {
		inventory.Warnings = []string{}
	}
	return inventory
}

func diffInventory(previousDigest string, before, after Inventory) SourceDiff {
	diff := emptySourceDiff()
	diff.PreviousDigest = previousDigest
	diff.ProxiesAdded, diff.ProxiesRemoved = diffNames(before.Proxies, after.Proxies)
	diff.GroupsAdded, diff.GroupsRemoved = diffNames(before.ProxyGroups, after.ProxyGroups)
	diff.ProxyProvidersAdded, diff.ProxyProvidersRemoved = diffNames(before.ProxyProviders, after.ProxyProviders)
	diff.RuleProvidersAdded, diff.RuleProvidersRemoved = diffNames(before.RuleProviders, after.RuleProviders)
	diff.RuleCountDelta = after.RuleCount - before.RuleCount
	return diff
}

func diffNames(before, after []string) (added, removed []string) {
	left, right := map[string]bool{}, map[string]bool{}
	for _, value := range before {
		left[value] = true
	}
	for _, value := range after {
		right[value] = true
	}
	for value := range right {
		if !left[value] {
			added = append(added, value)
		}
	}
	for value := range left {
		if !right[value] {
			removed = append(removed, value)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if added == nil {
		added = []string{}
	}
	if removed == nil {
		removed = []string{}
	}
	return added, removed
}

func inspectSource(data []byte, kind string) (Inventory, error) {
	inv := emptyInventory()
	if kind == "mihomo_profile" {
		inspection, err := mihomo.InspectImportedProfile(data)
		if err != nil {
			return inv, fmt.Errorf("parse mihomo profile: %w", err)
		}
		inv.Proxies = inspection.Proxies
		inv.ProxyProviders = inspection.ProxyProviders
		inv.ProxyGroups = inspection.ProxyGroups
		inv.RuleProviders = inspection.RuleProviders
		inv.RuleCount = inspection.RuleCount
		inv.TerminalMatch = inspection.TerminalMatch
		inv.Warnings = inspection.Warnings
		targets := append(append([]string{}, inv.Proxies...), inv.ProxyGroups...)
		for _, name := range append(targets, inv.RuleProviders...) {
			if strings.HasPrefix(name, "device/") || strings.HasPrefix(name, "open-surge-ruleset-") || strings.HasPrefix(name, mihomo.LocalRoutingGroupPrefix) {
				return inv, fmt.Errorf("imported source uses reserved OpenSurge name %q", name)
			}
		}
		return inv, nil
	}

	var document yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&document); err != nil {
		return inv, fmt.Errorf("parse YAML: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return inv, fmt.Errorf("top-level YAML must be a mapping")
	}
	root := document.Content[0]
	sections := map[string]*yaml.Node{}
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i]
		if _, exists := sections[key.Value]; exists {
			return inv, fmt.Errorf("duplicate top-level section %q", key.Value)
		}
		sections[key.Value] = root.Content[i+1]
	}
	inv.Proxies = sequenceNames(sections["proxies"])
	inv.ProxyGroups = sequenceNames(sections["proxy-groups"])
	inv.ProxyProviders = mappingKeys(sections["proxy-providers"])
	inv.RuleProviders = mappingKeys(sections["rule-providers"])
	targets := append(append([]string{}, inv.Proxies...), inv.ProxyGroups...)
	for _, name := range append(targets, inv.RuleProviders...) {
		if strings.HasPrefix(name, "device/") || strings.HasPrefix(name, "open-surge-ruleset-") || strings.HasPrefix(name, mihomo.LocalRoutingGroupPrefix) {
			return inv, fmt.Errorf("imported source uses reserved OpenSurge name %q", name)
		}
	}
	return inv, nil
}

func sequenceNames(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.SequenceNode {
		return []string{}
	}
	var names []string
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i < len(item.Content); i += 2 {
			if item.Content[i].Value == "name" && item.Content[i+1].Kind == yaml.ScalarNode {
				names = append(names, item.Content[i+1].Value)
			}
		}
	}
	sort.Strings(names)
	return names
}

func mappingKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return []string{}
	}
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		keys = append(keys, node.Content[i].Value)
	}
	sort.Strings(keys)
	return keys
}

// sourceSnapshotPaths lists every snapshot file a record owns. The current
// document and all retained versions live side by side in one per-source
// directory.
func sourceSnapshotPaths(source Source) []string {
	paths := []string{}
	seen := map[string]bool{}
	appendPath := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	appendPath(source.SnapshotPath)
	for _, version := range source.Versions {
		appendPath(version.SnapshotPath)
	}
	return paths
}

func snapshotReferenced(sources []Source, path string) bool {
	if path == "" {
		return false
	}
	for _, source := range sources {
		for _, candidate := range sourceSnapshotPaths(source) {
			if candidate == path {
				return true
			}
		}
	}
	return false
}

// sourceDocument returns the stored document text so the Web UI can offer it for
// editing.
func (s *Server) sourceDocument(source Source) (string, error) {
	if source.SnapshotPath == "" {
		return "", fmt.Errorf("source %q has no stored document", source.ID)
	}
	data, err := os.ReadFile(source.SnapshotPath)
	if err != nil {
		return "", fmt.Errorf("read source document: %w", err)
	}
	return string(data), nil
}

// renameSource changes the display name of an existing record. The ID is left
// untouched on purpose: it names the snapshot directory and keys the stored
// subscription credential, so rewriting it would orphan both. The trade-off is
// that a later import of the same subscription under the new name derives a
// different ID and therefore creates a separate entry.
func (s *Server) renameSource(id, name string) error {
	sources, err := s.store.Sources()
	if err != nil {
		return err
	}
	for i := range sources {
		if sources[i].ID == id {
			sources[i].Name = name
			return s.store.SaveSources(sources)
		}
	}
	return fmt.Errorf("source %q not found", id)
}

// errSourceNotRemoved marks the failure of the authoritative step of a delete.
// The caller uses it to tell "nothing happened" apart from "the record is gone
// but leftover files remain", which need different messages.
var errSourceNotRemoved = errors.New("source record could not be removed")

// deleteSource removes a record together with the snapshots it owns and the
// saved subscription credential. The profile the gateway actually runs is a
// separate copy under data/imported-profile-*.yaml, so removing a source never
// takes the running configuration's profile away. The handlers still refuse to
// delete the applied source so the library cannot silently lose the origin of
// what is currently running.
func (s *Server) deleteSource(ctx context.Context, source Source) error {
	sources, err := s.store.Sources()
	if err != nil {
		return fmt.Errorf("%w: %v", errSourceNotRemoved, err)
	}
	remaining := make([]Source, 0, len(sources))
	for _, candidate := range sources {
		if candidate.ID != source.ID {
			remaining = append(remaining, candidate)
		}
	}
	// The record removal is the visible part of a delete, so it is committed
	// first: a cleanup failure below must not resurrect an entry the operator
	// has already removed from the library.
	if err := s.store.SaveSources(remaining); err != nil {
		return fmt.Errorf("%w: %v", errSourceNotRemoved, err)
	}
	failures := []error{}
	if err := s.credentials.Delete(ctx, source.ID); err != nil {
		failures = append(failures, fmt.Errorf("delete saved subscription link: %w", err))
	}
	for _, path := range sourceSnapshotPaths(source) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("delete snapshot %s: %w", filepath.Base(path), err))
		}
	}
	// Snapshots of one source share a directory, so dropping it also removes any
	// file the per-path loop above could not name. The guard keeps a malformed
	// record from turning into a recursive delete outside the per-source area
	// under "sources".
	if source.SnapshotPath != "" {
		dir := filepath.Dir(source.SnapshotPath)
		if filepath.Base(filepath.Dir(dir)) == "sources" {
			if err := os.RemoveAll(dir); err != nil {
				failures = append(failures, fmt.Errorf("delete snapshot directory: %w", err))
			}
		}
	}
	return errors.Join(failures...)
}
