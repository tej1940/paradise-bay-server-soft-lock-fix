package main

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGameItemPricesCanBeEditedRepeatedlyAndReset(t *testing.T) {
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
	name := "hidden_event"

	if err := store.setGameItemPrice(name, gamePriceUpdate{PurchaseGold: intPointer(450), HurryAmount: intPointer(5)}); err != nil {
		t.Fatal(err)
	}
	if err := store.setGameItemPrice(name, gamePriceUpdate{PurchaseGold: intPointer(700), HurryAmount: intPointer(9)}); err != nil {
		t.Fatalf("second price update: %v", err)
	}

	priceData, err := os.ReadFile(filepath.Join(dataDir, name+".composition.json"))
	if err != nil {
		t.Fatal(err)
	}
	item, err := decodeGamePriceItem(name, "decorations", priceData)
	if err != nil {
		t.Fatal(err)
	}
	if item.PurchaseGold == nil || *item.PurchaseGold != 700 || item.HurryAmount == nil || *item.HurryAmount != 9 {
		t.Fatalf("updated prices = %+v, want 700 gold and 9 gems", item)
	}

	restored, err := store.resetGameItemPrice(name)
	if err != nil || !restored {
		t.Fatalf("reset price = %t, %v; want true, nil", restored, err)
	}
	restoredData, err := os.ReadFile(filepath.Join(dataDir, name+".composition.json"))
	if err != nil {
		t.Fatal(err)
	}
	equal, err := gameCompositionPricesEqual(restoredData, originals[name])
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Fatal("reset did not restore the original prices")
	}
}

