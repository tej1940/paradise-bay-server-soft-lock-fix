package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const maxInventoryAmount int64 = 1_000_000_000

var (
	errPlayerOnline               = errors.New("stop the game before editing this player's inventory")
	errPlayerInventoryUnavailable = errors.New("player inventory is not initialized or has an unsupported format")
	errInventoryItemNotFound      = errors.New("inventory item is not in the game catalog or this player's inventory")
)

type inventoryItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Image    string `json:"image,omitempty"`
	Amount   int64  `json:"amount"`
}

type playerInventory struct {
	Items    []inventoryItem `json:"items"`
	Nanopods int64           `json:"nanopods"`
	Gems     int64           `json:"gems"`
}

type inventoryItemMetadata struct {
	name     string
	category string
	image    string
}

type storageCatalogFile struct {
	Catalog map[string]struct {
		Compositions []struct {
			CompositionName string `json:"compositionName"`
		} `json:"compositions"`
	} `json:"catalog"`
}

type inventoryResourcesFile map[string]struct {
	IconFilename string `json:"iconFilename"`
	NameKey      string `json:"nameKey"`
}

func (s *Store) getPlayerInventory(uuid string) (playerInventory, error) {
	player, err := s.GetPlayerByCID(uuid)
	if err != nil {
		return playerInventory{}, err
	}

	key := playerGameEntityKey(uuid)
	blob, err := s.GetFragment(key)
	if errors.Is(err, sql.ErrNoRows) {
		return playerInventory{}, errPlayerInventoryUnavailable
	}
	if err != nil {
		return playerInventory{}, fmt.Errorf("load player inventory fragment: %w", err)
	}
	_, _, wealth, err := decodePlayerWealth(blob)
	if err != nil {
		return playerInventory{}, err
	}

	catalog, err := s.loadInventoryCatalog()
	if err != nil {
		return playerInventory{}, err
	}
	items := make(map[string]inventoryItem, len(catalog)+len(wealth))
	for id, metadata := range catalog {
		amount, err := decodeInventoryAmount(wealth[id])
		if err != nil && len(wealth[id]) != 0 {
			return playerInventory{}, fmt.Errorf("read inventory item %s: %w", id, err)
		}
		items[id] = inventoryItem{
			ID:       id,
			Name:     metadata.name,
			Category: metadata.category,
			Image:    metadata.image,
			Amount:   amount,
		}
	}
	for id, rawAmount := range wealth {
		if _, ok := items[id]; ok {
			continue
		}
		amount, err := decodeInventoryAmount(rawAmount)
		if err != nil {
			return playerInventory{}, fmt.Errorf("read inventory item %s: %w", id, err)
		}
		metadata := inventoryItemMetadata{name: inventoryDisplayName(id), category: "other"}
		items[id] = inventoryItem{ID: id, Name: metadata.name, Category: metadata.category, Amount: amount}
	}

	result := playerInventory{
		Items:    make([]inventoryItem, 0, len(items)),
		Nanopods: player.Nanopods,
		Gems:     player.Gems,
	}
	for _, item := range items {
		result.Items = append(result.Items, item)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		if result.Items[i].Category != result.Items[j].Category {
			return result.Items[i].Category < result.Items[j].Category
		}
		return result.Items[i].Name < result.Items[j].Name
	})
	return result, nil
}

func (s *Store) setPlayerInventoryItem(uuid, itemID string, amount int64) error {
	if !isSafeCompositionName(itemID) || amount < 0 || amount > maxInventoryAmount {
		return fmt.Errorf("invalid inventory item or quantity")
	}
	return s.withPlayerOffline(uuid, func() error {
		if _, err := s.GetPlayerByCID(uuid); err != nil {
			return err
		}
		blob, err := s.GetFragment(playerGameEntityKey(uuid))
		if errors.Is(err, sql.ErrNoRows) {
			return errPlayerInventoryUnavailable
		}
		if err != nil {
			return fmt.Errorf("load player inventory fragment: %w", err)
		}
		root, wallet, wealth, err := decodePlayerWealth(blob)
		if err != nil {
			return err
		}
		_, alreadyHeld := wealth[itemID]
		if !alreadyHeld {
			catalog, err := s.loadInventoryCatalog()
			if err != nil {
				return err
			}
			if _, allowed := catalog[itemID]; !allowed {
				return errInventoryItemNotFound
			}
		}

		encodedAmount, err := json.Marshal(amount)
		if err != nil {
			return fmt.Errorf("encode inventory quantity: %w", err)
		}
		wealth[itemID] = encodedAmount
		encodedWealth, err := json.Marshal(wealth)
		if err != nil {
			return fmt.Errorf("encode player inventory: %w", err)
		}
		wallet["wealth"] = encodedWealth
		encodedWallet, err := json.Marshal(wallet)
		if err != nil {
			return fmt.Errorf("encode player wallet: %w", err)
		}
		root["wallet"] = encodedWallet
		updated, err := json.Marshal(root)
		if err != nil {
			return fmt.Errorf("encode player fragment: %w", err)
		}
		sum := sha256.Sum256(updated)
		if err := s.UpsertFragment(playerGameEntityKey(uuid), string(updated), fmt.Sprintf("%x", sum)); err != nil {
			return fmt.Errorf("save player inventory: %w", err)
		}
		return nil
	})
}

