package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	gamePriceGoldResource = "resource_gold"
	gamePriceGemResource  = "resource_gems"
	maxGamePrice          = 1_000_000_000
)

type gamePriceCategory struct {
	ID    string          `json:"id"`
	Items []gamePriceItem `json:"items"`
}

type gamePriceItem struct {
	Composition  string `json:"composition"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	Image        string `json:"image"`
	PurchaseGold *int   `json:"purchaseGold"`
	HurryAmount  *int   `json:"hurryAmount"`
	Customized   bool   `json:"customized"`
	Clearable    bool   `json:"clearable"`
}

type gamePriceCatalogFile struct {
	Catalog map[string]buildMenuCategory `json:"catalog"`
}

type gamePriceUpdate struct {
	PurchaseGold *int `json:"purchaseGold"`
	HurryAmount  *int `json:"hurryAmount"`
}

func (s *Store) getGamePriceCategories() ([]gamePriceCategory, error) {
	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	paths, err := s.buildMenuPaths()
	if err != nil {
		return nil, err
	}
	return loadGamePriceCategories(paths)
}

func (s *Store) setGameItemPrice(name string, update gamePriceUpdate) error {
	if !isSafeCompositionName(name) {
		return fmt.Errorf("invalid composition name %q", name)
	}
	if update.PurchaseGold == nil && update.HurryAmount == nil {
		return fmt.Errorf("provide at least one price to update")
	}
	if strings.HasPrefix(name, "clearables_") && update.HurryAmount != nil {
		return fmt.Errorf("clearable items do not support hurry prices")
	}
	if update.PurchaseGold != nil && (*update.PurchaseGold < 0 || *update.PurchaseGold > maxGamePrice) {
		return fmt.Errorf("purchase price must be between 0 and %d", maxGamePrice)
	}
	if update.HurryAmount != nil && (*update.HurryAmount < 0 || *update.HurryAmount > maxGamePrice) {
		return fmt.Errorf("hurry price must be between 0 and %d", maxGamePrice)
	}

	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	paths, err := s.buildMenuPaths()
	if err != nil {
		return err
	}
	itemPath := filepath.Join(paths.DataDir, name+".composition.json")
	current, err := os.ReadFile(itemPath)
	if err != nil {
		return fmt.Errorf("read composition %s: %w", name, err)
	}
	updated, err := updateGameCompositionPrices(current, update, strings.HasPrefix(name, "clearables_"))
	if err != nil {
		return fmt.Errorf("update prices for %s: %w", name, err)
	}
	if bytes.Equal(current, updated) {
		return nil
	}
	backupDir := gamePriceBackupDir(paths)
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("create game price backup directory: %w", err)
	}
	if err := ensureGamePriceBackup(filepath.Join(backupDir, name+".composition.json"), current); err != nil {
		return err
	}
	if err := writeAtomicClientFile(itemPath, updated); err != nil {
		return fmt.Errorf("save composition prices: %w", err)
	}
	return nil
}

func (s *Store) resetGameItemPrice(name string) (bool, error) {
	if !isSafeCompositionName(name) {
		return false, fmt.Errorf("invalid composition name %q", name)
	}
	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	paths, err := s.buildMenuPaths()
	if err != nil {
		return false, err
	}
	backupPath := filepath.Join(gamePriceBackupDir(paths), name+".composition.json")
	original, err := os.ReadFile(backupPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read original prices for %s: %w", name, err)
	}
	itemPath := filepath.Join(paths.DataDir, name+".composition.json")
	current, err := os.ReadFile(itemPath)
	if err != nil {
		return false, fmt.Errorf("read composition %s: %w", name, err)
	}
	updated, err := restoreGameCompositionPrices(current, original)
	if err != nil {
		return false, fmt.Errorf("restore prices for %s: %w", name, err)
	}
	if bytes.Equal(current, updated) {
		return true, nil
	}
	if err := writeAtomicClientFile(itemPath, updated); err != nil {
		return false, fmt.Errorf("save restored prices: %w", err)
	}
	return true, nil
}

func (s *Store) resetAllGamePrices() (int, error) {
	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	paths, err := s.buildMenuPaths()
	if err != nil {
		return 0, err
	}
	backupDir := gamePriceBackupDir(paths)
	entries, err := os.ReadDir(backupDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read game price backups: %w", err)
	}
	updates := make([]buildMenuFileUpdate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".composition.json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".composition.json")
		if !isSafeCompositionName(name) {
			return 0, fmt.Errorf("unsafe game price backup name %q", entry.Name())
		}
		currentPath := filepath.Join(paths.DataDir, entry.Name())
		current, err := os.ReadFile(currentPath)
		if err != nil {
			return 0, fmt.Errorf("read composition %s: %w", name, err)
		}
		original, err := os.ReadFile(filepath.Join(backupDir, entry.Name()))
		if err != nil {
			return 0, fmt.Errorf("read original prices for %s: %w", name, err)
		}
		restored, err := restoreGameCompositionPrices(current, original)
		if err != nil {
			return 0, fmt.Errorf("restore prices for %s: %w", name, err)
		}
		if !bytes.Equal(current, restored) {
			updates = append(updates, buildMenuFileUpdate{Path: currentPath, Before: current, After: restored})
		}
	}
	applied, err := applyBuildMenuUpdates(updates)
	if err != nil {
		return 0, err
	}
	return len(applied), nil
}

func loadGamePriceCategories(paths buildMenuPaths) ([]gamePriceCategory, error) {
	data, err := os.ReadFile(paths.CatalogPath)
	if err != nil {
		return nil, fmt.Errorf("read build menu catalog: %w", err)
	}
	var catalog gamePriceCatalogFile
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode build menu catalog: %w", err)
	}
	if catalog.Catalog == nil {
		return nil, fmt.Errorf("build menu catalog has no catalog object")
	}

	names := make(map[string]string)
	categoryIDs := make([]string, 0, len(catalog.Catalog))
	for category := range catalog.Catalog {
		categoryIDs = append(categoryIDs, category)
	}
	sort.Strings(categoryIDs)
	for _, category := range categoryIDs {
		for _, entry := range catalog.Catalog[category].Compositions {
			if entry.CompositionName == "" {
				continue
			}
			if !isSafeCompositionName(entry.CompositionName) {
				return nil, fmt.Errorf("build menu catalog contains unsafe composition name %q", entry.CompositionName)
			}
			if _, exists := names[entry.CompositionName]; !exists {
				names[entry.CompositionName] = category
			}
		}
	}

	clearableNames, err := filepath.Glob(filepath.Join(paths.DataDir, "clearables_*.composition.json"))
	if err != nil {
		return nil, fmt.Errorf("list clearable compositions: %w", err)
	}
	for _, path := range clearableNames {
		name := strings.TrimSuffix(filepath.Base(path), ".composition.json")
		if isSafeCompositionName(name) {
			if _, exists := names[name]; !exists {
				names[name] = "clearing"
			}
		}
	}

	categoryItems := make(map[string][]gamePriceItem)
	backupDir := gamePriceBackupDir(paths)
	for name, category := range names {
		path := filepath.Join(paths.DataDir, name+".composition.json")
		composition, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read composition %s: %w", name, err)
		}
		item, err := decodeGamePriceItem(name, category, composition)
		if err != nil {
			return nil, fmt.Errorf("decode composition %s: %w", name, err)
		}
		if item == nil {
			continue
		}
		item.Image = gamePriceImage(paths.DataDir, name, item.Image)
		backup, err := os.ReadFile(filepath.Join(backupDir, name+".composition.json"))
		if err == nil {
			equal, compareErr := gameCompositionPricesEqual(composition, backup)
			if compareErr != nil {
				return nil, fmt.Errorf("compare original prices for %s: %w", name, compareErr)
			}
			item.Customized = !equal
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check original prices for %s: %w", name, err)
		}
		categoryItems[category] = append(categoryItems[category], *item)
	}

	result := make([]gamePriceCategory, 0, len(categoryItems))
	for category, items := range categoryItems {
		sort.Slice(items, func(i, j int) bool { return items[i].Composition < items[j].Composition })
		result = append(result, gamePriceCategory{ID: category, Items: items})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ID == "clearing" {
			return false
		}
		if result[j].ID == "clearing" {
			return true
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func decodeGamePriceItem(name, category string, data []byte) (*gamePriceItem, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(root["componentConfigs"], &components); err != nil || components == nil {
		return nil, fmt.Errorf("composition has no componentConfigs object")
	}
	var construction map[string]json.RawMessage
	if err := json.Unmarshal(components["construction"], &construction); err != nil {
		return nil, fmt.Errorf("decode construction component: %w", err)
	}
	if construction == nil {
		return nil, nil
	}
	var display struct {
		NameKey string `json:"nameKey"`
		IconKey string `json:"iconKey"`
	}
	if raw, ok := components["display"]; ok {
		if err := json.Unmarshal(raw, &display); err != nil {
			return nil, fmt.Errorf("decode display component: %w", err)
		}
	}

	item := &gamePriceItem{
		Composition: name,
		Name:        display.NameKey,
		Category:    category,
		Image:       filepath.Base(display.IconKey),
		Clearable:   strings.HasPrefix(name, "clearables_"),
	}
	if item.Name == "" {
		item.Name = name
	}
	if costData, ok := construction["constructionCostForOwnedQuantity"]; ok {
		var tiers []struct {
			Cost []struct {
				Amount   int    `json:"amount"`
				Resource string `json:"resource"`
			} `json:"cost"`
		}
		if err := json.Unmarshal(costData, &tiers); err != nil {
			return nil, fmt.Errorf("decode purchase costs: %w", err)
		}
		if len(tiers) > 0 {
			for _, cost := range tiers[0].Cost {
				if cost.Resource == gamePriceGoldResource {
					value := cost.Amount
					item.PurchaseGold = &value
					break
				}
			}
		}
	}
	if hurryData, ok := construction["hurryCost"]; ok {
		var hurry struct {
			Amount int `json:"amount"`
		}
		if err := json.Unmarshal(hurryData, &hurry); err != nil {
			return nil, fmt.Errorf("decode hurry cost: %w", err)
		}
		value := hurry.Amount
		item.HurryAmount = &value
	}
	return item, nil
}

func updateGameCompositionPrices(data []byte, update gamePriceUpdate, allowMissingConstruction bool) ([]byte, error) {
	root, components, construction, err := decodeGamePriceComposition(data, allowMissingConstruction)
	if err != nil {
		return nil, err
	}
	if update.PurchaseGold != nil {
		if err := setFirstTierGoldCost(construction, *update.PurchaseGold); err != nil {
			return nil, err
		}
	}
	if update.HurryAmount != nil {
		var hurry map[string]json.RawMessage
		if raw, ok := construction["hurryCost"]; ok {
			if err := json.Unmarshal(raw, &hurry); err != nil {
				return nil, fmt.Errorf("decode hurry cost: %w", err)
			}
		}
		if hurry == nil {
			hurry = make(map[string]json.RawMessage)
		}
		if _, ok := hurry["resource"]; !ok {
			resource, _ := json.Marshal(gamePriceGemResource)
			hurry["resource"] = resource
		}
		amount, _ := json.Marshal(*update.HurryAmount)
		hurry["amount"] = amount
		encodedHurry, err := json.Marshal(hurry)
		if err != nil {
			return nil, fmt.Errorf("encode hurry cost: %w", err)
		}
		construction["hurryCost"] = encodedHurry
	}
	if err := encodeGamePriceComposition(root, components, construction); err != nil {
		return nil, err
	}
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode composition: %w", err)
	}
	return append(updated, '\n'), nil
}

func setFirstTierGoldCost(construction map[string]json.RawMessage, amount int) error {
	var tiers []map[string]json.RawMessage
	if raw, ok := construction["constructionCostForOwnedQuantity"]; ok {
		if err := json.Unmarshal(raw, &tiers); err != nil {
			return fmt.Errorf("decode purchase costs: %w", err)
		}
	}
	if len(tiers) == 0 {
		tiers = []map[string]json.RawMessage{{}}
	}
	var costs []map[string]json.RawMessage
	if raw, ok := tiers[0]["cost"]; ok {
		if err := json.Unmarshal(raw, &costs); err != nil {
			return fmt.Errorf("decode purchase cost tier: %w", err)
		}
	}
	found := false
	for _, cost := range costs {
		var resource string
		if err := json.Unmarshal(cost["resource"], &resource); err != nil {
			continue
		}
		if resource == gamePriceGoldResource {
			encoded, _ := json.Marshal(amount)
			cost["amount"] = encoded
			found = true
			break
		}
	}
	if !found {
		resource, _ := json.Marshal(gamePriceGoldResource)
		encodedAmount, _ := json.Marshal(amount)
		costs = append(costs, map[string]json.RawMessage{"amount": encodedAmount, "resource": resource})
	}
	encodedCosts, err := json.Marshal(costs)
	if err != nil {
		return fmt.Errorf("encode purchase cost tier: %w", err)
	}
	tiers[0]["cost"] = encodedCosts
	encodedTiers, err := json.Marshal(tiers)
	if err != nil {
		return fmt.Errorf("encode purchase costs: %w", err)
	}
	construction["constructionCostForOwnedQuantity"] = encodedTiers
	return nil
}

func restoreGameCompositionPrices(current, original []byte) ([]byte, error) {
	return copyGameCompositionPrices(current, original)
}

func copyGameCompositionPrices(current, source []byte) ([]byte, error) {
	currentRoot, currentComponents, currentConstruction, err := decodeGamePriceComposition(current, false)
	if err != nil {
		return nil, err
	}
	_, _, sourceConstruction, err := decodeGamePriceComposition(source, false)
	if err != nil {
		return nil, fmt.Errorf("decode source composition prices: %w", err)
	}
	for _, key := range []string{"constructionCostForOwnedQuantity", "hurryCost"} {
		if sourceValue, ok := sourceConstruction[key]; ok {
			currentConstruction[key] = sourceValue
		} else {
			delete(currentConstruction, key)
		}
	}
	if err := encodeGamePriceComposition(currentRoot, currentComponents, currentConstruction); err != nil {
		return nil, err
	}
	updated, err := json.MarshalIndent(currentRoot, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode composition: %w", err)
	}
	return append(updated, '\n'), nil
}

func gameCompositionPricesEqual(first, second []byte) (bool, error) {
	_, _, firstConstruction, err := decodeGamePriceComposition(first, false)
	if err != nil {
		return false, err
	}
	_, _, secondConstruction, err := decodeGamePriceComposition(second, false)
	if err != nil {
		return false, err
	}
	for _, key := range []string{"constructionCostForOwnedQuantity", "hurryCost"} {
		firstValue, firstExists := firstConstruction[key]
		secondValue, secondExists := secondConstruction[key]
		if firstExists != secondExists {
			return false, nil
		}
		if !firstExists {
			continue
		}
		var firstJSON, secondJSON interface{}
		if err := json.Unmarshal(firstValue, &firstJSON); err != nil {
			return false, err
		}
		if err := json.Unmarshal(secondValue, &secondJSON); err != nil {
			return false, err
		}
		if !reflect.DeepEqual(firstJSON, secondJSON) {
			return false, nil
		}
	}
	return true, nil
}

func decodeGamePriceComposition(data []byte, createConstruction bool) (
	map[string]json.RawMessage,
	map[string]json.RawMessage,
	map[string]json.RawMessage,
	error,
) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, nil, fmt.Errorf("decode composition: %w", err)
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(root["componentConfigs"], &components); err != nil || components == nil {
		return nil, nil, nil, fmt.Errorf("composition has no componentConfigs object")
	}
	var construction map[string]json.RawMessage
	if raw, ok := components["construction"]; ok {
		if err := json.Unmarshal(raw, &construction); err != nil {
			return nil, nil, nil, fmt.Errorf("decode construction component: %w", err)
		}
	}
	if construction == nil && createConstruction {
		construction = map[string]json.RawMessage{}
	}
	if construction == nil {
		return nil, nil, nil, fmt.Errorf("composition has no construction component")
	}
	return root, components, construction, nil
}

func encodeGamePriceComposition(
	root map[string]json.RawMessage,
	components map[string]json.RawMessage,
	construction map[string]json.RawMessage,
) error {
	encodedConstruction, err := json.Marshal(construction)
	if err != nil {
		return fmt.Errorf("encode construction component: %w", err)
	}
	components["construction"] = encodedConstruction
	encodedComponents, err := json.Marshal(components)
	if err != nil {
		return fmt.Errorf("encode componentConfigs: %w", err)
	}
	root["componentConfigs"] = encodedComponents
	return nil
}

func gamePriceBackupDir(paths buildMenuPaths) string {
	return filepath.Join(paths.BackupDir, "prices")
}

func preserveCustomizedGamePrices(paths buildMenuPaths, name string, target, current []byte) ([]byte, error) {
	backupPath := filepath.Join(gamePriceBackupDir(paths), name+".composition.json")
	if _, err := os.Stat(backupPath); errors.Is(err, os.ErrNotExist) {
		return target, nil
	} else if err != nil {
		return nil, fmt.Errorf("check saved prices for %s: %w", name, err)
	}
	updated, err := copyGameCompositionPrices(target, current)
	if err != nil {
		return nil, fmt.Errorf("preserve saved prices for %s: %w", name, err)
	}
	return updated, nil
}

func ensureGamePriceBackup(path string, original []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read existing game price backup: %w", readErr)
		}
		if _, _, _, decodeErr := decodeGamePriceComposition(existing, false); decodeErr != nil {
			return fmt.Errorf("existing game price backup is invalid: %w", decodeErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("create game price backup: %w", err)
	}
	if _, err := file.Write(original); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("write game price backup: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close game price backup: %w", err)
	}
	return nil
}

func isSafeCompositionName(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

func gamePriceImage(dataDir, name, imageKey string) string {
	candidates := []string{imageKey}
	if strings.HasPrefix(name, "clearables_") {
		buildingName := "building_" + strings.TrimPrefix(name, "clearables_")
		buildingPath := filepath.Join(dataDir, buildingName+".composition.json")
		if composition, err := os.ReadFile(buildingPath); err == nil {
			var root struct {
				ComponentConfigs struct {
					Display struct {
						IconKey string `json:"iconKey"`
					} `json:"display"`
				} `json:"componentConfigs"`
			}
			if json.Unmarshal(composition, &root) == nil {
				candidates = append(candidates, root.ComponentConfigs.Display.IconKey)
			}
		}
	}
	for _, candidate := range candidates {
		if !strings.HasSuffix(strings.ToLower(candidate), ".png") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dataDir, candidate)); err == nil {
			return candidate
		}
		if _, err := os.Stat(filepath.Join(dataDir, strings.TrimSuffix(candidate, filepath.Ext(candidate))+".dds")); err == nil {
			return candidate
		}
	}
	return "icon_building_placeholder.png"
}

func decodeDDSIcon(data []byte) ([]byte, error) {
	if len(data) < 128 || string(data[:4]) != "DDS " || string(data[84:88]) != "DXT5" {
		return nil, fmt.Errorf("unsupported or invalid DDS texture")
	}
	width := int(binary.LittleEndian.Uint32(data[16:20]))
	height := int(binary.LittleEndian.Uint32(data[12:16]))
	if width <= 0 || height <= 0 || width > 2048 || height > 2048 {
		return nil, fmt.Errorf("DDS texture dimensions are invalid")
	}
	blockColumns := (width + 3) / 4
	blockRows := (height + 3) / 4
	required := 128 + blockColumns*blockRows*16
	if required > len(data) {
		return nil, fmt.Errorf("DDS texture data is truncated")
	}
	rgba := image.NewRGBA(image.Rect(0, 0, width, height))
	offset := 128
	for blockY := 0; blockY < blockRows; blockY++ {
		for blockX := 0; blockX < blockColumns; blockX++ {
			block := data[offset : offset+16]
			offset += 16
			decodeDXT5Block(rgba, blockX*4, blockY*4, block)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, rgba); err != nil {
		return nil, fmt.Errorf("encode DDS icon as PNG: %w", err)
	}
	return output.Bytes(), nil
}

func decodeDXT5Block(dst *image.RGBA, x, y int, block []byte) {
	alpha0, alpha1 := block[0], block[1]
	alphaTable := [8]uint8{alpha0, alpha1}
	if alpha0 > alpha1 {
		for i := 1; i <= 6; i++ {
			alphaTable[i+1] = uint8(((7-i)*int(alpha0) + i*int(alpha1)) / 7)
		}
	} else {
		for i := 1; i <= 4; i++ {
			alphaTable[i+1] = uint8(((5-i)*int(alpha0) + i*int(alpha1)) / 5)
		}
		alphaTable[6], alphaTable[7] = 0, 255
	}
	alphaIndices := uint64(0)
	for i := 0; i < 6; i++ {
		alphaIndices |= uint64(block[2+i]) << (8 * i)
	}

	color0 := binary.LittleEndian.Uint16(block[8:10])
	color1 := binary.LittleEndian.Uint16(block[10:12])
	colors := [4]color.RGBA{decodeRGB565(color0), decodeRGB565(color1)}
	colors[2] = color.RGBA{
		R: uint8((2*int(colors[0].R) + int(colors[1].R)) / 3),
		G: uint8((2*int(colors[0].G) + int(colors[1].G)) / 3),
		B: uint8((2*int(colors[0].B) + int(colors[1].B)) / 3),
		A: 255,
	}
	colors[3] = color.RGBA{
		R: uint8((int(colors[0].R) + 2*int(colors[1].R)) / 3),
		G: uint8((int(colors[0].G) + 2*int(colors[1].G)) / 3),
		B: uint8((int(colors[0].B) + 2*int(colors[1].B)) / 3),
		A: 255,
	}
	colorIndices := binary.LittleEndian.Uint32(block[12:16])
	for pixel := 0; pixel < 16; pixel++ {
		pixelX, pixelY := x+pixel%4, y+pixel/4
		if !image.Pt(pixelX, pixelY).In(dst.Bounds()) {
			continue
		}
		pixelColor := colors[(colorIndices>>(2*pixel))&3]
		pixelColor.A = alphaTable[(alphaIndices>>(3*pixel))&7]
		dst.SetRGBA(pixelX, pixelY, pixelColor)
	}
}

func decodeRGB565(value uint16) color.RGBA {
	r := uint8((value >> 11) & 0x1f)
	g := uint8((value >> 5) & 0x3f)
	b := uint8(value & 0x1f)
	return color.RGBA{R: (r << 3) | (r >> 2), G: (g << 2) | (g >> 4), B: (b << 3) | (b >> 2), A: 255}
}