func TestGamePriceCatalogIncludesClearablesAndGameCategories(t *testing.T) {
	store, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	_, dataDir := configureBuildMenuTestPackage(t, store)
	clearable := `{"componentConfigs":{"construction":{"constructionTimeSecForOwnedQuantity":[0]},"display":{"nameKey":"display_name_clearables_banana_tree"}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "clearables_banana_tree.composition.json"), []byte(clearable), 0o644); err != nil {
		t.Fatal(err)
	}
	building := `{"componentConfigs":{"construction":{},"display":{"iconKey":"icon_building_bananaTree.png"}}}`
	if err := os.WriteFile(filepath.Join(dataDir, "building_banana_tree.composition.json"), []byte(building), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "icon_building_bananaTree.dds"), []byte("texture"), 0o644); err != nil {
		t.Fatal(err)
	}

	categories, err := store.getGamePriceCategories()
	if err != nil {
		t.Fatal(err)
	}
	foundCategory, foundClearable := false, false
	for _, category := range categories {
		if category.ID == "decorations" {
			foundCategory = true
		}
		if category.ID == "clearing" {
			for _, item := range category.Items {
				if item.Composition == "clearables_banana_tree" && item.Clearable && item.Image == "icon_building_bananaTree.png" {
					foundClearable = true
				}
			}
		}
	}
	if !foundCategory || !foundClearable {
		t.Fatalf("missing catalog category or tree clear cost: %+v", categories)
	}

	if err := store.setGameItemPrice("clearables_banana_tree", gamePriceUpdate{PurchaseGold: intPointer(250)}); err != nil {
		t.Fatal(err)
	}
	priceData, err := os.ReadFile(filepath.Join(dataDir, "clearables_banana_tree.composition.json"))
	if err != nil {
		t.Fatal(err)
	}
	item, err := decodeGamePriceItem("clearables_banana_tree", "clearing", priceData)
	if err != nil {
		t.Fatal(err)
	}
	if item.PurchaseGold == nil || *item.PurchaseGold != 250 {
		t.Fatalf("tree clearing cost = %v, want 250 gold", item.PurchaseGold)
	}
}

func TestBuildMenuModeChangesPreserveEditedPrices(t *testing.T) {
	store, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	_, dataDir := configureBuildMenuTestPackage(t, store)
	name := "hidden_event"

	if _, err := store.setBuildMenuMode(buildMenuModeAllHidden); err != nil {
		t.Fatal(err)
	}
	if err := store.setGameItemPrice(name, gamePriceUpdate{PurchaseGold: intPointer(480)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.setBuildMenuMode(buildMenuModeEventDecorations); err != nil {
		t.Fatal(err)
	}
	if _, err := store.setBuildMenuMode(buildMenuModeDefault); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dataDir, name+".composition.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	item, err := decodeGamePriceItem(name, "decorations", data)
	if err != nil {
		t.Fatal(err)
	}
	if item.PurchaseGold == nil || *item.PurchaseGold != 480 {
		t.Fatalf("build menu reset changed custom price: %+v", item.PurchaseGold)
	}
	visible, exists, _, err := decodeBuildMenuVisibility(data)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || visible {
		t.Fatal("build menu reset did not restore original visibility")
	}

	if restored, err := store.resetGameItemPrice(name); err != nil || !restored {
		t.Fatalf("reset custom price = %t, %v", restored, err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	visible, exists, _, err = decodeBuildMenuVisibility(data)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || visible {
		t.Fatal("restoring the price changed build menu visibility")
	}
}

func TestGamePriceAdminRoutesAndPanel(t *testing.T) {
	store, mux := newAdminTestServer(t)
	_, dataDir := configureBuildMenuTestPackage(t, store)
	var icon bytes.Buffer
	if err := png.Encode(&icon, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "admin-test.png"), icon.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/admin/api/game/prices", nil)
	getRequest.RemoteAddr = "127.0.0.1:12345"
	getResponse := httptest.NewRecorder()
	mux.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET prices status = %d, want %d: %s", getResponse.Code, http.StatusOK, getResponse.Body.String())
	}

	updateRequest := httptest.NewRequest(http.MethodPut, "/admin/api/game/prices/hidden_event", strings.NewReader(`{"purchaseGold":-1}`))
	updateRequest.RemoteAddr = "127.0.0.1:12345"
	updateRequest.Header.Set("Content-Type", "application/json")
	updateResponse := httptest.NewRecorder()
	mux.ServeHTTP(updateResponse, updateRequest)
	if updateResponse.Code != http.StatusBadRequest {
		t.Fatalf("negative price status = %d, want %d", updateResponse.Code, http.StatusBadRequest)
	}

	if err := store.setGameItemPrice("hidden_event", gamePriceUpdate{PurchaseGold: intPointer(480)}); err != nil {
		t.Fatal(err)
	}
	imageRequest := httptest.NewRequest(http.MethodGet, "/admin/assets/admin-test.png", nil)
	imageRequest.RemoteAddr = "127.0.0.1:12345"
	imageResponse := httptest.NewRecorder()
	mux.ServeHTTP(imageResponse, imageRequest)
	if imageResponse.Code != http.StatusOK || imageResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image response = %d %q, want 200 image/png", imageResponse.Code, imageResponse.Header().Get("Content-Type"))
	}

	resetRequest := httptest.NewRequest(http.MethodPost, "/admin/api/game/prices/reset-all", nil)
	resetRequest.RemoteAddr = "127.0.0.1:12345"
	resetResponse := httptest.NewRecorder()
	mux.ServeHTTP(resetResponse, resetRequest)
	if resetResponse.Code != http.StatusOK {
		t.Fatalf("reset all status = %d, want %d: %s", resetResponse.Code, http.StatusOK, resetResponse.Body.String())
	}

	_, err := os.Stat(filepath.Join(dataDir, "hidden_event.composition.json"))
	if err != nil {
		t.Fatal(err)
	}
	panelRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	panelRequest.RemoteAddr = "127.0.0.1:12345"
	panelResponse := httptest.NewRecorder()
	mux.ServeHTTP(panelResponse, panelRequest)
	if panelResponse.Code != http.StatusOK {
		t.Fatalf("admin panel status = %d, want %d", panelResponse.Code, http.StatusOK)
	}
	html := panelResponse.Body.String()
	for _, expected := range []string{`id="game-prices"`, `id="priceRail"`, `id="priceCardSize"`, `id="resetAllPrices"`, "module-resize-handle", "pointermove", `id="columnResizeHandle"`, "adminColumnRatioKey", "ArrowLeft"} {
		if !strings.Contains(html, expected) {
			t.Errorf("admin panel is missing %q", expected)
		}
	}
}

func TestDecodeDDSIcon(t *testing.T) {
	path := filepath.Join("AppxPackageExtracted", "bundle", "data", "icon_building_bananaTree.dds")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("game icon fixture not present: %v", err)
	}
	pngData, err := decodeDDSIcon(data)
	if err != nil {
		t.Fatal(err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(pngData))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 256 || config.Height != 256 {
		t.Fatalf("converted icon dimensions = %dx%d, want 256x256", config.Width, config.Height)
	}
}

func intPointer(value int) *int {
	return &value
}
