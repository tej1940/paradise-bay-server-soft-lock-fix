package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxBackupBytes = 32 << 20

var errDeviceIDAlreadyLinked = errors.New("this device ID is already linked to another player")

type playerSummary struct {
	UUID                 string  `json:"uuid"`
	Alias                string  `json:"alias"`
	Z2DID                string  `json:"z2did"`
	Nanopods             int64   `json:"nanopods"`
	Gems                 int64   `json:"gems"`
	LifetimeValueDollars float64 `json:"lifetimeValueDollars"`
	LeaderboardTier      string  `json:"leaderboardTier"`
	FragmentCount        int     `json:"fragmentCount"`
	LastSavedAt          string  `json:"lastSavedAt"`
}

type fragmentSummary struct {
	Key       string `json:"key"`
	Hash      string `json:"hash"`
	UpdatedAt string `json:"updatedAt"`
}

type playerBackup struct {
	Version    int                `json:"version"`
	ExportedAt time.Time          `json:"exportedAt"`
	Player     Player             `json:"player"`
	Fragments  []DocumentFragment `json:"fragments"`
}

func registerAdminRoutes(mux *http.ServeMux, store *Store) {
	mux.HandleFunc("GET /admin", serveAdminPanel)
	mux.HandleFunc("GET /admin/", serveAdminPanel)
	mux.HandleFunc("GET /admin/api/game/world-snow", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		settings, err := store.getWorldSnowSettings()
		if err != nil {
			log.Printf("admin world snow settings: failed to load client composition: %v", err)
			writeAdminError(w, http.StatusServiceUnavailable, "Could not read the game composition. Check that the AppxPackageExtracted folder exists.")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("PUT /admin/api/game/world-snow", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if input.Enabled == nil {
			writeAdminError(w, http.StatusBadRequest, "The enabled field is required.")
			return
		}
		settings := worldSnowSettings{Enabled: *input.Enabled}
		if err := store.setWorldSnowEnabled(settings.Enabled); err != nil {
			log.Printf("admin world snow settings: failed to update client composition: %v", err)
			writeAdminError(w, http.StatusInternalServerError, "Could not update ground snow. The composition file and its original backup must be available.")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("GET /admin/api/game/build-menu", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		settings, err := store.getBuildMenuModeSettings()
		if err != nil {
			log.Printf("admin build menu settings: failed to load client compositions: %v", err)
			writeAdminError(w, http.StatusServiceUnavailable, "Could not read the build menu catalog or game compositions. Check that the AppxPackageExtracted folder exists.")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("PUT /admin/api/game/build-menu", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input struct {
			Mode *buildMenuMode `json:"mode"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if input.Mode == nil || !isValidBuildMenuMode(*input.Mode) {
			writeAdminError(w, http.StatusBadRequest, "Choose default, all-hidden, or event-decorations.")
			return
		}
		settings, err := store.setBuildMenuMode(*input.Mode)
		if err != nil {
			log.Printf("admin build menu settings: failed to update client compositions: %v", err)
			writeAdminError(w, http.StatusInternalServerError, "Could not update the build menu. Check that the game files are writable and the original backups are intact.")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("GET /admin/api/game/prices", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		categories, err := store.getGamePriceCategories()
		if err != nil {
			log.Printf("admin game prices: failed to load client compositions: %v", err)
			writeAdminError(w, http.StatusServiceUnavailable, "Could not read the game catalog or compositions. Check that the AppxPackageExtracted folder exists.")
			return
		}
		writeJSON(w, http.StatusOK, categories)
	})
	mux.HandleFunc("PUT /admin/api/game/prices/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input gamePriceUpdate
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if input.PurchaseGold == nil && input.HurryAmount == nil {
			writeAdminError(w, http.StatusBadRequest, "Provide at least one price to update.")
			return
		}
		if !isSafeCompositionName(r.PathValue("name")) ||
			(input.PurchaseGold != nil && (*input.PurchaseGold < 0 || *input.PurchaseGold > maxGamePrice)) ||
			(input.HurryAmount != nil && (*input.HurryAmount < 0 || *input.HurryAmount > maxGamePrice)) ||
			(strings.HasPrefix(r.PathValue("name"), "clearables_") && input.HurryAmount != nil) {
			writeAdminError(w, http.StatusBadRequest, "Composition name or price is invalid.")
			return
		}
		if err := store.setGameItemPrice(r.PathValue("name"), input); err != nil {
			log.Printf("admin game prices: failed to update %s: %v", r.PathValue("name"), err)
			writeAdminError(w, http.StatusInternalServerError, "Could not save this item's prices. Check that the game files are writable and the original backups are intact.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /admin/api/game/prices/{name}/reset", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		restored, err := store.resetGameItemPrice(r.PathValue("name"))
		if err != nil {
			log.Printf("admin game prices: failed to reset %s: %v", r.PathValue("name"), err)
			writeAdminError(w, http.StatusInternalServerError, "Could not restore this item's original prices.")
			return
		}
		if !restored {
			writeAdminError(w, http.StatusNotFound, "This item has no saved price changes to reset.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /admin/api/game/prices/reset-all", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		restored, err := store.resetAllGamePrices()
		if err != nil {
			log.Printf("admin game prices: failed to reset all prices: %v", err)
			writeAdminError(w, http.StatusInternalServerError, "Could not restore the original prices.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"restored": restored})
	})
	mux.HandleFunc("GET /admin/assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		name := r.PathValue("name")
		if !isSafeCompositionName(strings.TrimSuffix(name, filepath.Ext(name))) ||
			!strings.EqualFold(filepath.Ext(name), ".png") {
			http.NotFound(w, r)
			return
		}
		if store.appxPackageDir == "" {
			http.NotFound(w, r)
			return
		}
		dataDir := filepath.Join(store.appxPackageDir, "bundle", "data")
		assetPath := filepath.Join(dataDir, name)
		data, err := os.ReadFile(assetPath)
		if errors.Is(err, os.ErrNotExist) {
			assetPath = filepath.Join(dataDir, strings.TrimSuffix(name, filepath.Ext(name))+".dds")
			data, err = os.ReadFile(assetPath)
			if err == nil {
				data, err = decodeDDSIcon(data)
			}
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(data); err != nil {
			log.Printf("admin game prices: failed to write image %s: %v", name, err)
		}
	})
	mux.HandleFunc("GET /admin/api/world-market/settings", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		settings, err := store.getWorldMarketSettings()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "Could not load market settings.")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("PUT /admin/api/world-market/settings", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var settings worldMarketSettings
		if err := decodeAdminJSON(w, r, &settings); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateWorldMarketSettings(settings); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.setWorldMarketSettings(settings); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "Could not save market settings.")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	})
	mux.HandleFunc("GET /admin/api/players", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		players, err := store.ListPlayers()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "Could not load players.")
			return
		}
		summaries := make([]playerSummary, 0, len(players))
		for _, player := range players {
			fragments, err := store.GetPlayerFragments(player.UUID)
			if err != nil {
				writeAdminError(w, http.StatusInternalServerError, "Could not load player saves.")
				return
			}
			summaries = append(summaries, summarizePlayer(player, fragments))
		}
		writeJSON(w, http.StatusOK, summaries)
	})
	mux.HandleFunc("GET /admin/api/players/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		player, fragments, ok := loadAdminPlayer(w, store, r.PathValue("uuid"))
		if !ok {
			return
		}
		items := make([]fragmentSummary, 0, len(fragments))
		for _, fragment := range fragments {
			items = append(items, fragmentSummary{
				Key:       fragment.Key,
				Hash:      fragment.Hash,
				UpdatedAt: fragment.UpdatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"player":    summarizePlayer(player, fragments),
			"fragments": items,
		})
	})
	mux.HandleFunc("GET /admin/api/players/{uuid}/inventory", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		inventory, err := store.getPlayerInventory(r.PathValue("uuid"))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
		case errors.Is(err, errPlayerInventoryUnavailable):
			writeAdminError(w, http.StatusConflict, "L’inventaire du joueur n’est pas initialisé ou son format n’est pas reconnu. Connecte ce joueur au jeu une première fois, puis réessaie.")
		case err != nil:
			log.Printf("admin inventory: failed to load player %s: %v", r.PathValue("uuid"), err)
			writeAdminError(w, http.StatusServiceUnavailable, "Impossible de charger l’inventaire. Vérifie que les catalogues extraits du jeu sont disponibles.")
		default:
			writeJSON(w, http.StatusOK, inventory)
		}
	})
	mux.HandleFunc("PUT /admin/api/players/{uuid}/inventory/{itemID}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input struct {
			Amount *int64 `json:"amount"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if input.Amount == nil || *input.Amount < 0 || *input.Amount > maxInventoryAmount || !isSafeCompositionName(r.PathValue("itemID")) {
			writeAdminError(w, http.StatusBadRequest, "Choisis un objet et une quantité comprise entre 0 et 1 000 000 000.")
			return
		}
		err := store.setPlayerInventoryItem(r.PathValue("uuid"), r.PathValue("itemID"), *input.Amount)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
		case errors.Is(err, errInventoryItemNotFound):
			writeAdminError(w, http.StatusNotFound, "Cet objet n’est ni dans le catalogue de stockage ni dans l’inventaire du joueur.")
		case errors.Is(err, errPlayerOnline):
			writeAdminError(w, http.StatusConflict, "Ferme le jeu avant de modifier cet inventaire pour éviter de perdre les changements.")
		case errors.Is(err, errPlayerInventoryUnavailable):
			writeAdminError(w, http.StatusConflict, "L’inventaire du joueur n’est pas initialisé ou son format n’est pas reconnu.")
		case err != nil:
			log.Printf("admin inventory: failed to update %s for player %s: %v", r.PathValue("itemID"), r.PathValue("uuid"), err)
			writeAdminError(w, http.StatusInternalServerError, "Impossible d’enregistrer cet objet.")
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("PUT /admin/api/players/{uuid}/currencies", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input struct {
			Nanopods *int64 `json:"nanopods"`
			Gems     *int64 `json:"gems"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if (input.Nanopods == nil && input.Gems == nil) ||
			(input.Nanopods != nil && (*input.Nanopods < 0 || *input.Nanopods > maxInventoryAmount)) ||
			(input.Gems != nil && (*input.Gems < 0 || *input.Gems > maxInventoryAmount)) {
			writeAdminError(w, http.StatusBadRequest, "Indique au moins un solde compris entre 0 et 1 000 000 000.")
			return
		}
		player, err := store.setPlayerCurrencies(r.PathValue("uuid"), input.Nanopods, input.Gems)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
		case errors.Is(err, errPlayerOnline):
			writeAdminError(w, http.StatusConflict, "Ferme le jeu avant de modifier cet inventaire pour éviter de perdre les changements.")
		case err != nil:
			log.Printf("admin inventory: failed to update currencies for player %s: %v", r.PathValue("uuid"), err)
			writeAdminError(w, http.StatusInternalServerError, "Impossible d’enregistrer les monnaies du joueur.")
		default:
			writeJSON(w, http.StatusOK, map[string]int64{"nanopods": player.Nanopods, "gems": player.Gems})
		}
	})
	mux.HandleFunc("PUT /admin/api/players/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input struct {
			Alias string `json:"alias"`
			Z2DID string `json:"z2did"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		input.Alias = strings.TrimSpace(input.Alias)
		input.Z2DID = strings.TrimSpace(input.Z2DID)
		if input.Alias == "" || len(input.Alias) > 100 || len(input.Z2DID) > 256 {
			writeAdminError(w, http.StatusBadRequest, "The alias must be 1 to 100 characters and the device ID must be no more than 256 characters.")
			return
		}
		err := store.SetPlayerIdentity(r.PathValue("uuid"), input.Alias, input.Z2DID)
		switch {
		case errors.Is(err, errDeviceIDAlreadyLinked):
			writeAdminError(w, http.StatusConflict, err.Error())
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Player not found.")
		case err != nil:
			writeAdminError(w, http.StatusInternalServerError, "Could not update the player.")
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("DELETE /admin/api/players/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		if err := store.DeletePlayer(r.PathValue("uuid")); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, http.StatusNotFound, "Player not found.")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "Could not delete the player.")
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /admin/api/players/{uuid}/world-market/refresh", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		playerUUID := r.PathValue("uuid")
		err := store.refreshWorldMarketCache(playerUUID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Player not found.")
		case err != nil:
			writeAdminError(w, http.StatusInternalServerError, "Could not refresh this player's market.")
		default:
			activeSessions, err := store.pushWorldMarketCacheUpdate(playerUUID)
			if err != nil {
				log.Printf("admin world market refresh: failed to notify player %s: %v", playerUUID, err)
				writeAdminError(w, http.StatusInternalServerError, "Market refreshed, but could not notify the connected client.")
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"ok":             true,
				"activeSessions": activeSessions,
			})
		}
	})
	mux.HandleFunc("POST /admin/api/players/{uuid}/skip-seal-house-tutorial", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		err := store.SkipDiveTutorial(r.PathValue("uuid"))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Player not found.")
		case err != nil:
			writeAdminError(w, http.StatusInternalServerError, "Could not update mission progress.")
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("GET /admin/api/players/{uuid}/backup", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		player, fragments, ok := loadAdminPlayer(w, store, r.PathValue("uuid"))
		if !ok {
			return
		}
		backup := playerBackup{
			Version:    1,
			ExportedAt: time.Now().UTC(),
			Player:     player,
			Fragments:  fragments,
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="paradise-bay-%s.json"`, player.UUID))
		writeJSON(w, http.StatusOK, backup)
	})
	mux.HandleFunc("POST /admin/api/players/{uuid}/restore", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		uuid := r.PathValue("uuid")
		currentPlayer, err := store.GetPlayerByCID(uuid)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, http.StatusNotFound, "Player not found.")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "Could not load the player.")
			}
			return
		}
		var backup playerBackup
		if err := decodeAdminJSON(w, r, &backup); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if backup.Version != 1 || backup.Player.UUID != uuid || strings.TrimSpace(backup.Player.Alias) == "" || backup.Fragments == nil {
			writeAdminError(w, http.StatusBadRequest, "This backup is not compatible with this player.")
			return
		}
		// A restore must not undo a device relink performed after the backup was made.
		backup.Player.Z2DID = currentPlayer.Z2DID
		backup.Player.CurrentTcID = currentPlayer.CurrentTcID
		seen := make(map[string]struct{}, len(backup.Fragments))
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		for i := range backup.Fragments {
			fragment := &backup.Fragments[i]
			if fragment.Key == "" || len(fragment.Key) > 2048 || !strings.Contains(fragment.Key, uuid) || !json.Valid([]byte(fragment.BlobJSON)) {
				writeAdminError(w, http.StatusBadRequest, "The backup contains an invalid fragment or one belonging to another player.")
				return
			}
			if _, duplicate := seen[fragment.Key]; duplicate {
				writeAdminError(w, http.StatusBadRequest, "The backup contains duplicate fragments.")
				return
			}
			seen[fragment.Key] = struct{}{}
			hash := sha256.Sum256([]byte(fragment.BlobJSON))
			fragment.Hash = hex.EncodeToString(hash[:])
			fragment.UpdatedAt = now
		}
		if err := store.ReplacePlayerSave(backup.Player, backup.Fragments); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, http.StatusNotFound, "Player not found.")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "Restore failed; no changes were applied.")
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

func summarizePlayer(player Player, fragments []DocumentFragment) playerSummary {
	summary := playerSummary{
		UUID:                 player.UUID,
		Alias:                player.Alias,
		Z2DID:                player.Z2DID,
		Nanopods:             player.Nanopods,
		Gems:                 player.Gems,
		LifetimeValueDollars: player.LifetimeValueDollars,
		LeaderboardTier:      player.LeaderboardTier,
		FragmentCount:        len(fragments),
	}
	for _, fragment := range fragments {
		if fragment.UpdatedAt > summary.LastSavedAt {
			summary.LastSavedAt = fragment.UpdatedAt
		}
	}
	return summary
}

func loadAdminPlayer(w http.ResponseWriter, store *Store, uuid string) (Player, []DocumentFragment, bool) {
	player, err := store.GetPlayerByCID(uuid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAdminError(w, http.StatusNotFound, "Player not found.")
		} else {
			writeAdminError(w, http.StatusInternalServerError, "Could not load the player.")
		}
		return Player{}, nil, false
	}
	fragments, err := store.GetPlayerFragments(uuid)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "Could not load the save.")
		return Player{}, nil, false
	}
	return player, fragments, true
}

func requireLocalAdmin(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		http.Error(w, "Admin panel is available only from this computer.", http.StatusForbidden)
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, destination interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Printf("failed to write admin response: %v\n", err)
	}
}

func serveAdminPanel(w http.ResponseWriter, r *http.Request) {
	if !requireLocalAdmin(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_, _ = io.WriteString(w, adminPanelHTML)
}

const adminPanelHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Paradise Bay — Administration</title>
<style>
:root{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;color:#193446;background:#eef5f7;font-synthesis:none}
*{box-sizing:border-box}body{margin:0}header{background:#145b73;color:white;padding:22px max(24px,calc((100% - 1120px)/2))}
header h1{margin:0;font-size:1.55rem}header p{margin:5px 0 0;color:#d4edf3}
main{position:relative;max-width:1120px;margin:26px auto;padding:0 20px;display:grid;grid-template-columns:minmax(0,1.2fr) minmax(0,.8fr);gap:20px;align-items:start}
.module-column{display:flex;flex-direction:column;gap:20px;min-width:0}.module-column>section{align-self:stretch}
section{position:relative;min-width:0;align-self:start;background:white;border:1px solid #d5e2e7;border-radius:12px;padding:20px;box-shadow:0 3px 12px #1745590d}
h2{font-size:1.12rem;margin:0 0 14px}button,.button{border:0;border-radius:7px;background:#176b83;color:white;padding:9px 13px;font:inherit;cursor:pointer;text-decoration:none;display:inline-block}
button.secondary,.button.secondary{background:#e5eff2;color:#234957}button.danger{background:#a13b34}
button:disabled{opacity:.55;cursor:wait}.toolbar,.module-heading{display:flex;justify-content:space-between;align-items:center;gap:10px;margin-bottom:12px}
.module-heading h2{margin:0}.module-actions{display:flex;align-items:center;gap:8px}.module-handle{background:#e5eff2;color:#234957;padding:6px 9px;font-size:.82rem;cursor:grab}.module-handle:active{cursor:grabbing}.module-handle:focus-visible{outline:3px solid #68aebe;outline-offset:2px}
section.dragging{opacity:.5}section.drop-target{outline:2px dashed #176b83;outline-offset:3px}
table{width:100%;table-layout:fixed;border-collapse:collapse;font-size:.92rem}th,td{text-align:left;padding:10px 8px;border-bottom:1px solid #e6eef1;overflow-wrap:anywhere}th{color:#58717c;font-weight:600}
tr.player{cursor:pointer}tr.player:hover,tr.player.selected{background:#edf7f8}.muted{color:#607985;font-size:.88rem}.empty{padding:18px;text-align:center;color:#607985}
label{display:block;font-weight:600;font-size:.9rem;margin:13px 0 5px}input{width:100%;border:1px solid #bcced5;border-radius:7px;padding:10px;font:inherit}
.toggle-row{display:flex;align-items:center;gap:9px;margin:12px 0}.toggle-row input{width:auto;margin:0}
.settings-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}
.actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:16px}.notice{margin:12px 0;padding:10px;border-radius:7px;background:#edf7f8}.notice.error{background:#fbeceb;color:#8d2b25}
.fragments{max-height:250px;overflow:auto;margin-top:8px}.fragments li{font-size:.82rem;overflow-wrap:anywhere;margin:7px 0}
#details{min-width:0;overflow-wrap:anywhere}
.price-toolbar{display:flex;align-items:end;justify-content:space-between;gap:12px;flex-wrap:wrap}.price-toolbar label{min-width:220px;flex:1}
.price-rail{display:flex;gap:12px;overflow-x:auto;padding:8px 2px 14px;cursor:grab;user-select:none;touch-action:pan-y;overscroll-behavior-x:contain;-webkit-overflow-scrolling:touch}
.price-rail.dragging{cursor:grabbing}.price-card{flex:0 0 var(--price-card-width,220px);border:1px solid #d5e2e7;border-radius:10px;padding:12px;background:#fbfdfe}
.price-card img{display:block;width:var(--price-image-size,72px);height:var(--price-image-size,72px);object-fit:contain;margin:0 auto 8px;pointer-events:none}.price-card strong{display:block;min-height:40px;overflow-wrap:anywhere}
.price-card .price-id{min-height:30px;font-size:.72rem;overflow-wrap:anywhere}.price-card label{margin:8px 0 3px}.price-card input{padding:7px}.price-card .actions{margin-top:10px}.price-card .actions button{flex:1;padding:7px 8px;font-size:.82rem}
.price-size-control{max-width:480px;margin:12px 0}.price-size-control label{margin:0 0 4px}.price-size-row{display:flex;align-items:center;gap:12px}.price-size-row input[type=range]{padding:0;accent-color:#176b83}.price-size-row output{min-width:54px;text-align:right;font-size:.88rem;color:#607985}
.price-scroll-hint{margin:8px 0 0}
.inventory-toolbar,.inventory-currency-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}
.inventory-toolbar label,.inventory-currency-grid label{min-width:0}
.inventory-card .inventory-id{min-height:30px}
.inventory-card label{margin:7px 0 4px}
.inventory-card .actions{margin-top:8px}
.inventory-card .actions button{flex:1;padding:7px 8px}
.inventory-status{margin:10px 0 0}
#game-prices,#player-inventory{grid-column:1/-1}
.module-resized{overflow:auto}.module-column>section.module-resized{align-self:flex-start}
.module-resize-handle{position:absolute;right:5px;bottom:5px;z-index:2;width:20px;height:20px;padding:0;border:0;border-radius:4px;background:linear-gradient(135deg,transparent 48%,#75909b 49% 55%,transparent 56% 68%,#75909b 69% 75%,transparent 76%);cursor:nwse-resize;touch-action:none}
.module-resize-handle:hover{background-color:#e5eff2}.module-resize-handle:focus-visible{outline:2px solid #176b83;outline-offset:1px}
.module-resized.resizing{user-select:none}
#columnResizeHandle{position:absolute;top:0;z-index:5;width:16px;padding:0;transform:translateX(-50%);border:0;border-radius:4px;background:transparent;color:#607985;cursor:col-resize;touch-action:none}
#columnResizeHandle:after{content:"";position:absolute;top:50%;left:50%;width:4px;height:42px;transform:translate(-50%,-50%);border-left:1px solid #8da7b2;border-right:1px solid #8da7b2;border-radius:2px}
#columnResizeHandle:hover:after,#columnResizeHandle:focus-visible:after{background:#e5eff2;border-color:#176b83}
#columnResizeHandle:focus-visible{outline:2px solid #176b83;outline-offset:1px}
.module-status{margin:5px 0 0;color:#d4edf3;font-size:.8rem}
@media(max-width:760px){main{grid-template-columns:1fr}.module-column{display:contents}.settings-grid{grid-template-columns:1fr}table{font-size:.82rem}th,td{padding:8px 4px}}
</style>
</head>
<body>
<header><h1>Paradise Bay — Administration</h1><p>Manage players and retrieve saves after reinstalling.</p><p id="moduleOrderStatus" class="module-status" role="status" aria-live="polite"></p></header>
<main id="adminModules">
<div class="module-column" id="primaryModules" data-module-column>
<section data-module-id="players"><div class="toolbar"><h2>Players</h2><div class="module-actions"><button type="button" class="module-handle" draggable="true" data-module-handle aria-label="Move Players module" title="Drag to move, or use arrow keys">Move</button><button class="secondary" id="refresh">Refresh</button></div></div>
<div id="status" class="muted" role="status">Loading…</div>
<table><thead><tr><th>Alias</th><th>Device ID</th><th>Fragments</th></tr></thead><tbody id="players"></tbody></table>
</section>
<section data-module-id="world-market"><div class="module-heading"><h2>World Market</h2><button type="button" class="module-handle" draggable="true" data-module-handle aria-label="Move World Market module" title="Drag to move, or use arrow keys">Move</button></div>
<p class="muted">These settings apply globally at the next refresh. The button immediately regenerates the selected player's cache; the updated cache will be used the next time the game requests the market (you may need to close and reopen the market).</p>
<form id="marketSettings">
<div class="settings-grid">
<div><label for="marketInterval">Refresh interval (minutes)</label><input id="marketInterval" type="number" min="1" max="1440" step="1" required></div>
<div><label for="marketOfferCount">Offers per refresh</label><input id="marketOfferCount" type="number" min="1" max="27" step="1" required></div>
</div>
<div class="actions"><button type="submit">Save settings</button><button type="button" class="secondary" id="refreshMarket" disabled>Refresh selected player's shop</button></div>
</form>
<div id="marketStatus" class="muted" role="status">Loading market settings…</div>
</section>
<section data-module-id="build-menu"><div class="module-heading"><h2>Hidden Build Menu Items</h2><button type="button" class="module-handle" draggable="true" data-module-handle aria-label="Move Hidden Build Menu Items module" title="Drag to move, or use arrow keys">Move</button></div>
<p class="muted">Show hidden catalog items, or only hidden event decorations. Existing prices are kept; items without a build cost get an instant 1,000-gold cost. Originals are saved outside the game package. Changes take effect after restarting the game. Disabled features and items missing from the catalog are not enabled or added.</p>
<form id="buildMenuSettings">
<label for="buildMenuMode">Visibility</label>
<select id="buildMenuMode">
<option value="default">Default (restore original visibility and prices)</option>
<option value="all-hidden">Show all hidden items</option>
<option value="event-decorations">Show event decorations only</option>
</select>
<div class="actions"><button type="submit">Apply visibility and prices</button></div>
</form>
<div id="buildMenuStatus" class="muted" role="status">Loading build menu setting…</div>
</section>
</div>
<div class="module-column" id="secondaryModules" data-module-column>
<section class="save-section" data-module-id="player-save"><div class="module-heading"><h2>Selected Player Save</h2><button type="button" class="module-handle" draggable="true" data-module-handle aria-label="Move Selected Player Save module" title="Drag to move, or use arrow keys">Move</button></div><div id="details" class="muted">Select a player to view their save.</div></section>
<section data-module-id="island-appearance"><div class="module-heading"><h2>Island Appearance</h2><button type="button" class="module-handle" draggable="true" data-module-handle aria-label="Move Island Appearance module" title="Drag to move, or use arrow keys">Move</button></div>
<p class="muted">Turn ground snow on or off on the main island. Changes take effect the next time you launch the game.</p>
<form id="worldSnowSettings">
<label class="toggle-row" for="worldSnowEnabled"><input id="worldSnowEnabled" type="checkbox">Ground snow enabled</label>
<div class="actions"><button type="submit">Save</button></div>
</form>
<div id="worldSnowStatus" class="muted" role="status">Loading snow setting…</div>
</section>
</div>
<button id="columnResizeHandle" type="button" role="separator" aria-orientation="vertical" aria-valuemin="25" aria-valuemax="75" aria-label="Redimensionner les colonnes" title="Glisser pour modifier la largeur des colonnes" tabindex="0"></button>
<section id="player-inventory"><div class="module-heading"><h2>Inventaire</h2><span id="inventoryPlayerName" class="muted">Aucun joueur sélectionné</span></div>
<p class="muted">Sélectionne un joueur dans la liste pour gérer ses ressources. Arrête le jeu et télécharge une sauvegarde avant toute modification ; les changements seront chargés au prochain lancement.</p>
<form id="inventoryCurrencies"><div class="inventory-currency-grid"><div><label for="inventoryNanopods">Or / nanopods</label><input id="inventoryNanopods" type="number" min="0" max="1000000000" step="1" disabled></div><div><label for="inventoryGems">Gemmes</label><input id="inventoryGems" type="number" min="0" max="1000000000" step="1" disabled></div></div><div class="actions"><button type="submit" disabled>Enregistrer les monnaies</button></div></form>
<div class="inventory-toolbar"><label for="inventoryCategory">Catégorie<select id="inventoryCategory" disabled><option value="all">Toutes les catégories</option></select></label><label for="inventorySearch">Rechercher<input id="inventorySearch" type="search" placeholder="Nom ou identifiant" disabled></label></div>
<div class="price-size-control"><label for="inventoryCardSize">Taille des cartes et des images</label><div class="price-size-row"><input id="inventoryCardSize" type="range" min="160" max="360" step="20" value="220"><output id="inventoryCardSizeValue" for="inventoryCardSize">220 px</output></div></div>
<p class="muted price-scroll-hint">Fais défiler horizontalement ou clique-glisse les cartes vers la gauche ou la droite.</p>
<div id="inventoryItems" class="price-rail" tabindex="0" aria-label="Inventaire du joueur">Sélectionne un joueur pour afficher son inventaire.</div>
<div id="inventoryStatus" class="muted inventory-status" role="status" aria-live="polite">Sélectionne un joueur dans la liste pour gérer son inventaire.</div>
</section>
<section id="game-prices"><div class="module-heading"><h2>Prix du jeu</h2></div>
<p class="muted">Modifie le prix de base (premier palier) en or, le coût d’accélération (skip) en gemmes et le déblayage des arbres et obstacles. Redémarre le jeu pour appliquer les changements.</p>
<div class="price-toolbar"><label for="priceCategory">Catégorie du jeu<select id="priceCategory"></select></label><button type="button" class="secondary" id="resetAllPrices">Restaurer tous les prix</button></div>
<div class="price-size-control"><label for="priceCardSize">Taille des cartes et des images</label><div class="price-size-row"><input id="priceCardSize" type="range" min="160" max="360" step="20" value="220"><output id="priceCardSizeValue" for="priceCardSize">220 px</output></div></div>
<p class="muted price-scroll-hint">Fais défiler horizontalement ou clique-glisse les cartes vers la gauche ou la droite.</p>
<div id="priceRail" class="price-rail" tabindex="0" aria-label="Prix des objets de la catégorie"></div>
<div id="priceStatus" class="muted" role="status">Chargement des prix…</div>
</section>
</main>
<script>
const playersEl=document.querySelector("#players"),detailsEl=document.querySelector("#details"),statusEl=document.querySelector("#status"),marketStatusEl=document.querySelector("#marketStatus"),worldSnowStatusEl=document.querySelector("#worldSnowStatus"),buildMenuStatusEl=document.querySelector("#buildMenuStatus"),priceStatusEl=document.querySelector("#priceStatus"),priceRailEl=document.querySelector("#priceRail"),priceCategoryEl=document.querySelector("#priceCategory"),priceCardSizeEl=document.querySelector("#priceCardSize"),priceCardSizeValueEl=document.querySelector("#priceCardSizeValue"),refreshMarketButton=document.querySelector("#refreshMarket"),adminModulesEl=document.querySelector("#adminModules"),moduleOrderStatusEl=document.querySelector("#moduleOrderStatus"),primaryModulesEl=document.querySelector("#primaryModules"),secondaryModulesEl=document.querySelector("#secondaryModules"),columnResizeHandleEl=document.querySelector("#columnResizeHandle");
const moduleLayoutKey="paradiseBayAdminModuleLayout";
const priceCardSizeKey="paradiseBayPriceCardWidth";
const adminColumnRatioKey="paradiseBayAdminColumnRatio";
const inventoryRailEl=document.querySelector("#inventoryItems"),inventoryCardSizeEl=document.querySelector("#inventoryCardSize"),inventoryCardSizeValueEl=document.querySelector("#inventoryCardSizeValue");
let selected="";
let gamePriceCategories=[];
function esc(value){return String(value||"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]))}
async function request(url,options){const response=await fetch(url,options);const data=await response.json();if(!response.ok)throw new Error(data.error||"Server error");return data}
function moduleColumns(){return Array.from(adminModulesEl.querySelectorAll(":scope > .module-column"))}
function modulePanels(){return Array.from(adminModulesEl.querySelectorAll(".module-column > section[data-module-id]"))}
function saveModuleLayout(){for(const panel of resizableModules())applyStoredModuleSize(panel);const layout={};for(const column of moduleColumns())layout[column.id]=Array.from(column.querySelectorAll(":scope > section[data-module-id]")).map(panel=>panel.dataset.moduleId);try{localStorage.setItem(moduleLayoutKey,JSON.stringify(layout));moduleOrderStatusEl.textContent="Module layout saved in this browser."}catch(error){moduleOrderStatusEl.textContent="Could not save module layout: "+error.message}}
function restoreModuleLayout(){let saved;try{const raw=localStorage.getItem(moduleLayoutKey);if(!raw)return;saved=JSON.parse(raw)}catch(error){moduleOrderStatusEl.textContent="Could not load saved module layout: "+error.message;return}if(!saved||typeof saved!=="object"||Array.isArray(saved)){moduleOrderStatusEl.textContent="Saved module layout is invalid; using the default layout.";return}const panels=new Map(modulePanels().map(panel=>[panel.dataset.moduleId,panel]));const defaults=new Map(modulePanels().map(panel=>[panel.dataset.moduleId,panel.parentElement]));const placed=new Set();for(const column of moduleColumns()){const ids=saved[column.id];if(Array.isArray(ids))for(const id of ids){const panel=panels.get(id);if(typeof id==="string"&&panel&&!placed.has(id)){column.append(panel);placed.add(id)}}}for(const [id,panel] of panels)if(!placed.has(id))defaults.get(id).append(panel)}
function placeModule(panel,column,target,before){if(target){column.insertBefore(panel,before?target:target.nextElementSibling)}else{column.append(panel)}saveModuleLayout()}
function moveModuleByKey(panel,direction,handle){const column=panel.parentElement;const columns=moduleColumns();if(direction==="ArrowUp"||direction==="ArrowDown"){const siblings=Array.from(column.querySelectorAll(":scope > section[data-module-id]"));const index=siblings.indexOf(panel);const targetIndex=index+(direction==="ArrowUp"?-1:1);if(targetIndex<0||targetIndex>=siblings.length)return;const reference=direction==="ArrowUp"?siblings[targetIndex]:siblings[targetIndex+1]||null;column.insertBefore(panel,reference)}else{const index=columns.indexOf(column);const targetIndex=index+(direction==="ArrowLeft"?-1:1);if(targetIndex<0||targetIndex>=columns.length)return;const destination=columns[targetIndex];const siblings=Array.from(destination.querySelectorAll(":scope > section[data-module-id]"));destination.insertBefore(panel,siblings[Math.min(siblings.length,Array.from(column.querySelectorAll(":scope > section[data-module-id]")).indexOf(panel))]||null)}saveModuleLayout();handle.focus()}
function setDraggedModule(panel){for(const item of modulePanels())item.classList.remove("dragging","drop-target");return panel}
const moduleSizeKey="paradiseBayAdminModuleSizes";
let moduleSizes={},resizingModule=null;
function resizableModules(){return Array.from(adminModulesEl.querySelectorAll(".module-column > section[data-module-id],#game-prices,#player-inventory"))}
function moduleSizeID(panel){return panel.dataset.moduleId||panel.id}
function moduleMaxWidth(panel){const column=panel.closest(".module-column");return column&&getComputedStyle(column).display!=="contents"?column.clientWidth:adminModulesEl.clientWidth}
function applyStoredModuleSize(panel){const size=moduleSizes[moduleSizeID(panel)];if(!size||!Number.isFinite(size.width)||!Number.isFinite(size.height))return;const maxWidth=moduleMaxWidth(panel);if(maxWidth<=0)return;const minWidth=Math.min(220,maxWidth),maxHeight=Math.max(200,window.innerHeight*1.5);panel.style.width=Math.max(minWidth,Math.min(maxWidth,size.width))+"px";panel.style.height=Math.max(160,Math.min(maxHeight,size.height))+"px";panel.classList.add("module-resized")}
function saveModuleSizes(){try{localStorage.setItem(moduleSizeKey,JSON.stringify(moduleSizes))}catch(error){moduleOrderStatusEl.textContent="Module sizes could not be saved: "+error.message}}
function finishModuleResize(){if(!resizingModule)return;resizingModule.panel.classList.remove("resizing");resizingModule=null;saveModuleSizes()}
function initializeModuleResizing(){try{const saved=localStorage.getItem(moduleSizeKey);if(saved){const parsed=JSON.parse(saved);if(!parsed||typeof parsed!=="object"||Array.isArray(parsed))throw new Error("saved module sizes are invalid");moduleSizes=parsed}}catch(error){moduleOrderStatusEl.textContent="Module sizes could not be loaded: "+error.message;moduleSizes={}}
for(const panel of resizableModules()){applyStoredModuleSize(panel);const handle=document.createElement("button");handle.type="button";handle.className="module-resize-handle";handle.title="Drag to resize this module";handle.setAttribute("aria-label","Resize "+moduleSizeID(panel));panel.append(handle);handle.addEventListener("pointerdown",event=>{if(event.button!==0)return;event.preventDefault();event.stopPropagation();const rect=panel.getBoundingClientRect();resizingModule={panel,handle,pointerId:event.pointerId,startX:event.clientX,startY:event.clientY,startWidth:rect.width,startHeight:rect.height};panel.classList.add("resizing");handle.setPointerCapture(event.pointerId)});handle.addEventListener("pointermove",event=>{if(!resizingModule||event.pointerId!==resizingModule.pointerId)return;const panel=resizingModule.panel,maxWidth=moduleMaxWidth(panel),minWidth=Math.min(220,maxWidth),maxHeight=Math.max(200,window.innerHeight*1.5),width=Math.max(minWidth,Math.min(maxWidth,resizingModule.startWidth+event.clientX-resizingModule.startX)),height=Math.max(160,Math.min(maxHeight,resizingModule.startHeight+event.clientY-resizingModule.startY));panel.style.width=width+"px";panel.style.height=height+"px";panel.classList.add("module-resized");moduleSizes[moduleSizeID(panel)]={width,height};event.preventDefault()});handle.addEventListener("pointerup",finishModuleResize);handle.addEventListener("pointercancel",finishModuleResize);handle.addEventListener("lostpointercapture",finishModuleResize)}}
restoreModuleLayout();
initializeModuleResizing();
let adminColumnRatio=.6,columnResizeDrag=null;
function adminColumnSpace(){const style=getComputedStyle(adminModulesEl),horizontalPadding=parseFloat(style.paddingLeft)+parseFloat(style.paddingRight),gap=parseFloat(style.columnGap)||0;return adminModulesEl.clientWidth-horizontalPadding-gap}
function positionColumnResizeHandle(){const primary=primaryModulesEl.getBoundingClientRect(),secondary=secondaryModulesEl.getBoundingClientRect(),container=adminModulesEl.getBoundingClientRect(),top=Math.min(primary.top,secondary.top),bottom=Math.max(primary.bottom,secondary.bottom);columnResizeHandleEl.style.left=(primary.right+(secondary.left-primary.right)/2-container.left)+"px";columnResizeHandleEl.style.top=(top-container.top)+"px";columnResizeHandleEl.style.height=(bottom-top)+"px"}
function applyAdminColumnRatio(ratio){if(window.innerWidth<=760){adminModulesEl.style.gridTemplateColumns="";columnResizeHandleEl.style.display="none";return}const available=adminColumnSpace();if(available<=0)return;const minimumRatio=Math.min(.5,260/available);adminColumnRatio=Math.max(minimumRatio,Math.min(1-minimumRatio,ratio));adminModulesEl.style.gridTemplateColumns="minmax(260px,"+adminColumnRatio+"fr) minmax(260px,"+(1-adminColumnRatio)+"fr)";columnResizeHandleEl.style.display="block";positionColumnResizeHandle();for(const panel of resizableModules())applyStoredModuleSize(panel);columnResizeHandleEl.setAttribute("aria-valuenow",String(Math.round(adminColumnRatio*100)))}
function saveAdminColumnRatio(){try{localStorage.setItem(adminColumnRatioKey,String(adminColumnRatio));moduleOrderStatusEl.textContent="Column widths saved in this browser."}catch(error){moduleOrderStatusEl.textContent="Column widths could not be saved: "+error.message}}
function initializeColumnResizing(){try{const saved=localStorage.getItem(adminColumnRatioKey);if(saved!==null){const ratio=Number(saved);if(!Number.isFinite(ratio)||ratio<=0||ratio>=1)throw new Error("saved column width is invalid");adminColumnRatio=ratio}}catch(error){moduleOrderStatusEl.textContent="Column widths could not be loaded: "+error.message;adminColumnRatio=.6}
const update=()=>applyAdminColumnRatio(adminColumnRatio);update();window.addEventListener("resize",update);if(typeof ResizeObserver!=="undefined"){const observer=new ResizeObserver(positionColumnResizeHandle);observer.observe(primaryModulesEl);observer.observe(secondaryModulesEl)}columnResizeHandleEl.addEventListener("pointerdown",event=>{if(event.button!==0||window.innerWidth<=760)return;event.preventDefault();columnResizeDrag={pointerId:event.pointerId,startX:event.clientX,startRatio:adminColumnRatio};columnResizeHandleEl.setPointerCapture(event.pointerId)});columnResizeHandleEl.addEventListener("pointermove",event=>{if(!columnResizeDrag||event.pointerId!==columnResizeDrag.pointerId)return;const available=adminColumnSpace();if(available>0)applyAdminColumnRatio(columnResizeDrag.startRatio+(event.clientX-columnResizeDrag.startX)/available);event.preventDefault()});const finish=()=>{if(!columnResizeDrag)return;columnResizeDrag=null;saveAdminColumnRatio()};columnResizeHandleEl.addEventListener("pointerup",finish);columnResizeHandleEl.addEventListener("pointercancel",finish);columnResizeHandleEl.addEventListener("lostpointercapture",finish);columnResizeHandleEl.addEventListener("keydown",event=>{if(event.key!=="ArrowLeft"&&event.key!=="ArrowRight")return;event.preventDefault();applyAdminColumnRatio(adminColumnRatio+(event.key==="ArrowRight" ? 0.02 : -0.02));saveAdminColumnRatio()})}
initializeColumnResizing();
function setStatus(message,error=false){statusEl.textContent=message;statusEl.className=error?"notice error":"muted"}
function setMarketStatus(message,error=false){marketStatusEl.textContent=message;marketStatusEl.className=error?"notice error":"muted"}
function setWorldSnowStatus(message,error=false){worldSnowStatusEl.textContent=message;worldSnowStatusEl.className=error?"notice error":"muted"}
function setBuildMenuStatus(message,error=false){buildMenuStatusEl.textContent=message;buildMenuStatusEl.className=error?"notice error":"muted"}
function setPriceStatus(message,error=false){priceStatusEl.textContent=message;priceStatusEl.className=error?"notice error":"muted"}
const inventoryCategoryNames={storage_barn:"Grange",storage_silo:"Silo",other:"Autres objets"};
let playerInventoryItems=[],playerInventoryUUID="",inventoryLoadSerial=0;
function setInventoryStatus(message,error=false){const status=document.querySelector("#inventoryStatus");if(status){status.textContent=message;status.className=error?"notice error inventory-status":"muted inventory-status"}}
function resetPlayerInventory(message){inventoryLoadSerial++;playerInventoryUUID="";playerInventoryItems=[];inventoryRailEl.replaceChildren();inventoryRailEl.textContent=message;document.querySelector("#inventoryPlayerName").textContent="Aucun joueur sélectionné";document.querySelector("#inventoryNanopods").disabled=true;document.querySelector("#inventoryGems").disabled=true;document.querySelector("#inventoryCurrencies").querySelector("button").disabled=true;document.querySelector("#inventoryCategory").disabled=true;document.querySelector("#inventorySearch").disabled=true;setInventoryStatus(message)}
function renderInventoryItems(){const rail=inventoryRailEl,category=document.querySelector("#inventoryCategory"),search=document.querySelector("#inventorySearch");if(!category||!search)return;rail.replaceChildren();const query=search.value.trim().toLocaleLowerCase(),items=playerInventoryItems.filter(item=>(category.value==="all"||item.category===category.value)&&(!query||item.name.toLocaleLowerCase().includes(query)||item.id.toLocaleLowerCase().includes(query)));if(!items.length){rail.textContent="Aucun objet ne correspond à ces filtres.";return}
for(const item of items){const card=document.createElement("article");card.className="price-card inventory-card";if(item.image){const image=document.createElement("img");image.loading="lazy";image.src="/admin/assets/"+encodeURIComponent(item.image);image.alt=item.name;card.append(image)}const name=document.createElement("strong");name.textContent=item.name;card.append(name);const id=document.createElement("div");id.className="muted price-id inventory-id";id.textContent=item.id;card.append(id);const label=document.createElement("label");label.textContent="Quantité";const input=document.createElement("input");input.type="number";input.min="0";input.max="1000000000";input.step="1";input.value=String(item.amount);input.setAttribute("aria-label","Quantité de "+item.name);label.append(input);card.append(label);const actions=document.createElement("div");actions.className="actions";const button=document.createElement("button");button.type="button";button.textContent="Enregistrer";const uuid=playerInventoryUUID;button.addEventListener("click",async()=>{const amount=Number(input.value);if(uuid!==selected){setInventoryStatus("Sélectionne de nouveau le joueur avant de modifier son inventaire.",true);return}if(!Number.isSafeInteger(amount)||amount<0||amount>1000000000){setInventoryStatus("La quantité doit être comprise entre 0 et 1 000 000 000.",true);return}button.disabled=true;try{await request("/admin/api/players/"+encodeURIComponent(uuid)+"/inventory/"+encodeURIComponent(item.id),{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({amount})});item.amount=amount;if(uuid===playerInventoryUUID)setInventoryStatus("Quantité enregistrée pour "+item.name+".")}catch(error){if(uuid===playerInventoryUUID)setInventoryStatus(error.message,true)}finally{button.disabled=false}});actions.append(button);card.append(actions);rail.append(card)}}
async function loadPlayerInventory(uuid,player){playerInventoryUUID=uuid;const loadSerial=++inventoryLoadSerial;document.querySelector("#inventoryPlayerName").textContent="Inventaire de "+player.alias;const nanopods=document.querySelector("#inventoryNanopods"),gems=document.querySelector("#inventoryGems");nanopods.value=player.nanopods;gems.value=player.gems;nanopods.disabled=false;gems.disabled=false;document.querySelector("#inventoryCurrencies").querySelector("button").disabled=false;setInventoryStatus("Chargement de l’inventaire…");try{const inventory=await request("/admin/api/players/"+encodeURIComponent(uuid)+"/inventory");if(loadSerial!==inventoryLoadSerial||uuid!==selected)return;playerInventoryItems=inventory.items;nanopods.value=inventory.nanopods;gems.value=inventory.gems;const category=document.querySelector("#inventoryCategory"),selectedCategory=category.value;category.replaceChildren();const all=document.createElement("option");all.value="all";all.textContent="Toutes les catégories ("+playerInventoryItems.length+")";category.append(all);const categories=Array.from(new Set(playerInventoryItems.map(item=>item.category))).sort();for(const id of categories){const option=document.createElement("option");option.value=id;option.textContent=(inventoryCategoryNames[id]||id)+" ("+playerInventoryItems.filter(item=>item.category===id).length+")";category.append(option)}if(categories.includes(selectedCategory))category.value=selectedCategory;category.disabled=false;const search=document.querySelector("#inventorySearch");search.disabled=false;category.onchange=renderInventoryItems;search.oninput=renderInventoryItems;renderInventoryItems();setInventoryStatus("Inventaire chargé. Choisis une quantité puis enregistre-la.")}catch(error){if(loadSerial===inventoryLoadSerial&&uuid===selected)setInventoryStatus(error.message,true)}}
function applyPriceCardSize(value){const size=Number(value);if(!Number.isInteger(size)||size<160||size>360){setPriceStatus("La taille doit être comprise entre 160 et 360 px.",true);return false}priceCardSizeEl.value=String(size);priceCardSizeValueEl.value=size+" px";priceCardSizeValueEl.textContent=size+" px";inventoryCardSizeEl.value=String(size);inventoryCardSizeValueEl.value=size+" px";inventoryCardSizeValueEl.textContent=size+" px";for(const rail of [priceRailEl,inventoryRailEl]){rail.style.setProperty("--price-card-width",size+"px");rail.style.setProperty("--price-image-size",Math.round(size*.34)+"px")}return true}
function loadPriceCardSize(){try{const saved=localStorage.getItem(priceCardSizeKey);if(saved!==null&&!applyPriceCardSize(saved))localStorage.removeItem(priceCardSizeKey)}catch(error){setPriceStatus("Impossible de charger la taille des cartes : "+error.message,true)}}
async function refresh(){setStatus("Loading…");try{const players=await request("/admin/api/players");playersEl.replaceChildren();if(!players.length){playersEl.innerHTML='<tr><td colspan="3" class="empty">No players registered.</td></tr>'}
for(const p of players){const tr=document.createElement("tr");tr.className="player"+(p.uuid===selected?" selected":"");tr.innerHTML="<td>"+esc(p.alias)+"<div class=\"muted\">"+esc(p.uuid)+"</div></td><td>"+esc(p.z2did||"—")+"</td><td>"+p.fragmentCount+"</td>";tr.addEventListener("click",()=>showPlayer(p.uuid));playersEl.append(tr)}
setStatus(players.length+" player(s)");}catch(e){setStatus(e.message,true)}}
async function loadMarketSettings(){try{const settings=await request("/admin/api/world-market/settings");document.querySelector("#marketInterval").value=settings.refreshIntervalMinutes;document.querySelector("#marketOfferCount").value=settings.offerCount;setMarketStatus("Settings loaded.")}catch(e){setMarketStatus(e.message,true)}}
async function loadWorldSnowSettings(){try{const settings=await request("/admin/api/game/world-snow");document.querySelector("#worldSnowEnabled").checked=settings.enabled;setWorldSnowStatus("Snow setting loaded.")}catch(e){setWorldSnowStatus(e.message,true)}}
async function loadBuildMenuSettings(){try{const settings=await request("/admin/api/game/build-menu");document.querySelector("#buildMenuMode").value=settings.mode;setBuildMenuStatus("Setting loaded. "+settings.allHiddenCount+" eligible hidden catalog item(s), including "+settings.eventDecorationCount+" event decoration(s).")}catch(e){setBuildMenuStatus(e.message,true)}}
const gameCategoryNames={decorations:"Décorations",fences:"Clôtures",foliage:"Végétation",harvesters:"Récolteurs",houses:"Maisons",pets:"Animaux",seasonal:"Saisonnier",utility:"Utilitaires",clearing:"Déblayage"};
function displayPriceName(item){return String(item.name||item.composition).replace(/^display_name_/,"").replace(/^mill_/,"").replace(/^clearables_/,"").replace(/_/g," ").replace(/\b\w/g,char=>char.toUpperCase())}
function renderPriceCategory(){const category=gamePriceCategories.find(item=>item.id===priceCategoryEl.value);priceRailEl.replaceChildren();if(!category||!category.items.length){priceRailEl.textContent="Aucun objet dans cette catégorie.";return}
for(const item of category.items){const card=document.createElement("article");card.className="price-card";const purchaseLabel=item.clearable?"Prix de déblayage (or)":"Prix de base (or)",hurryMarkup=item.clearable?"":'<label>Accélération / skip (gemmes)<input type="number" min="0" max="1000000000" step="1" data-hurry value="'+(item.hurryAmount===null?"":item.hurryAmount)+'" placeholder="Non défini"></label>';card.innerHTML='<img src="/admin/assets/'+encodeURIComponent(item.image)+'" alt="'+esc(displayPriceName(item))+'"><strong>'+esc(displayPriceName(item))+'</strong><div class="muted price-id">'+esc(item.composition)+'</div><label>'+purchaseLabel+'<input type="number" min="0" max="1000000000" step="1" data-purchase value="'+(item.purchaseGold===null?"":item.purchaseGold)+'" placeholder="Non défini"></label>'+hurryMarkup+'<div class="actions"><button type="button" data-save>Enregistrer</button><button type="button" class="secondary" data-reset '+(item.customized?"":"disabled")+'>Reset</button></div>';
card.querySelector("[data-save]").addEventListener("click",async()=>{const body={},purchase=card.querySelector("[data-purchase]").value,hurryInput=card.querySelector("[data-hurry]"),hurry=hurryInput?hurryInput.value:"";if(purchase!=="")body.purchaseGold=Number(purchase);if(hurry!=="")body.hurryAmount=Number(hurry);if(!Object.keys(body).length){setPriceStatus("Saisis au moins un prix à modifier.",true);return}try{await request("/admin/api/game/prices/"+encodeURIComponent(item.composition),{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)});await loadGamePrices();setPriceStatus("Prix enregistré pour "+displayPriceName(item)+". Redémarre le jeu pour appliquer le changement.")}catch(error){setPriceStatus(error.message,true)}});
card.querySelector("[data-reset]").addEventListener("click",async()=>{try{await request("/admin/api/game/prices/"+encodeURIComponent(item.composition)+"/reset",{method:"POST"});await loadGamePrices();setPriceStatus("Prix d’origine restaurés pour "+displayPriceName(item)+".")}catch(error){setPriceStatus(error.message,true)}});
priceRailEl.append(card)}}
async function loadGamePrices(){try{const selected=priceCategoryEl.value;gamePriceCategories=await request("/admin/api/game/prices");priceCategoryEl.replaceChildren();for(const category of gamePriceCategories){const option=document.createElement("option");option.value=category.id;option.textContent=(gameCategoryNames[category.id]||category.id)+" ("+category.items.length+")";priceCategoryEl.append(option)}if(gamePriceCategories.some(category=>category.id===selected))priceCategoryEl.value=selected;renderPriceCategory();setPriceStatus("Prix chargés. "+gamePriceCategories.reduce((total,category)=>total+category.items.length,0)+" objet(s) disponibles.")}catch(error){setPriceStatus(error.message,true)}}
async function showPlayer(uuid){selected=uuid;refreshMarketButton.disabled=!selected;resetPlayerInventory("Chargement de l’inventaire…");await refresh();try{const data=await request("/admin/api/players/"+encodeURIComponent(uuid));renderDetails(data.player,data.fragments);loadPlayerInventory(uuid,data.player)}catch(e){detailsEl.textContent=e.message;resetPlayerInventory(e.message)}}
function renderDetails(p,fragments){detailsEl.innerHTML='<div class="muted">UUID: '+esc(p.uuid)+'</div><div class="notice">'+p.fragmentCount+' fragment(s) · last saved: '+esc(p.lastSavedAt||"unavailable")+'<br>Balance: '+p.nanopods+' nanopods · '+p.gems+' gems</div><form id="identity"><label for="alias">Alias</label><input id="alias" maxlength="100" required><label for="z2did">Device ID (z2did)</label><input id="z2did" maxlength="256"><p class="muted">After reinstalling, the game may create a new "Guest" account. Note its z2did from the list, stop the game, delete the temporary account, then assign its z2did to this save.</p><button>Save player details</button></form><div class="actions"><a class="button secondary" href="/admin/api/players/'+encodeURIComponent(p.uuid)+'/backup">Download save</a><button type="button" class="secondary" id="restore">Restore from file</button><button type="button" class="danger" id="deletePlayer">Delete this account</button><input id="backupFile" type="file" accept="application/json,.json" hidden></div><h2 style="margin-top:22px">Saved fragments</h2><ul class="fragments" id="fragmentList"></ul>';
document.querySelector("#alias").value=p.alias;document.querySelector("#z2did").value=p.z2did||"";
const list=document.querySelector("#fragmentList");if(!fragments.length){list.innerHTML="<li>No saved fragments.</li>"}else for(const f of fragments){const li=document.createElement("li");li.textContent=f.key+" · "+f.updatedAt;list.append(li)}
document.querySelector("#identity").addEventListener("submit",async e=>{e.preventDefault();try{await request("/admin/api/players/"+encodeURIComponent(p.uuid),{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({alias:document.querySelector("#alias").value,z2did:document.querySelector("#z2did").value})});setStatus("Player updated.");await showPlayer(p.uuid)}catch(error){setStatus(error.message,true)}});
const skipTutorial=document.createElement("button");skipTutorial.type="button";skipTutorial.className="secondary";skipTutorial.textContent="Unlock camera (skip tutorial)";skipTutorial.addEventListener("click",async()=>{if(!confirm("Stop the game and download a backup first. This marks the final diving tutorial missions as complete for this player. Continue?"))return;try{await request("/admin/api/players/"+encodeURIComponent(p.uuid)+"/skip-seal-house-tutorial",{method:"POST"});await showPlayer(p.uuid);setStatus("Diving tutorial skipped. Restart the game to apply the change.")}catch(error){setStatus(error.message,true)}});detailsEl.querySelector(".actions").insertBefore(skipTutorial,document.querySelector("#deletePlayer"));
const fileInput=document.querySelector("#backupFile");document.querySelector("#restore").addEventListener("click",()=>fileInput.click());fileInput.addEventListener("change",async()=>{if(!fileInput.files.length)return;if(!confirm("Restoring this backup will replace this player's current data. Continue?")){fileInput.value="";return}try{const backup=JSON.parse(await fileInput.files[0].text());await request("/admin/api/players/"+encodeURIComponent(p.uuid)+"/restore",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(backup)});setStatus("Save restored.");await showPlayer(p.uuid)}catch(error){setStatus(error.message,true)}finally{fileInput.value=""}});
document.querySelector("#deletePlayer").addEventListener("click",async()=>{if(!confirm("Permanently delete "+p.alias+" and all associated fragments? Download a backup first."))return;try{await request("/admin/api/players/"+encodeURIComponent(p.uuid),{method:"DELETE"});selected="";refreshMarketButton.disabled=true;resetPlayerInventory("Sélectionne un joueur dans la liste pour gérer son inventaire.");detailsEl.textContent="Account deleted.";setStatus("Account and associated data deleted.");await refresh()}catch(error){setStatus(error.message,true)}});
}
document.querySelector("#marketSettings").addEventListener("submit",async e=>{e.preventDefault();try{const settings={refreshIntervalMinutes:Number(document.querySelector("#marketInterval").value),offerCount:Number(document.querySelector("#marketOfferCount").value)};await request("/admin/api/world-market/settings",{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(settings)});setMarketStatus("Settings saved. They will take effect at the next refresh. Refresh the selected player's shop to apply them immediately.")}catch(error){setMarketStatus(error.message,true)}});
document.querySelector("#worldSnowSettings").addEventListener("submit",async e=>{e.preventDefault();const enabled=document.querySelector("#worldSnowEnabled").checked;try{await request("/admin/api/game/world-snow",{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({enabled})});setWorldSnowStatus("Setting saved. Close and relaunch the game to apply the change.")}catch(error){setWorldSnowStatus(error.message,true)}});
document.querySelector("#buildMenuSettings").addEventListener("submit",async e=>{e.preventDefault();const mode=document.querySelector("#buildMenuMode").value;try{const settings=await request("/admin/api/game/build-menu",{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({mode})});document.querySelector("#buildMenuMode").value=settings.mode;setBuildMenuStatus("Visibility and prices saved. Close and relaunch the game to apply. "+settings.allHiddenCount+" eligible hidden catalog item(s), including "+settings.eventDecorationCount+" event decoration(s).")}catch(error){setBuildMenuStatus(error.message,true)}});
priceCategoryEl.addEventListener("change",renderPriceCategory);
document.querySelector("#inventoryCurrencies").addEventListener("submit",async event=>{event.preventDefault();const uuid=playerInventoryUUID,nanopods=Number(document.querySelector("#inventoryNanopods").value),gems=Number(document.querySelector("#inventoryGems").value);if(!uuid||uuid!==selected){setInventoryStatus("Sélectionne un joueur avant de modifier ses monnaies.",true);return}if(!Number.isSafeInteger(nanopods)||nanopods<0||nanopods>1000000000||!Number.isSafeInteger(gems)||gems<0||gems>1000000000){setInventoryStatus("Les soldes doivent être compris entre 0 et 1 000 000 000.",true);return}try{await request("/admin/api/players/"+encodeURIComponent(uuid)+"/currencies",{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({nanopods,gems})});if(uuid===selected){await showPlayer(uuid);setStatus("Monnaies enregistrées. Relance le jeu pour appliquer les changements.")}}catch(error){if(uuid===selected)setInventoryStatus(error.message,true)}});
priceCardSizeEl.addEventListener("input",()=>applyPriceCardSize(priceCardSizeEl.value));
inventoryCardSizeEl.addEventListener("input",()=>applyPriceCardSize(inventoryCardSizeEl.value));
function savePriceCardSize(value,onError){if(!applyPriceCardSize(value))return;try{localStorage.setItem(priceCardSizeKey,String(value))}catch(error){onError("La taille est appliquée mais n’a pas pu être mémorisée : "+error.message,true)}}
priceCardSizeEl.addEventListener("change",()=>savePriceCardSize(priceCardSizeEl.value,setPriceStatus));
inventoryCardSizeEl.addEventListener("change",()=>savePriceCardSize(inventoryCardSizeEl.value,setInventoryStatus));
document.querySelector("#resetAllPrices").addEventListener("click",async()=>{if(!confirm("Restaurer les prix d'origine de tous les objets modifiés ?"))return;try{const result=await request("/admin/api/game/prices/reset-all",{method:"POST"});await loadGamePrices();setPriceStatus(result.restored+" objet(s) restauré(s). Redémarre le jeu pour appliquer les changements.")}catch(error){setPriceStatus(error.message,true)}});
function initializeHorizontalRail(rail){let drag=null,glideFrame=0,wheelFrame=0,wheelTarget=null;function glide(velocity){cancelAnimationFrame(glideFrame);const step=()=>{velocity*=.94;if(Math.abs(velocity)<.02){glideFrame=0;return}rail.scrollLeft+=velocity*16;glideFrame=requestAnimationFrame(step)};glideFrame=requestAnimationFrame(step)}function finish(withMomentum){if(!drag)return;const velocity=drag.velocity;drag=null;rail.classList.remove("dragging");if(withMomentum)glide(velocity)}
rail.addEventListener("pointerdown",event=>{if(event.button!==0||event.target.closest("button,input,select,a"))return;cancelAnimationFrame(glideFrame);cancelAnimationFrame(wheelFrame);glideFrame=0;wheelFrame=0;wheelTarget=null;drag={pointerId:event.pointerId,lastX:event.clientX,lastTime:event.timeStamp,velocity:0};rail.classList.add("dragging");rail.setPointerCapture(event.pointerId)});
rail.addEventListener("pointermove",event=>{if(!drag||event.pointerId!==drag.pointerId)return;const delta=event.clientX-drag.lastX,elapsed=Math.max(event.timeStamp-drag.lastTime,1);rail.scrollLeft-=delta;drag.velocity=Math.max(-1.5,Math.min(1.5,-delta/elapsed));drag.lastX=event.clientX;drag.lastTime=event.timeStamp;event.preventDefault()});rail.addEventListener("pointerup",()=>finish(true));rail.addEventListener("pointercancel",()=>finish(false));rail.addEventListener("lostpointercapture",()=>finish(false));
rail.addEventListener("wheel",event=>{if(Math.abs(event.deltaX)>Math.abs(event.deltaY)||rail.scrollWidth<=rail.clientWidth)return;event.preventDefault();const multiplier=event.deltaMode===1?16:event.deltaMode===2?rail.clientHeight:1;wheelTarget=(wheelTarget===null?rail.scrollLeft:wheelTarget)+event.deltaY*multiplier;wheelTarget=Math.max(0,Math.min(wheelTarget,rail.scrollWidth-rail.clientWidth));if(wheelFrame)return;const smoothWheel=()=>{const difference=wheelTarget-rail.scrollLeft;if(Math.abs(difference)<.5){rail.scrollLeft=wheelTarget;wheelFrame=0;wheelTarget=null;return}rail.scrollLeft+=difference*.22;wheelFrame=requestAnimationFrame(smoothWheel)};wheelFrame=requestAnimationFrame(smoothWheel)},{passive:false})}
initializeHorizontalRail(priceRailEl);initializeHorizontalRail(inventoryRailEl);
refreshMarketButton.addEventListener("click",async()=>{if(!selected)return;refreshMarketButton.disabled=true;try{const result=await request("/admin/api/players/"+encodeURIComponent(selected)+"/world-market/refresh",{method:"POST"});setMarketStatus(result.activeSessions>0?"Market refreshed and sent to the connected client. If the market is already open, close and reopen only the market.":"Market refreshed. No client is connected; the offers will load at the next login.")}catch(error){setMarketStatus(error.message,true)}finally{refreshMarketButton.disabled=!selected}});
let draggedModule=null;
for(const handle of document.querySelectorAll("[data-module-handle]")){handle.addEventListener("dragstart",event=>{draggedModule=setDraggedModule(handle.closest("section[data-module-id]"));draggedModule.classList.add("dragging");event.dataTransfer.effectAllowed="move";event.dataTransfer.setData("text/plain",draggedModule.dataset.moduleId)});handle.addEventListener("dragend",()=>{if(draggedModule)draggedModule.classList.remove("dragging");draggedModule=null;for(const panel of modulePanels())panel.classList.remove("drop-target")});handle.addEventListener("keydown",event=>{if(["ArrowUp","ArrowDown","ArrowLeft","ArrowRight"].includes(event.key)){event.preventDefault();moveModuleByKey(handle.closest("section[data-module-id]"),event.key,handle)}})}
adminModulesEl.addEventListener("dragover",event=>{if(!draggedModule)return;const target=event.target.closest("section[data-module-id],.module-column");if(target){event.preventDefault();for(const panel of modulePanels())panel.classList.remove("drop-target");if(target.matches("section[data-module-id]")&&target!==draggedModule)target.classList.add("drop-target")}});
adminModulesEl.addEventListener("drop",event=>{if(!draggedModule)return;event.preventDefault();const target=event.target.closest("section[data-module-id]");const column=(target||event.target.closest(".module-column"));if(column){const targetColumn=target?target.closest(".module-column"):column;if(target===draggedModule){draggedModule.classList.remove("dragging","drop-target");draggedModule=null;return}const before=target?event.clientY<target.getBoundingClientRect().top+target.getBoundingClientRect().height/2:false;placeModule(draggedModule,targetColumn,target,before)}draggedModule.classList.remove("dragging","drop-target");draggedModule=null;for(const panel of modulePanels())panel.classList.remove("drop-target")});
document.querySelector("#refresh").addEventListener("click",refresh);refresh();loadMarketSettings();loadWorldSnowSettings();loadBuildMenuSettings();loadPriceCardSize();loadGamePrices();
</script>
</body>
</html>`