func (s *Store) setPlayerCurrencies(uuid string, nanopods, gems *int64) (Player, error) {
	if nanopods == nil && gems == nil {
		return Player{}, errors.New("provide at least one currency balance")
	}
	if (nanopods != nil && (*nanopods < 0 || *nanopods > maxInventoryAmount)) ||
		(gems != nil && (*gems < 0 || *gems > maxInventoryAmount)) {
		return Player{}, errors.New("currency balances are outside the allowed range")
	}

	var updated Player
	err := s.withPlayerOffline(uuid, func() error {
		player, err := s.GetPlayerByCID(uuid)
		if err != nil {
			return err
		}
		columns := make([]string, 0, 2)
		if nanopods != nil {
			player.Nanopods = *nanopods
			columns = append(columns, "nanopods")
		}
		if gems != nil {
			player.Gems = *gems
			columns = append(columns, "gems")
		}
		result, err := s.db.NewUpdate().Model(&player).Column(columns...).WherePK().Exec(context.Background())
		if err != nil {
			return fmt.Errorf("save player currencies: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("verify player currency update: %w", err)
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		updated = player
		return nil
	})
	return updated, err
}

func (s *Store) withPlayerOffline(uuid string, update func() error) error {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if len(s.sessions[uuid]) != 0 {
		return errPlayerOnline
	}
	return update()
}

func playerGameEntityKey(uuid string) string {
	return "EntityCollection:gamePlayer:D[GamePlayer]P[" + uuid + "]"
}

func decodePlayerWealth(blob string) (map[string]json.RawMessage, map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &root); err != nil || root == nil {
		return nil, nil, nil, errPlayerInventoryUnavailable
	}
	var fragmentType string
	if err := json.Unmarshal(root["_t"], &fragmentType); err != nil || fragmentType != "GameEntityFragment:v1" {
		return nil, nil, nil, errPlayerInventoryUnavailable
	}
	var wallet map[string]json.RawMessage
	if err := json.Unmarshal(root["wallet"], &wallet); err != nil || wallet == nil {
		return nil, nil, nil, errPlayerInventoryUnavailable
	}
	var wealth map[string]json.RawMessage
	if err := json.Unmarshal(wallet["wealth"], &wealth); err != nil || wealth == nil {
		return nil, nil, nil, errPlayerInventoryUnavailable
	}
	return root, wallet, wealth, nil
}

func decodeInventoryAmount(raw json.RawMessage) (int64, error) {
	var amount int64
	if len(raw) == 0 {
		return 0, nil
	}
	if err := json.Unmarshal(raw, &amount); err != nil {
		return 0, err
	}
	if amount < 0 || amount > maxInventoryAmount {
		return 0, fmt.Errorf("quantity is outside the allowed range")
	}
	return amount, nil
}

func (s *Store) loadInventoryCatalog() (map[string]inventoryItemMetadata, error) {
	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	bundleDir := filepath.Join(s.appxPackageDir, "bundle")
	dataDir := filepath.Join(bundleDir, "data")
	storageData, err := os.ReadFile(filepath.Join(bundleDir, "StorageConfigService_StorageCatalog.json"))
	if err != nil {
		return nil, fmt.Errorf("read storage catalog: %w", err)
	}
	var storageCatalog storageCatalogFile
	if err := json.Unmarshal(storageData, &storageCatalog); err != nil {
		return nil, fmt.Errorf("decode storage catalog: %w", err)
	}
	if len(storageCatalog.Catalog) == 0 {
		return nil, errors.New("storage catalog has no categories")
	}
	resourceData, err := os.ReadFile(filepath.Join(dataDir, "ResourcesConfig.json"))
	if err != nil {
		return nil, fmt.Errorf("read resource catalog: %w", err)
	}
	var resources inventoryResourcesFile
	if err := json.Unmarshal(resourceData, &resources); err != nil {
		return nil, fmt.Errorf("decode resource catalog: %w", err)
	}
	if len(resources) == 0 {
		return nil, errors.New("resource catalog is empty")
	}

	categories := make([]string, 0, len(storageCatalog.Catalog))
	for category := range storageCatalog.Catalog {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	result := make(map[string]inventoryItemMetadata)
	for _, category := range categories {
		for _, composition := range storageCatalog.Catalog[category].Compositions {
			id := composition.CompositionName
			if !strings.HasPrefix(id, "resource_") || !isSafeCompositionName(id) {
				continue
			}
			metadata, exists := resources[id]
			item := inventoryItemMetadata{
				name:     inventoryDisplayName(id),
				category: category,
			}
			if exists {
				item.name = inventoryDisplayName(metadata.NameKey)
				item.image = gamePriceImage(dataDir, id, metadata.IconFilename)
			}
			if item.name == "" {
				item.name = inventoryDisplayName(id)
			}
			result[id] = item
		}
	}
	if len(result) == 0 {
		return nil, errors.New("storage catalog has no resource items")
	}
	return result, nil
}

func inventoryDisplayName(value string) string {
	value = strings.TrimPrefix(value, "resource_name_")
	value = strings.TrimPrefix(value, "resource_")
	value = strings.ReplaceAll(value, "_", " ")
	var name strings.Builder
	runes := []rune(value)
	for index, current := range runes {
		if index > 0 && unicode.IsUpper(current) && (unicode.IsLower(runes[index-1]) || unicode.IsDigit(runes[index-1])) {
			name.WriteByte(' ')
		}
		if index == 0 {
			current = unicode.ToUpper(current)
		}
		name.WriteRune(current)
	}
	return strings.TrimSpace(name.String())
}
