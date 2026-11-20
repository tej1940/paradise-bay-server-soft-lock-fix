package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type buildMenuTestCost struct {
	Amount   int    `json:"amount"`
	Resource string `json:"resource"`
}

type buildMenuTestCostTier struct {
	Cost []buildMenuTestCost `json:"cost"`
}

type buildMenuTestConstruction struct {
	Costs []buildMenuTestCostTier `json:"constructionCostForOwnedQuantity"`
	Times []int                   `json:"constructionTimeSecForOwnedQuantity"`
}

func configureBuildMenuTestPackage(t *testing.T, store *Store) (map[string][]byte, string) {
	t.Helper()
	store.appxPackageDir = filepath.Join(t.TempDir(), "AppxPackageExtracted")
	dataDir := filepath.Join(store.appxPackageDir, "bundle", "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.appxPackageDir, "AppxManifest.xml"), []byte("<Package/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := `{"catalog":{"decorations":{"compositions":[{"compositionName":"hidden_event","_feature_buildingConfigServicePostCook":"event_halloween"},{"compositionName":"hidden_weather","_feature_buildingConfigServicePostCook":"weather_machine"},{"compositionName":"hidden_regular","_feature_buildingConfigServicePostCook":""},{"compositionName":"hidden_disabled","_feature_buildingConfigServicePostCook":"event_disabled"}]}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "BuildMenuCatalog.json"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	features := `{"features":["event_halloween","weather_machine"]}`
	if err := os.WriteFile(filepath.Join(store.appxPackageDir, "bundle", "features.json"), []byte(features), 0o644); err != nil {
		t.Fatal(err)
	}

	originals := map[string][]byte{
		"hidden_event":    buildMenuTestComposition(false, []string{"tag_decoration"}),
		"hidden_weather":  buildMenuTestComposition(false, []string{"tag_decoration"}),
		"hidden_regular":  buildMenuTestCompositionWithGoldCost(false, []string{"tag_decoration"}, 250),
		"hidden_disabled": buildMenuTestComposition(false, []string{"tag_decoration"}),
	}
	for name, original := range originals {
		if err := os.WriteFile(filepath.Join(dataDir, name+".composition.json"), original, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return originals, dataDir
}

func buildMenuTestComposition(visible bool, tags []string) []byte {
	return buildMenuTestCompositionWithGoldCost(visible, tags, 0)
}

func buildMenuTestCompositionWithGoldCost(visible bool, tags []string, amount int) []byte {
	encodedTags, _ := json.Marshal(tags)
	cost := ""
	if amount > 0 {
		cost = fmt.Sprintf(`,"constructionCostForOwnedQuantity":[{"cost":[{"amount":%d,"resource":"resource_gold"}]}]`, amount)
	}
	return []byte(fmt.Sprintf(
		`{"componentConfigs":{"construction":{"_t":"VillageConstructionComponent"%s},"display":{"visibleInBuildMenuWhenUnowned":%t},"tags":{"tags":%s}}}`,
		cost,
		visible,
		encodedTags,
	))
}

func TestBuildMenuModesBackUpAndRestoreCompositions(t *testing.T) {
	store, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	originals, dataDir := configureBuildMenuTestPackage(t, store)

	settings, err := store.getBuildMenuModeSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != buildMenuModeDefault || settings.AllHiddenCount != 3 || settings.EventDecorationCount != 1 {
		t.Fatalf("initial build menu settings = %+v", settings)
	}

	settings, err = store.setBuildMenuMode(buildMenuModeAllHidden)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != buildMenuModeAllHidden {
		t.Fatalf("all-hidden mode = %q", settings.Mode)
	}
	for _, name := range []string{"hidden_event", "hidden_weather", "hidden_regular"} {
		assertBuildMenuVisible(t, filepath.Join(dataDir, name+".composition.json"), true)
	}
	assertBuildMenuVisible(t, filepath.Join(dataDir, "hidden_disabled.composition.json"), false)
	assertBuildMenuGoldPrice(t, filepath.Join(dataDir, "hidden_event.composition.json"), 1000)
	assertBuildMenuGoldPrice(t, filepath.Join(dataDir, "hidden_weather.composition.json"), 1000)
	assertBuildMenuGoldPrice(t, filepath.Join(dataDir, "hidden_regular.composition.json"), 250)
	assertBuildMenuInstantPurchase(t, filepath.Join(dataDir, "hidden_weather.composition.json"))

	paths, err := store.buildMenuPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hidden_event", "hidden_weather", "hidden_regular"} {
		backup, err := os.ReadFile(buildMenuBackupPath(paths, name))
		if err != nil {
			t.Fatalf("read backup for %s: %v", name, err)
		}
		if !bytes.Equal(backup, originals[name]) {
			t.Errorf("backup for %s differs from the original composition", name)
		}
	}

	legacyVisible, err := setBuildMenuVisibility(originals["hidden_weather"], true)
	if err != nil {
		t.Fatal(err)
	}
	weatherPath := filepath.Join(dataDir, "hidden_weather.composition.json")
	if err := os.WriteFile(weatherPath, legacyVisible, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.setBuildMenuMode(buildMenuModeAllHidden); err != nil {
		t.Fatal(err)
	}
	assertBuildMenuGoldPrice(t, weatherPath, 1000)

	settings, err = store.setBuildMenuMode(buildMenuModeEventDecorations)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != buildMenuModeEventDecorations || settings.EventDecorationCount != 1 {
		t.Fatalf("event decoration mode settings = %+v", settings)
	}
	assertBuildMenuVisible(t, filepath.Join(dataDir, "hidden_event.composition.json"), true)
	assertBuildMenuVisible(t, filepath.Join(dataDir, "hidden_weather.composition.json"), false)
	assertBuildMenuVisible(t, filepath.Join(dataDir, "hidden_regular.composition.json"), false)
	assertBuildMenuVisible(t, filepath.Join(dataDir, "hidden_disabled.composition.json"), false)
	assertBuildMenuHasNoGoldPrice(t, filepath.Join(dataDir, "hidden_weather.composition.json"))

	settings, err = store.setBuildMenuMode(buildMenuModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != buildMenuModeDefault {
		t.Fatalf("restored build menu mode = %q", settings.Mode)
	}
	for name, original := range originals {
		restored, err := os.ReadFile(filepath.Join(dataDir, name+".composition.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restored, original) {
			t.Errorf("composition %s was not restored byte-for-byte", name)
		}
	}
}

func TestBuildMenuAdminAPIAndPanel(t *testing.T) {
	store, mux := newAdminTestServer(t)
	configureBuildMenuTestPackage(t, store)

	getRequest := httptest.NewRequest(http.MethodGet, "/admin/api/game/build-menu", nil)
	getRequest.RemoteAddr = "127.0.0.1:12345"
	getResponse := httptest.NewRecorder()
	mux.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d: %s", getResponse.Code, http.StatusOK, getResponse.Body.String())
	}
	var settings buildMenuModeSettings
	if err := json.Unmarshal(getResponse.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Mode != buildMenuModeDefault || settings.AllHiddenCount != 3 || settings.EventDecorationCount != 1 {
		t.Fatalf("GET build menu settings = %+v", settings)
	}

	invalidRequest := httptest.NewRequest(http.MethodPut, "/admin/api/game/build-menu", bytes.NewBufferString(`{"mode":"unknown"}`))
	invalidRequest.RemoteAddr = "127.0.0.1:12345"
	invalidResponse := httptest.NewRecorder()
	mux.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want %d", invalidResponse.Code, http.StatusBadRequest)
	}

	putRequest := httptest.NewRequest(http.MethodPut, "/admin/api/game/build-menu", bytes.NewBufferString(`{"mode":"event-decorations"}`))
	putRequest.RemoteAddr = "127.0.0.1:12345"
	putResponse := httptest.NewRecorder()
	mux.ServeHTTP(putResponse, putRequest)
	if putResponse.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d: %s", putResponse.Code, http.StatusOK, putResponse.Body.String())
	}
	if err := json.Unmarshal(putResponse.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Mode != buildMenuModeEventDecorations {
		t.Fatalf("PUT mode = %q", settings.Mode)
	}

	panelRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	panelRequest.RemoteAddr = "127.0.0.1:12345"
	panelResponse := httptest.NewRecorder()
	mux.ServeHTTP(panelResponse, panelRequest)
	if panelResponse.Code != http.StatusOK {
		t.Fatalf("admin panel status = %d, want %d", panelResponse.Code, http.StatusOK)
	}
	if !bytes.Contains(panelResponse.Body.Bytes(), []byte(`id="buildMenuSettings"`)) {
		t.Fatal("admin panel does not include build menu visibility settings")
	}
}

func assertBuildMenuVisible(t *testing.T, path string, want bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	visible, exists, _, err := decodeBuildMenuVisibility(data)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || visible != want {
		t.Fatalf("composition visibility = (%t, %t), want (true, %t)", visible, exists, want)
	}
}

func assertBuildMenuGoldPrice(t *testing.T, path string, want int) {
	t.Helper()
	construction := readBuildMenuTestConstruction(t, path)
	if len(construction.Costs) == 0 || len(construction.Costs[0].Cost) == 0 {
		t.Fatalf("composition %s has no construction cost", path)
	}
	cost := construction.Costs[0].Cost[0]
	if cost.Resource != "resource_gold" || cost.Amount != want {
		t.Fatalf("composition %s cost = %+v, want %d gold", path, cost, want)
	}
}

func assertBuildMenuInstantPurchase(t *testing.T, path string) {
	t.Helper()
	construction := readBuildMenuTestConstruction(t, path)
	if len(construction.Times) != 1 || construction.Times[0] != 0 {
		t.Fatalf("composition %s construction times = %v, want [0]", path, construction.Times)
	}
}

func assertBuildMenuHasNoGoldPrice(t *testing.T, path string) {
	t.Helper()
	construction := readBuildMenuTestConstruction(t, path)
	if len(construction.Costs) != 0 {
		t.Fatalf("composition %s unexpectedly has an injected construction cost", path)
	}
}

func readBuildMenuTestConstruction(t *testing.T, path string) buildMenuTestConstruction {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var composition struct {
		ComponentConfigs struct {
			Construction buildMenuTestConstruction `json:"construction"`
		} `json:"componentConfigs"`
	}
	if err := json.Unmarshal(data, &composition); err != nil {
		t.Fatal(err)
	}
	return composition.ComponentConfigs.Construction
}
