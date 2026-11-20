package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupInventoryTest(t *testing.T) (*Store, *http.ServeMux, Player) {
	t.Helper()
	store, mux := newAdminTestServer(t)
	player, err := store.CreatePlayer("inventory-owner", "Inventory Island", "inventory-device")
	if err != nil {
		t.Fatal(err)
	}

	store.appxPackageDir = filepath.Join(t.TempDir(), "AppxPackageExtracted")
	bundleDir := filepath.Join(store.appxPackageDir, "bundle")
	dataDir := filepath.Join(bundleDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	storageCatalog := `{"catalog":{"storage_barn":{"compositions":[{"compositionName":"resource_banana"},{"compositionName":"resource_coconut"}]},"storage_silo":{"compositions":[{"compositionName":"resource_fish"}]}}}`
	resources := `{"resource_banana":{"nameKey":"resource_name_bananas","iconFilename":"icon_bananas.png"},"resource_coconut":{"nameKey":"resource_name_coconut","iconFilename":"icon_coconut.png"},"resource_fish":{"nameKey":"resource_name_fish","iconFilename":"icon_fish.png"}}`
	for path, content := range map[string]string{
		filepath.Join(bundleDir, "StorageConfigService_StorageCatalog.json"): storageCatalog,
		filepath.Join(dataDir, "ResourcesConfig.json"):                       resources,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	blob := `{"_compositionName":"player","_t":"GameEntityFragment:v1","level":[],"wallet":{"capacity":[],"receivedInitialResources":true,"wealth":{"resource_banana":7,"resource_custom_ship_part":2}}}`
	sum := sha256.Sum256([]byte(blob))
	if err := store.UpsertFragment(playerGameEntityKey(player.UUID), blob, fmt.Sprintf("%x", sum)); err != nil {
		t.Fatal(err)
	}
	return store, mux, player
}

func inventoryRequest(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	return response
}

func TestAdminPlayerInventoryLoadsCatalogAndHeldItems(t *testing.T) {
	_, mux, player := setupInventoryTest(t)
	response := inventoryRequest(mux, http.MethodGet, "/admin/api/players/"+player.UUID+"/inventory", "")
	if response.Code != http.StatusOK {
		t.Fatalf("inventory status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var inventory playerInventory
	if err := json.Unmarshal(response.Body.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Nanopods != player.Nanopods || inventory.Gems != player.Gems {
		t.Fatalf("currency balances = (%d, %d), want (%d, %d)", inventory.Nanopods, inventory.Gems, player.Nanopods, player.Gems)
	}
	items := make(map[string]inventoryItem, len(inventory.Items))
	for _, item := range inventory.Items {
		items[item.ID] = item
	}
	if len(items) != 4 {
		t.Fatalf("inventory item count = %d, want 4", len(items))
	}
	if items["resource_banana"].Amount != 7 || items["resource_banana"].Category != "storage_barn" {
		t.Fatalf("banana inventory item = %+v", items["resource_banana"])
	}
	if items["resource_coconut"].Amount != 0 || items["resource_coconut"].Name != "Coconut" {
		t.Fatalf("unowned catalog item = %+v", items["resource_coconut"])
	}
	if items["resource_custom_ship_part"].Amount != 2 || items["resource_custom_ship_part"].Category != "other" {
		t.Fatalf("other held item = %+v", items["resource_custom_ship_part"])
	}
}

func TestAdminPanelRendersInventoryEditor(t *testing.T) {
	_, mux := newAdminTestServer(t)
	response := inventoryRequest(mux, http.MethodGet, "/admin", "")
	if response.Code != http.StatusOK {
		t.Fatalf("admin panel status = %d, want %d", response.Code, http.StatusOK)
	}
	for _, expected := range []string{`id="player-inventory"`, `id="inventoryCurrencies"`, `id="inventoryCategory"`, `id="inventorySearch"`, `id="inventoryItems"`, `id="inventoryStatus"`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("admin panel is missing %q", expected)
		}
	}
}

func TestInventoryCatalogReadsExtractedGameData(t *testing.T) {
	store, _ := newAdminTestServer(t)
	store.appxPackageDir = "AppxPackageExtracted"
	catalog, err := store.loadInventoryCatalog()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("extracted game catalog not present: %v", err)
		}
		t.Fatal(err)
	}
	banana, ok := catalog["resource_banana"]
	if !ok {
		t.Fatal("storage catalog is missing resource_banana")
	}
	if banana.category != "storage_silo" || banana.image != "icon_resource_bananas.png" {
		t.Fatalf("banana catalog data = %+v", banana)
	}
}

func TestAdminPlayerInventoryUpdatesQuantitiesAndCurrencies(t *testing.T) {
	store, mux, player := setupInventoryTest(t)
	response := inventoryRequest(mux, http.MethodPut, "/admin/api/players/"+player.UUID+"/inventory/resource_coconut", `{"amount":42}`)
	if response.Code != http.StatusOK {
		t.Fatalf("inventory update status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	blob, err := store.GetFragment(playerGameEntityKey(player.UUID))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &root); err != nil {
		t.Fatal(err)
	}
	var fragment struct {
		Wallet struct {
			Capacity []json.RawMessage `json:"capacity"`
			Wealth   map[string]int64  `json:"wealth"`
		} `json:"wallet"`
		Level []json.RawMessage `json:"level"`
	}
	if err := json.Unmarshal([]byte(blob), &fragment); err != nil {
		t.Fatal(err)
	}
	if fragment.Wallet.Wealth["resource_coconut"] != 42 || fragment.Wallet.Wealth["resource_banana"] != 7 || fragment.Wallet.Wealth["resource_custom_ship_part"] != 2 || len(fragment.Wallet.Capacity) != 0 || len(fragment.Level) != 0 {
		t.Fatalf("updated player fragment did not preserve other data: %s", blob)
	}
	var storedHash string
	if err := store.db.NewSelect().Model((*DocumentFragment)(nil)).Column("hash").Where("key = ?", playerGameEntityKey(player.UUID)).Scan(context.Background(), &storedHash); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(blob))
	if storedHash != fmt.Sprintf("%x", sum) {
		t.Fatalf("stored hash = %q, want hash of updated fragment", storedHash)
	}

	response = inventoryRequest(mux, http.MethodPut, "/admin/api/players/"+player.UUID+"/currencies", `{"nanopods":321,"gems":654}`)
	if response.Code != http.StatusOK {
		t.Fatalf("currency update status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	updated, err := store.GetPlayerByCID(player.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Nanopods != 321 || updated.Gems != 654 {
		t.Fatalf("updated currency balances = (%d, %d), want (321, 654)", updated.Nanopods, updated.Gems)
	}
}

func TestAdminPlayerInventoryRejectsOnlineAndInvalidItems(t *testing.T) {
	store, mux, player := setupInventoryTest(t)
	session := store.registerGameSession(player.UUID)
	defer store.unregisterGameSession(session)

	response := inventoryRequest(mux, http.MethodPut, "/admin/api/players/"+player.UUID+"/inventory/resource_banana", `{"amount":99}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("online inventory update status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if err := store.setPlayerInventoryItem(player.UUID, "resource_not_in_catalog", 1); !errors.Is(err, errPlayerOnline) {
		t.Fatalf("online inventory update error = %v, want %v", err, errPlayerOnline)
	}
	store.unregisterGameSession(session)
	if err := store.setPlayerInventoryItem(player.UUID, "resource_not_in_catalog", 1); !errors.Is(err, errInventoryItemNotFound) {
		t.Fatalf("unknown inventory update error = %v, want %v", err, errInventoryItemNotFound)
	}
}
