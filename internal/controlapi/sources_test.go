package controlapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open-mihomo-gateway/internal/config"
	"open-mihomo-gateway/internal/runtime"
)

const testProfile = "proxies:\n  - {name: node-a, type: direct}\nproxy-groups:\n  - {name: Main, type: select, proxies: [DIRECT]}\nrules:\n  - MATCH,DIRECT\n"

func importTestSource(t *testing.T, server *Server, name, origin, document string) Source {
	t.Helper()
	source, err := server.importReader(name, "mihomo_profile", origin, strings.NewReader(document))
	if err != nil {
		t.Fatalf("import source: %v", err)
	}
	return source
}

// markSourceApplied reproduces a running gateway that uses the given source: the
// desired config selects its profile and the runtime state records the applied
// digest.
func markSourceApplied(t *testing.T, server *Server, source Source) {
	t.Helper()
	cfg, err := config.LoadRuntime(server.configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mihomo.ProfileMode = config.MihomoProfileModeImported
	cfg.Mihomo.Profile = source.SnapshotPath
	if err := os.WriteFile(server.configPath, []byte(config.Render(cfg)), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := runtime.NewPaths(cfg)
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SaveState(paths.StateFile, runtime.State{ProfileDigest: source.Digest, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// markSourceDesiredOnly leaves the profile selected for the next start with no
// gateway running, which is the state a stop produces.
func markSourceDesiredOnly(t *testing.T, server *Server, source Source) {
	t.Helper()
	markSourceApplied(t, server, source)
	cfg, err := config.LoadRuntime(server.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.RemoveState(runtime.NewPaths(cfg).StateFile); err != nil {
		t.Fatal(err)
	}
}

func decodeSource(t *testing.T, body []byte) Source {
	t.Helper()
	var source Source
	if err := json.Unmarshal(body, &source); err != nil {
		t.Fatalf("decode source: %v body=%s", err, body)
	}
	return source
}

func TestImportReaderKeepsDerivingStableIdentity(t *testing.T) {
	server := newTestServer(t)
	first := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	second := importTestSource(t, server, "home", "file:home.yaml", strings.Replace(testProfile, "node-a", "node-c", 1))
	if first.ID != second.ID {
		t.Fatalf("repeated import changed the identity: %s -> %s", first.ID, second.ID)
	}
	if got := sourceID("home", "file:home.yaml"); got != first.ID {
		t.Fatalf("sourceID = %s want %s", got, first.ID)
	}
	if stored, err := server.store.Sources(); err != nil || len(stored) != 1 {
		t.Fatalf("stored sources = %#v err=%v", stored, err)
	}
}

func TestSourceEditReturnsSubscriptionLinkAndDocument(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "一元机场", "https://example.com/profile", testProfile)
	if err := server.credentials.Put(t.Context(), source.ID, "https://example.com/profile?token=secret"); err != nil {
		t.Fatal(err)
	}
	response := performAuthorized(server, http.MethodGet, "/api/v1/sources/"+source.ID+"/edit", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", response.Code, response.Body.String())
	}
	var edit SourceEditDocument
	if err := json.Unmarshal(response.Body.Bytes(), &edit); err != nil {
		t.Fatal(err)
	}
	if edit.ID != source.ID || edit.Name != "一元机场" || edit.Kind != "mihomo_profile" || edit.Digest != source.Digest {
		t.Fatalf("edit metadata = %#v", edit)
	}
	if edit.URL != "https://example.com/profile?token=secret" {
		t.Fatalf("edit subscription link = %q", edit.URL)
	}
	if edit.Document != testProfile {
		t.Fatalf("edit document = %q", edit.Document)
	}
}

func TestSourceEditOmitsSubscriptionForLocalDocument(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	response := performAuthorized(server, http.MethodGet, "/api/v1/sources/"+source.ID+"/edit", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", response.Code, response.Body.String())
	}
	var edit SourceEditDocument
	if err := json.Unmarshal(response.Body.Bytes(), &edit); err != nil {
		t.Fatal(err)
	}
	if edit.URL != "" {
		t.Fatalf("local source exposed a subscription link: %q", edit.URL)
	}
}

func TestSourceEditUnknownSourceReturnsNotFound(t *testing.T) {
	server := newTestServer(t)
	response := performAuthorized(server, http.MethodGet, "/api/v1/sources/missing/edit", nil)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "source_not_found") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSourceUpdateRenamesInPlaceAndKeepsVersions(t *testing.T) {
	server := newTestServer(t)
	first := importTestSource(t, server, "旧名字", "file:home.yaml", testProfile)
	markSourceApplied(t, server, first)
	second := importTestSource(t, server, "旧名字", "file:home.yaml", strings.Replace(testProfile, "node-a", "node-b", 1))
	if second.ID != first.ID || len(second.Versions) != 1 || !second.Versions[0].Applied {
		t.Fatalf("setup source = %#v", second)
	}

	response := performAuthorized(server, http.MethodPut, "/api/v1/sources/"+first.ID, []byte(`{"name":"新名字"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", response.Code, response.Body.String())
	}
	updated := decodeSource(t, response.Body.Bytes())
	if updated.ID != first.ID {
		t.Fatalf("rename changed the source identity: %s -> %s", first.ID, updated.ID)
	}
	if updated.Name != "新名字" {
		t.Fatalf("rename did not apply: %#v", updated)
	}
	if len(updated.Versions) != 1 || !updated.Versions[0].Applied || updated.Versions[0].Digest != first.Digest {
		t.Fatalf("rename dropped the version history: %#v", updated.Versions)
	}
	if updated.SnapshotPath != "" || updated.FetchURL != "" {
		t.Fatalf("update response leaked private fields: %#v", updated)
	}
	stored, err := server.store.Sources()
	if err != nil || len(stored) != 1 || stored[0].Name != "新名字" {
		t.Fatalf("stored sources = %#v err=%v", stored, err)
	}
}

func TestSourceUpdateKeepsCurrentNameWhenNameIsEmpty(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	body, err := json.Marshal(SourceUpdateRequest{Document: strings.Replace(testProfile, "node-a", "node-d", 1)})
	if err != nil {
		t.Fatal(err)
	}
	response := performAuthorized(server, http.MethodPut, "/api/v1/sources/"+source.ID, body)
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	updated := decodeSource(t, response.Body.Bytes())
	if updated.Name != "home" || updated.ID != source.ID {
		t.Fatalf("update = %#v", updated)
	}
	if updated.Digest == source.Digest {
		t.Fatal("document update did not replace the snapshot")
	}
	if len(updated.Versions) != 1 || updated.Versions[0].Digest != source.Digest {
		t.Fatalf("previous document was not retained as a version: %#v", updated.Versions)
	}
}

func TestSourceUpdateStoresInvalidDocumentAsDraft(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	body, err := json.Marshal(SourceUpdateRequest{Name: "home", Document: "proxies: [unterminated\n"})
	if err != nil {
		t.Fatal(err)
	}
	response := performAuthorized(server, http.MethodPut, "/api/v1/sources/"+source.ID, body)
	if response.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	updated := decodeSource(t, response.Body.Bytes())
	if updated.Valid || updated.Validation == "" {
		t.Fatalf("broken document was reported as valid: %#v", updated)
	}
	if updated.ID != source.ID || updated.Name != "home" {
		t.Fatalf("broken document changed the record identity: %#v", updated)
	}
	// An invalid draft must stay inapplicable.
	apply := performAuthorized(server, http.MethodPost, "/api/v1/sources/"+source.ID+"/apply", nil)
	if apply.Code != http.StatusConflict || !strings.Contains(apply.Body.String(), "source_not_applicable") {
		t.Fatalf("apply of an invalid draft status=%d body=%s", apply.Code, apply.Body.String())
	}
}

func TestSourceUpdateRejectsReplacementSubscriptionWhenFetchFails(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "https://example.com/profile", testProfile)
	if err := server.credentials.Put(t.Context(), source.ID, "https://example.com/profile?token=secret"); err != nil {
		t.Fatal(err)
	}
	// A plain HTTP URL is refused before any fetch is attempted.
	body, err := json.Marshal(SourceUpdateRequest{Name: "home", URL: "http://192.168.1.10/profile"})
	if err != nil {
		t.Fatal(err)
	}
	response := performAuthorized(server, http.MethodPut, "/api/v1/sources/"+source.ID, body)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "source_update_failed") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if value, err := server.credentials.Get(t.Context(), source.ID); err != nil || value != "https://example.com/profile?token=secret" {
		t.Fatalf("failed update changed the saved link: %q err=%v", value, err)
	}
	stored, err := server.store.Sources()
	if err != nil || len(stored) != 1 || stored[0].Digest != source.Digest || stored[0].Name != "home" {
		t.Fatalf("failed update changed the record: %#v err=%v", stored, err)
	}
}

func TestSourceUpdateRejectsUnknownFields(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	response := performAuthorized(server, http.MethodPut, "/api/v1/sources/"+source.ID, []byte(`{"name":"home","unexpected":true}`))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSourceDeleteRemovesRecordSnapshotsAndCredential(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "一元机场", "https://example.com/profile", testProfile)
	if err := server.credentials.Put(t.Context(), source.ID, "https://example.com/profile?token=secret"); err != nil {
		t.Fatal(err)
	}
	snapshotDirectory := filepath.Dir(source.SnapshotPath)

	response := performAuthorized(server, http.MethodDelete, "/api/v1/sources/"+source.ID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Revision string   `json:"revision"`
		Sources  []Source `json:"sources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sources) != 0 || payload.Revision == "" {
		t.Fatalf("delete response = %#v", payload)
	}
	if stored, err := server.store.Sources(); err != nil || len(stored) != 0 {
		t.Fatalf("stored sources = %#v err=%v", stored, err)
	}
	if _, err := os.Stat(source.SnapshotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot survived the delete: %v", err)
	}
	if _, err := os.Stat(snapshotDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory survived the delete: %v", err)
	}
	if _, err := server.credentials.Get(t.Context(), source.ID); err == nil {
		t.Fatal("saved subscription link survived the delete")
	}
}

func TestSourceDeleteKeepsUnrelatedSources(t *testing.T) {
	server := newTestServer(t)
	keep := importTestSource(t, server, "keep", "file:keep.yaml", testProfile)
	drop := importTestSource(t, server, "drop", "file:drop.yaml", testProfile)

	response := performAuthorized(server, http.MethodDelete, "/api/v1/sources/"+drop.ID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	stored, err := server.store.Sources()
	if err != nil || len(stored) != 1 || stored[0].ID != keep.ID {
		t.Fatalf("stored sources = %#v err=%v", stored, err)
	}
	if _, err := os.Stat(keep.SnapshotPath); err != nil {
		t.Fatalf("unrelated snapshot was removed: %v", err)
	}
}

func TestSourceDeleteRefusesAppliedSourceUntilGatewayStops(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	markSourceApplied(t, server, source)

	response := performAuthorized(server, http.MethodDelete, "/api/v1/sources/"+source.ID, nil)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "source_in_use") {
		t.Fatalf("applied delete status=%d body=%s", response.Code, response.Body.String())
	}
	if stored, err := server.store.Sources(); err != nil || len(stored) != 1 {
		t.Fatalf("refused delete removed the source: %#v err=%v", stored, err)
	}

	// Stopping the gateway clears the applied marker, which is the documented
	// path to deleting the source that was running.
	cfg, err := config.LoadRuntime(server.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.RemoveState(runtime.NewPaths(cfg).StateFile); err != nil {
		t.Fatal(err)
	}
	after := performAuthorized(server, http.MethodDelete, "/api/v1/sources/"+source.ID, nil)
	if after.Code != http.StatusOK {
		t.Fatalf("delete after stop status=%d body=%s", after.Code, after.Body.String())
	}
	if stored, err := server.store.Sources(); err != nil || len(stored) != 0 {
		t.Fatalf("source survived the delete: %#v err=%v", stored, err)
	}
}

func TestSourceDeleteAllowsDesiredSource(t *testing.T) {
	server := newTestServer(t)
	source := importTestSource(t, server, "home", "file:home.yaml", testProfile)
	markSourceDesiredOnly(t, server, source)
	current, err := server.decoratedSourceByID(source.ID)
	if err != nil || !current.Desired || current.Applied {
		t.Fatalf("setup source state = %#v err=%v", current, err)
	}

	response := performAuthorized(server, http.MethodDelete, "/api/v1/sources/"+source.ID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("desired delete status=%d body=%s", response.Code, response.Body.String())
	}
	if stored, err := server.store.Sources(); err != nil || len(stored) != 0 {
		t.Fatalf("source survived the delete: %#v err=%v", stored, err)
	}
}

func TestSourceDeleteUnknownSourceReturnsNotFound(t *testing.T) {
	server := newTestServer(t)
	response := performAuthorized(server, http.MethodDelete, "/api/v1/sources/missing", nil)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "source_not_found") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSourceDeleteIgnoresUnknownSourcePathInRecord(t *testing.T) {
	server := newTestServer(t)
	if err := server.store.SaveSources([]Source{{ID: "orphan", Name: "orphan", SnapshotPath: ""}}); err != nil {
		t.Fatal(err)
	}
	source, err := server.sourceByID("orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.deleteSource(t.Context(), source); err != nil {
		t.Fatalf("delete without a snapshot must succeed: %v", err)
	}
	if stored, err := server.store.Sources(); err != nil || len(stored) != 0 {
		t.Fatalf("stored sources = %#v err=%v", stored, err)
	}
}
