package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAdminTestServer(t *testing.T) (*Store, *http.ServeMux) {
	t.Helper()

	store, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	mux := http.NewServeMux()
	registerAdminRoutes(mux, store)
	return store, mux
}

func TestAdminRoutesRequireLoopback(t *testing.T) {
	_, mux := newAdminTestServer(t)

	remoteRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	remoteRequest.RemoteAddr = "192.0.2.1:12345"
	remoteResponse := httptest.NewRecorder()
	mux.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusForbidden {
		t.Fatalf("remote admin request status = %d, want %d", remoteResponse.Code, http.StatusForbidden)
	}

	localRequest := httptest.NewRequest(http.MethodGet, "/admin/api/players", nil)
	localRequest.RemoteAddr = "127.0.0.1:12345"
	localResponse := httptest.NewRecorder()
	mux.ServeHTTP(localResponse, localRequest)
	if localResponse.Code != http.StatusOK {
		t.Fatalf("local admin API status = %d, want %d: %s", localResponse.Code, http.StatusOK, localResponse.Body.String())
	}
}

func TestAdminRestorePreservesRelinkedDeviceAndRestoresGameAlias(t *testing.T) {
	store, mux := newAdminTestServer(t)
	player, err := store.CreatePlayer("restore-owner", "Island", "old-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlayerGameAlias(player.UUID, "Saved Island Name"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFragment("GamePlayer:storage:P[restore-owner]", `{"items":["saved"]}`, "saved-hash"); err != nil {
		t.Fatal(err)
	}

	backupRequest := httptest.NewRequest(http.MethodGet, "/admin/api/players/restore-owner/backup", nil)
	backupRequest.RemoteAddr = "127.0.0.1:12345"
	backupResponse := httptest.NewRecorder()
	mux.ServeHTTP(backupResponse, backupRequest)
	if backupResponse.Code != http.StatusOK {
		t.Fatalf("backup request status = %d, want %d: %s", backupResponse.Code, http.StatusOK, backupResponse.Body.String())
	}

	if err := store.SetPlayerIdentity(player.UUID, player.Alias, "new-device"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPlayerGameAlias(player.UUID, "Changed Name"); err != nil {
		t.Fatal(err)
	}

	restoreRequest := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/players/restore-owner/restore",
		strings.NewReader(backupResponse.Body.String()),
	)
	restoreRequest.RemoteAddr = "127.0.0.1:12345"
	restoreRequest.Header.Set("Content-Type", "application/json")
	restoreResponse := httptest.NewRecorder()
	mux.ServeHTTP(restoreResponse, restoreRequest)
	if restoreResponse.Code != http.StatusOK {
		t.Fatalf("restore request status = %d, want %d: %s", restoreResponse.Code, http.StatusOK, restoreResponse.Body.String())
	}

	restoredPlayer, err := store.GetPlayerByCID(player.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredPlayer.Z2DID != "new-device" {
		t.Fatalf("restored device ID = %q, want %q", restoredPlayer.Z2DID, "new-device")
	}
	if restoredPlayer.GameAlias != "Saved Island Name" {
		t.Fatalf("restored game alias = %q, want %q", restoredPlayer.GameAlias, "Saved Island Name")
	}
}

func TestAdminSkipDiveTutorial(t *testing.T) {
	store, mux := newAdminTestServer(t)
	player, err := store.CreatePlayer("tutorial-owner", "Island", "tutorial-device")
	if err != nil {
		t.Fatal(err)
	}

	historyKey := "TKMissions:history:P[" + player.UUID + "]"
	history := `{"_t":"TKMissionHistoryFragment:v1","mission_angelfish_dive_update_tutorial_2a":"a","mission_angelfish_sunshine_collect_seal_house":"a"}`
	if err := store.UpsertFragment(historyKey, history, "history-hash"); err != nil {
		t.Fatal(err)
	}
	stateCacheKey := "TKMissions:stateCache:P[" + player.UUID + "]"
	stateCache := `{"_t":"TKMissionCacheFragment:v1","cache":{"a":{"mission_angelfish_dive_update_tutorial_2a":false},"c":{"mission_angelfish_dive_update_tutorial_2":true},"p":{"mission_angelfish_dive_update_tutorial_3":true,"mission_angelfish_dive_update_tutorial_4":true},"s":[],"v":[]}}`
	if err := store.UpsertFragment(stateCacheKey, stateCache, "cache-hash"); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/admin/api/players/"+player.UUID+"/skip-seal-house-tutorial", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("skip tutorial status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	historyBlob, err := store.GetFragment(historyKey)
	if err != nil {
		t.Fatal(err)
	}
	var restoredHistory map[string]string
	if err := json.Unmarshal([]byte(historyBlob), &restoredHistory); err != nil {
		t.Fatal(err)
	}
	for _, missionID := range diveTutorialMissionIDs {
		if restoredHistory[missionID] != "c" {
			t.Errorf("mission %q status = %q, want completed", missionID, restoredHistory[missionID])
		}
	}

	if restoredHistory["mission_angelfish_sunshine_collect_seal_house"] != "a" {
		t.Fatal("skipping the dive tutorial changed an unrelated mission")
	}

	stateCacheBlob, err := store.GetFragment(stateCacheKey)
	if err != nil {
		t.Fatal(err)
	}
	var restoredState struct {
		Cache map[string]json.RawMessage `json:"cache"`
	}
	if err := json.Unmarshal([]byte(stateCacheBlob), &restoredState); err != nil {
		t.Fatal(err)
	}
	var completedCache map[string]bool
	if err := json.Unmarshal(restoredState.Cache["c"], &completedCache); err != nil {
		t.Fatal(err)
	}
	var activeCache map[string]bool
	if err := json.Unmarshal(restoredState.Cache["a"], &activeCache); err != nil {
		t.Fatal(err)
	}
	var pendingCache map[string]bool
	if err := json.Unmarshal(restoredState.Cache["p"], &pendingCache); err != nil {
		t.Fatal(err)
	}
	for _, missionID := range diveTutorialMissionIDs {
		if !completedCache[missionID] {
			t.Errorf("mission %q is not marked completed in the cache", missionID)
		}
		if _, ok := activeCache[missionID]; ok {
			t.Errorf("mission %q remains in the active cache", missionID)
		}
		if _, ok := pendingCache[missionID]; ok {
			t.Errorf("mission %q remains in the pending cache", missionID)
		}
	}
	if string(restoredState.Cache["s"]) != "[]" || string(restoredState.Cache["v"]) != "[]" {
		t.Fatal("the non-map mission caches were changed")
	}
}

func TestNewPlayerSkipsDiveTutorialByDefault(t *testing.T) {
	store, _ := newAdminTestServer(t)
	player, err := store.CreatePlayer("new-player", "New Island", "new-device")
	if err != nil {
		t.Fatal(err)
	}

	assertDiveTutorialSkipped(t, store, player.UUID)
}

func TestExistingPlayersSkipDiveTutorialOnServerStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	player, err := store.CreatePlayer("existing-player", "Existing Island", "existing-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFragment(
		"TKMissions:history:P["+player.UUID+"]",
		`{"_t":"TKMissionHistoryFragment:v1","mission_angelfish_dive_update_tutorial_2a":"a"}`,
		"active-history",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertFragment(
		"TKMissions:stateCache:P["+player.UUID+"]",
		`{"_t":"TKMissionCacheFragment:v1","cache":{"a":{"mission_angelfish_dive_update_tutorial_2a":false},"c":{},"p":{"mission_angelfish_dive_update_tutorial_3":true},"s":[],"v":[]}}`,
		"active-cache",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	assertDiveTutorialSkipped(t, store, player.UUID)
}

func assertDiveTutorialSkipped(t *testing.T, store *Store, uuid string) {
	t.Helper()

	historyBlob, err := store.GetFragment("TKMissions:history:P[" + uuid + "]")
	if err != nil {
		t.Fatal(err)
	}
	var history map[string]string
	if err := json.Unmarshal([]byte(historyBlob), &history); err != nil {
		t.Fatal(err)
	}
	for _, missionID := range diveTutorialMissionIDs {
		if history[missionID] != "c" {
			t.Errorf("mission %q status = %q, want completed", missionID, history[missionID])
		}
	}

	cacheBlob, err := store.GetFragment("TKMissions:stateCache:P[" + uuid + "]")
	if err != nil {
		t.Fatal(err)
	}
	var fragment struct {
		Cache map[string]json.RawMessage `json:"cache"`
	}
	if err := json.Unmarshal([]byte(cacheBlob), &fragment); err != nil {
		t.Fatal(err)
	}
	var completed map[string]bool
	if err := json.Unmarshal(fragment.Cache["c"], &completed); err != nil {
		t.Fatal(err)
	}
	for _, missionID := range diveTutorialMissionIDs {
		if !completed[missionID] {
			t.Errorf("mission %q is not marked complete in cache", missionID)
		}
	}
}

func TestAdminWorldMarketSettingsAndSelectedPlayerRefresh(t *testing.T) {
	store, mux := newAdminTestServer(t)
	selectedPlayer, err := store.CreatePlayer("market-selected", "Selected Island", "market-selected-device")
	if err != nil {
		t.Fatal(err)
	}
	otherPlayer, err := store.CreatePlayer("market-other", "Other Island", "market-other-device")
	if err != nil {
		t.Fatal(err)
	}
	selectedSession := store.registerGameSession(selectedPlayer.UUID)
	defer store.unregisterGameSession(selectedSession)
	otherSession := store.registerGameSession(otherPlayer.UUID)
	defer store.unregisterGameSession(otherSession)
	otherCacheBefore, err := store.GetFragment(worldMarketCacheKey(otherPlayer.UUID))
	if err != nil {
		t.Fatal(err)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/admin/api/world-market/settings", nil)
	getRequest.RemoteAddr = "127.0.0.1:12345"
	getResponse := httptest.NewRecorder()
	mux.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get market settings status = %d, want %d: %s", getResponse.Code, http.StatusOK, getResponse.Body.String())
	}
	var settings worldMarketSettings
	if err := json.Unmarshal(getResponse.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode default market settings: %v", err)
	}
	if settings.RefreshIntervalMinutes != worldMarketDefaultRefreshIntervalMinutes || settings.OfferCount != worldMarketOfferCount {
		t.Fatalf("default market settings = %+v", settings)
	}

	invalidRequest := httptest.NewRequest(
		http.MethodPut,
		"/admin/api/world-market/settings",
		strings.NewReader(`{"refreshIntervalMinutes":0,"offerCount":3}`),
	)
	invalidRequest.RemoteAddr = "127.0.0.1:12345"
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	mux.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid settings status = %d, want %d", invalidResponse.Code, http.StatusBadRequest)
	}

	settingsBody := `{"refreshIntervalMinutes":2,"offerCount":3}`
	saveRequest := httptest.NewRequest(
		http.MethodPut,
		"/admin/api/world-market/settings",
		strings.NewReader(settingsBody),
	)
	saveRequest.RemoteAddr = "127.0.0.1:12345"
	saveRequest.Header.Set("Content-Type", "application/json")
	saveResponse := httptest.NewRecorder()
	mux.ServeHTTP(saveResponse, saveRequest)
	if saveResponse.Code != http.StatusOK {
		t.Fatalf("save market settings status = %d, want %d: %s", saveResponse.Code, http.StatusOK, saveResponse.Body.String())
	}

	refreshRequest := httptest.NewRequest(
		http.MethodPost,
		"/admin/api/players/"+selectedPlayer.UUID+"/world-market/refresh",
		nil,
	)
	refreshRequest.RemoteAddr = "127.0.0.1:12345"
	refreshResponse := httptest.NewRecorder()
	mux.ServeHTTP(refreshResponse, refreshRequest)
	if refreshResponse.Code != http.StatusOK {
		t.Fatalf("refresh selected market status = %d, want %d: %s", refreshResponse.Code, http.StatusOK, refreshResponse.Body.String())
	}
	var refreshResult struct {
		ActiveSessions int `json:"activeSessions"`
	}
	if err := json.Unmarshal(refreshResponse.Body.Bytes(), &refreshResult); err != nil {
		t.Fatalf("decode refresh result: %v", err)
	}
	if refreshResult.ActiveSessions != 1 {
		t.Fatalf("active sessions notified = %d, want 1", refreshResult.ActiveSessions)
	}

	selectedCacheBlob, err := store.GetFragment(worldMarketCacheKey(selectedPlayer.UUID))
	if err != nil {
		t.Fatal(err)
	}
	var selectedCache worldMarketCacheFragment
	if err := json.Unmarshal([]byte(selectedCacheBlob), &selectedCache); err != nil {
		t.Fatalf("decode refreshed selected market: %v", err)
	}
	if len(selectedCache.View) != 3 {
		t.Fatalf("selected player offer count = %d, want 3", len(selectedCache.View))
	}
	select {
	case update := <-selectedSession.pushes:
		if update.key != worldMarketCacheKey(selectedPlayer.UUID) || update.blobJSON != selectedCacheBlob {
			t.Fatal("active player session did not receive the refreshed market cache")
		}
	case <-time.After(time.Second):
		t.Fatal("active player session did not receive a market refresh notification")
	}
	select {
	case <-otherSession.pushes:
		t.Fatal("market refresh notification was sent to another player's session")
	default:
	}
	for i, rawView := range selectedCache.View {
		var view worldMarketCachedView
		if err := json.Unmarshal(rawView, &view); err != nil {
			t.Fatalf("decode refreshed selected market view %d: %v", i, err)
		}
		if view.UUID == "" || len(view.Slots) != 1 || len(view.Sales) != 1 {
			t.Fatalf("refreshed selected market view %d is malformed: %+v", i, view)
		}
	}
	now := time.Now().UnixMilli()
	if selectedCache.NextRefreshTimeMillis <= now ||
		selectedCache.NextRefreshTimeMillis > time.Now().Add(3*time.Minute).UnixMilli() {
		t.Fatalf("selected market next refresh time %d is not about two minutes ahead", selectedCache.NextRefreshTimeMillis)
	}

	otherCacheAfter, err := store.GetFragment(worldMarketCacheKey(otherPlayer.UUID))
	if err != nil {
		t.Fatal(err)
	}
	if otherCacheAfter != otherCacheBefore {
		t.Fatal("refreshing the selected player's market changed another player's market")
	}

	persistedSettings, err := store.getWorldMarketSettings()
	if err != nil {
		t.Fatal(err)
	}
	if persistedSettings.RefreshIntervalMinutes != 2 || persistedSettings.OfferCount != 3 {
		t.Fatalf("persisted market settings = %+v, want 2 minutes and 3 offers", persistedSettings)
	}
}

func TestAdminPanelKeepsSaveManagementVisible(t *testing.T) {
	_, mux := newAdminTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("admin panel status = %d, want %d", response.Code, http.StatusOK)
	}

	html := response.Body.String()
	saveSection := strings.Index(html, "<section><h2>Sauvegarde sélectionnée</h2>")
	marketSection := strings.Index(html, "<section><h2>Marché mondial</h2>")
	if saveSection < 0 || marketSection < 0 || saveSection > marketSection {
		t.Fatal("save section should appear before the market controls")
	}
	if !strings.Contains(html, "Télécharger la sauvegarde") || !strings.Contains(html, "Restaurer un fichier") {
		t.Fatal("admin panel is missing backup or restore controls")
	}
}
