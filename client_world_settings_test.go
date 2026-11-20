package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func configureWorldSnowTestPackage(t *testing.T, store *Store, composition []byte) (string, string) {
	t.Helper()
	store.appxPackageDir = filepath.Join(t.TempDir(), "AppxPackageExtracted")
	dataDir := filepath.Join(store.appxPackageDir, "bundle", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	compositionPath := filepath.Join(dataDir, "env_playerLand.composition.json")
	if err := os.WriteFile(compositionPath, composition, 0o644); err != nil {
		t.Fatal(err)
	}
	return compositionPath, compositionPath + worldSnowBackupSuffix
}

func TestWorldSnowToggleRestoresOriginalComposition(t *testing.T) {
	store, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	original := []byte(`{"version":1,"componentConfigs":{"env_playerLand":{"keep":true},"env_trade_snow":{"modelFilename":"env_snow_d"}}}`)
	compositionPath, backupPath := configureWorldSnowTestPackage(t, store, original)

	settings, err := store.getWorldSnowSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled {
		t.Fatal("snow should initially be enabled")
	}
	if err := store.setWorldSnowEnabled(false); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read generated backup: %v", err)
	}
	if !bytes.Equal(backup, original) {
		t.Fatal("generated backup differs from the original composition")
	}

	updated, err := os.ReadFile(compositionPath)
	if err != nil {
		t.Fatal(err)
	}
	var composition struct {
		Version          int                        `json:"version"`
		ComponentConfigs map[string]json.RawMessage `json:"componentConfigs"`
	}
	if err := json.Unmarshal(updated, &composition); err != nil {
		t.Fatal(err)
	}
	if _, exists := composition.ComponentConfigs[worldSnowComponent]; exists {
		t.Fatal("snow component is still present after disabling")
	}
	if _, exists := composition.ComponentConfigs["env_playerLand"]; !exists {
		t.Fatal("disabling snow removed an unrelated island component")
	}
	if composition.Version != 1 {
		t.Fatalf("unrelated top-level version changed to %d", composition.Version)
	}

	if err := store.setWorldSnowEnabled(true); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(compositionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, original, restored) {
		t.Fatalf("re-enabled composition does not match the original: %s", restored)
	}
}

func TestWorldSnowAdminAPIUpdatesComposition(t *testing.T) {
	store, mux := newAdminTestServer(t)
	configureWorldSnowTestPackage(t, store, []byte(`{"componentConfigs":{"env_trade_snow":{"modelFilename":"env_snow_d"}}}`))

	panelRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	panelRequest.RemoteAddr = "127.0.0.1:12345"
	panelResponse := httptest.NewRecorder()
	mux.ServeHTTP(panelResponse, panelRequest)
	if panelResponse.Code != http.StatusOK {
		t.Fatalf("admin panel status = %d, want %d", panelResponse.Code, http.StatusOK)
	}
	if !bytes.Contains(panelResponse.Body.Bytes(), []byte(`id="worldSnowSettings"`)) {
		t.Fatal("admin panel does not include the snow toggle")
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/admin/api/game/world-snow", nil)
	getRequest.RemoteAddr = "127.0.0.1:12345"
	getResponse := httptest.NewRecorder()
	mux.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d: %s", getResponse.Code, http.StatusOK, getResponse.Body.String())
	}
	var settings worldSnowSettings
	if err := json.Unmarshal(getResponse.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled {
		t.Fatal("GET reported snow disabled when the component is present")
	}

	putRequest := httptest.NewRequest(http.MethodPut, "/admin/api/game/world-snow", bytes.NewBufferString(`{"enabled":false}`))
	putRequest.RemoteAddr = "127.0.0.1:12345"
	putResponse := httptest.NewRecorder()
	mux.ServeHTTP(putResponse, putRequest)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d: %s", putResponse.Code, http.StatusOK, putResponse.Body.String())
	}
	if err := json.Unmarshal(putResponse.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Enabled {
		t.Fatal("PUT response reported snow enabled after disabling")
	}

	invalidRequest := httptest.NewRequest(http.MethodPut, "/admin/api/game/world-snow", bytes.NewBufferString(`{}`))
	invalidRequest.RemoteAddr = "127.0.0.1:12345"
	invalidResponse := httptest.NewRecorder()
	mux.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("PUT without enabled status = %d, want %d", invalidResponse.Code, http.StatusBadRequest)
	}
}

func TestWorldSnowCannotBeEnabledWithoutOriginalBackup(t *testing.T) {
	store, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	compositionPath, _ := configureWorldSnowTestPackage(t, store, []byte(`{"componentConfigs":{"env_playerLand":{"keep":true}}}`))
	before, err := os.ReadFile(compositionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setWorldSnowEnabled(true); err == nil {
		t.Fatal("enabling snow without the original component backup succeeded")
	}
	after, err := os.ReadFile(compositionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed enable modified the island composition")
	}
}

func jsonEqual(t *testing.T, left, right []byte) bool {
	t.Helper()
	var leftValue, rightValue interface{}
	if err := json.Unmarshal(left, &leftValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		t.Fatal(err)
	}
	leftJSON, err := json.Marshal(leftValue)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.Marshal(rightValue)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(leftJSON, rightJSON)
}
