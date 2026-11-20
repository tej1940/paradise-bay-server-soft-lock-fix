package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type buildMenuMode string

const (
	buildMenuModeDefault          buildMenuMode = "default"
	buildMenuModeAllHidden        buildMenuMode = "all-hidden"
	buildMenuModeEventDecorations buildMenuMode = "event-decorations"
	buildMenuDefaultGoldPrice                   = 1000
)

type buildMenuModeSettings struct {
	Mode                 buildMenuMode `json:"mode"`
	AllHiddenCount       int           `json:"allHiddenCount"`
	EventDecorationCount int           `json:"eventDecorationCount"`
}

type buildMenuCatalogFile struct {
	Catalog map[string]buildMenuCategory `json:"catalog"`
}

type buildMenuCategory struct {
	Compositions []buildMenuCatalogEntry `json:"compositions"`
}

type buildMenuCatalogEntry struct {
	CompositionName string `json:"compositionName"`
	Feature         string `json:"_feature_buildingConfigServicePostCook"`
}

type buildMenuFeatureFile struct {
	Features []string `json:"features"`
}

type buildMenuModeState struct {
	Mode buildMenuMode `json:"mode"`
}

type buildMenuLocation struct {
	Category string
	Feature  string
}

type buildMenuItem struct {
	Name            string
	Path            string
	Baseline        []byte
	Current         []byte
	EventDecoration bool
}

type buildMenuFileUpdate struct {
	Path   string
	Before []byte
	After  []byte
}

type buildMenuPaths struct {
	DataDir      string
	CatalogPath  string
	FeaturesPath string
	BackupDir    string
	StatePath    string
}

func (s *Store) getBuildMenuModeSettings() (buildMenuModeSettings, error) {
	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	paths, err := s.buildMenuPaths()
	if err != nil {
		return buildMenuModeSettings{}, err
	}
	items, err := loadBuildMenuItems(paths)
	if err != nil {
		return buildMenuModeSettings{}, err
	}
	mode, err := readBuildMenuMode(paths.StatePath)
	if err != nil {
		return buildMenuModeSettings{}, err
	}
	return buildMenuSettingsForItems(mode, items), nil
}

func (s *Store) setBuildMenuMode(mode buildMenuMode) (buildMenuModeSettings, error) {
	if !isValidBuildMenuMode(mode) {
		return buildMenuModeSettings{}, fmt.Errorf("unsupported build menu mode %q", mode)
	}

	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	paths, err := s.buildMenuPaths()
	if err != nil {
		return buildMenuModeSettings{}, err
	}
	items, err := loadBuildMenuItems(paths)
	if err != nil {
		return buildMenuModeSettings{}, err
	}

	if mode == buildMenuModeDefault {
		updates, err := buildMenuRestoreUpdates(paths)
		if err != nil {
			return buildMenuModeSettings{}, err
		}
		for i := range updates {
			updated, err := preserveCustomizedGamePrices(paths, filepath.Base(strings.TrimSuffix(updates[i].Path, ".composition.json")), updates[i].After, updates[i].Before)
			if err != nil {
				return buildMenuModeSettings{}, err
			}
			updates[i].After = updated
		}
		applied, err := applyBuildMenuUpdates(updates)
		if err != nil {
			return buildMenuModeSettings{}, err
		}
		if err := writeBuildMenuMode(paths.StatePath, mode); err != nil {
			if rollbackErr := rollbackBuildMenuUpdates(applied); rollbackErr != nil {
				return buildMenuModeSettings{}, fmt.Errorf("save build menu mode: %w; rollback failed: %v", err, rollbackErr)
			}
			return buildMenuModeSettings{}, fmt.Errorf("save build menu mode: %w", err)
		}
		return buildMenuSettingsForItems(mode, items), nil
	}

	if err := os.MkdirAll(paths.BackupDir, 0o755); err != nil {
		return buildMenuModeSettings{}, fmt.Errorf("create build menu backup directory: %w", err)
	}
	for _, item := range items {
		if err := ensureBuildMenuBackup(buildMenuBackupPath(paths, item.Name), item.Baseline); err != nil {
			return buildMenuModeSettings{}, err
		}
	}

	updates := make([]buildMenuFileUpdate, 0, len(items))
	for _, item := range items {
		targetVisible := mode == buildMenuModeAllHidden || item.EventDecoration
		after := item.Baseline
		if targetVisible {
			after, err = setBuildMenuVisibility(item.Baseline, true)
			if err != nil {
				return buildMenuModeSettings{}, fmt.Errorf("update composition %s: %w", item.Name, err)
			}
			after, err = ensureBuildMenuGoldPrice(after, buildMenuDefaultGoldPrice)
			if err != nil {
				return buildMenuModeSettings{}, fmt.Errorf("set purchase cost for composition %s: %w", item.Name, err)
			}
		}
		after, err = preserveCustomizedGamePrices(paths, item.Name, after, item.Current)
		if err != nil {
			return buildMenuModeSettings{}, err
		}
		if bytes.Equal(item.Current, after) {
			continue
		}
		updates = append(updates, buildMenuFileUpdate{
			Path:   item.Path,
			Before: item.Current,
			After:  after,
		})
	}

	applied, err := applyBuildMenuUpdates(updates)
	if err != nil {
		return buildMenuModeSettings{}, err
	}
	if err := writeBuildMenuMode(paths.StatePath, mode); err != nil {
		if rollbackErr := rollbackBuildMenuUpdates(applied); rollbackErr != nil {
			return buildMenuModeSettings{}, fmt.Errorf("save build menu mode: %w; rollback failed: %v", err, rollbackErr)
		}
		return buildMenuModeSettings{}, fmt.Errorf("save build menu mode: %w", err)
	}
	return buildMenuSettingsForItems(mode, items), nil
}

func (s *Store) buildMenuPaths() (buildMenuPaths, error) {
	if s.appxPackageDir == "" {
		return buildMenuPaths{}, fmt.Errorf("game package directory is not configured")
	}
	dataDir := filepath.Join(s.appxPackageDir, "bundle", "data")
	paths := buildMenuPaths{
		DataDir:      dataDir,
		CatalogPath:  filepath.Join(dataDir, "BuildMenuCatalog.json"),
		FeaturesPath: filepath.Join(s.appxPackageDir, "bundle", "features.json"),
	}
	manifestPath := filepath.Join(s.appxPackageDir, "AppxManifest.xml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return buildMenuPaths{}, fmt.Errorf("read game package manifest: %w", err)
	}
	catalog, err := os.ReadFile(paths.CatalogPath)
	if err != nil {
		return buildMenuPaths{}, fmt.Errorf("read build menu catalog: %w", err)
	}
	features, err := os.ReadFile(paths.FeaturesPath)
	if err != nil {
		return buildMenuPaths{}, fmt.Errorf("read game feature list: %w", err)
	}
	hash := sha256.New()
	for _, content := range [][]byte{manifest, catalog, features} {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
	}
	fingerprint := hex.EncodeToString(hash.Sum(nil))
	backupRoot := filepath.Join(filepath.Dir(s.appxPackageDir), "client-config-backups", "build-menu", fingerprint)
	paths.BackupDir = backupRoot
	paths.StatePath = filepath.Join(backupRoot, "mode.json")
	return paths, nil
}

func loadBuildMenuItems(paths buildMenuPaths) ([]buildMenuItem, error) {
	catalogData, err := os.ReadFile(paths.CatalogPath)
	if err != nil {
		return nil, fmt.Errorf("read build menu catalog: %w", err)
	}
	var catalog buildMenuCatalogFile
	if err := json.Unmarshal(catalogData, &catalog); err != nil {
		return nil, fmt.Errorf("decode build menu catalog: %w", err)
	}
	if catalog.Catalog == nil {
		return nil, fmt.Errorf("build menu catalog has no catalog object")
	}

	featuresData, err := os.ReadFile(paths.FeaturesPath)
	if err != nil {
		return nil, fmt.Errorf("read game feature list: %w", err)
	}
	var featureFile buildMenuFeatureFile
	if err := json.Unmarshal(featuresData, &featureFile); err != nil {
		return nil, fmt.Errorf("decode game feature list: %w", err)
	}
	activeFeatures := make(map[string]struct{}, len(featureFile.Features))
	for _, feature := range featureFile.Features {
		activeFeatures[feature] = struct{}{}
	}

	locations := make(map[string][]buildMenuLocation)
	for category, categoryData := range catalog.Catalog {
		for _, entry := range categoryData.Compositions {
			if entry.CompositionName == "" {
				continue
			}
			if filepath.Base(entry.CompositionName) != entry.CompositionName ||
				strings.ContainsAny(entry.CompositionName, `/\`) {
				return nil, fmt.Errorf("build menu catalog contains unsafe composition name %q", entry.CompositionName)
			}
			locations[entry.CompositionName] = append(locations[entry.CompositionName], buildMenuLocation{
				Category: category,
				Feature:  entry.Feature,
			})
		}
	}

	names := make([]string, 0, len(locations))
	for name := range locations {
		names = append(names, name)
	}
	sort.Strings(names)

	items := make([]buildMenuItem, 0)
	for _, name := range names {
		itemPath := filepath.Join(paths.DataDir, name+".composition.json")
		current, err := os.ReadFile(itemPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read build menu composition %s: %w", name, err)
		}

		baseline := current
		backupPath := buildMenuBackupPath(paths, name)
		backup, err := os.ReadFile(backupPath)
		if err == nil {
			baseline = backup
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read original build menu composition %s: %w", name, err)
		}

		visible, hasVisibility, tags, err := decodeBuildMenuVisibility(baseline)
		if err != nil {
			return nil, fmt.Errorf("decode build menu composition %s: %w", name, err)
		}
		if backup != nil && (!hasVisibility || visible) {
			return nil, fmt.Errorf("original build menu composition %s was not hidden", name)
		}
		if !hasVisibility || visible {
			continue
		}
		_, hasCurrentVisibility, _, err := decodeBuildMenuVisibility(current)
		if err != nil {
			return nil, fmt.Errorf("decode current build menu composition %s: %w", name, err)
		}
		if !hasCurrentVisibility {
			return nil, fmt.Errorf("current build menu composition %s has no visibility setting", name)
		}

		featureActive := false
		eventDecoration := false
		for _, location := range locations[name] {
			active := location.Feature == ""
			if !active {
				_, active = activeFeatures[location.Feature]
			}
			if !active {
				continue
			}
			featureActive = true
			if hasTag(tags, "tag_decoration") &&
				(location.Category == "seasonal" || strings.HasPrefix(location.Feature, "event_")) {
				eventDecoration = true
			}
		}
		if !featureActive {
			continue
		}
		items = append(items, buildMenuItem{
			Name:            name,
			Path:            itemPath,
			Baseline:        baseline,
			Current:         current,
			EventDecoration: eventDecoration,
		})
	}
	return items, nil
}

func decodeBuildMenuVisibility(data []byte) (bool, bool, []string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return false, false, nil, fmt.Errorf("decode composition JSON: %w", err)
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(root["componentConfigs"], &components); err != nil {
		return false, false, nil, fmt.Errorf("decode componentConfigs: %w", err)
	}
	var display map[string]json.RawMessage
	if err := json.Unmarshal(components["display"], &display); err != nil {
		return false, false, nil, fmt.Errorf("decode display component: %w", err)
	}
	visibility, exists := display["visibleInBuildMenuWhenUnowned"]
	if !exists {
		return false, false, nil, nil
	}
	var visible bool
	if err := json.Unmarshal(visibility, &visible); err != nil {
		return false, false, nil, fmt.Errorf("decode visibleInBuildMenuWhenUnowned: %w", err)
	}
	var tagsComponent struct {
		Tags []string `json:"tags"`
	}
	if rawTags, ok := components["tags"]; ok {
		if err := json.Unmarshal(rawTags, &tagsComponent); err != nil {
			return false, false, nil, fmt.Errorf("decode composition tags: %w", err)
		}
	}
	return visible, true, tagsComponent.Tags, nil
}

func setBuildMenuVisibility(data []byte, visible bool) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("decode composition JSON: %w", err)
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(root["componentConfigs"], &components); err != nil {
		return nil, fmt.Errorf("decode componentConfigs: %w", err)
	}
	var display map[string]json.RawMessage
	if err := json.Unmarshal(components["display"], &display); err != nil {
		return nil, fmt.Errorf("decode display component: %w", err)
	}
	value, err := json.Marshal(visible)
	if err != nil {
		return nil, fmt.Errorf("encode menu visibility: %w", err)
	}
	display["visibleInBuildMenuWhenUnowned"] = value
	encodedDisplay, err := json.Marshal(display)
	if err != nil {
		return nil, fmt.Errorf("encode display component: %w", err)
	}
	components["display"] = encodedDisplay
	encodedComponents, err := json.Marshal(components)
	if err != nil {
		return nil, fmt.Errorf("encode componentConfigs: %w", err)
	}
	root["componentConfigs"] = encodedComponents
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode composition JSON: %w", err)
	}
	return append(updated, '\n'), nil
}

func ensureBuildMenuGoldPrice(data []byte, price int) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("decode composition JSON: %w", err)
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(root["componentConfigs"], &components); err != nil {
		return nil, fmt.Errorf("decode componentConfigs: %w", err)
	}
	var construction map[string]json.RawMessage
	if err := json.Unmarshal(components["construction"], &construction); err != nil {
		return nil, fmt.Errorf("decode construction component: %w", err)
	}
	if construction == nil {
		return nil, fmt.Errorf("composition has no construction component")
	}
	if _, hasCost := construction["constructionCostForOwnedQuantity"]; hasCost {
		return data, nil
	}

	defaultCost, err := json.Marshal([]struct {
		Cost []struct {
			Amount   int    `json:"amount"`
			Resource string `json:"resource"`
		} `json:"cost"`
	}{{
		Cost: []struct {
			Amount   int    `json:"amount"`
			Resource string `json:"resource"`
		}{{Amount: price, Resource: "resource_gold"}},
	}})
	if err != nil {
		return nil, fmt.Errorf("encode default purchase cost: %w", err)
	}
	construction["constructionCostForOwnedQuantity"] = defaultCost
	if _, hasBuildTime := construction["constructionTimeSecForOwnedQuantity"]; !hasBuildTime {
		construction["constructionTimeSecForOwnedQuantity"] = json.RawMessage(`[0]`)
	}
	encodedConstruction, err := json.Marshal(construction)
	if err != nil {
		return nil, fmt.Errorf("encode construction component: %w", err)
	}
	components["construction"] = encodedConstruction
	encodedComponents, err := json.Marshal(components)
	if err != nil {
		return nil, fmt.Errorf("encode componentConfigs: %w", err)
	}
	root["componentConfigs"] = encodedComponents
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode composition JSON: %w", err)
	}
	return append(updated, '\n'), nil
}

func hasTag(tags []string, expected string) bool {
	for _, tag := range tags {
		if tag == expected {
			return true
		}
	}
	return false
}

func buildMenuSettingsForItems(mode buildMenuMode, items []buildMenuItem) buildMenuModeSettings {
	settings := buildMenuModeSettings{Mode: mode}
	for _, item := range items {
		settings.AllHiddenCount++
		if item.EventDecoration {
			settings.EventDecorationCount++
		}
	}
	return settings
}

func buildMenuBackupPath(paths buildMenuPaths, name string) string {
	return filepath.Join(paths.BackupDir, name+".composition.json")
}

func ensureBuildMenuBackup(path string, original []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read existing build menu backup: %w", readErr)
		}
		if !bytes.Equal(existing, original) {
			return fmt.Errorf("existing build menu backup does not match the original composition")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("create build menu backup: %w", err)
	}
	if _, err := file.Write(original); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("write build menu backup: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close build menu backup: %w", err)
	}
	return nil
}

func buildMenuRestoreUpdates(paths buildMenuPaths) ([]buildMenuFileUpdate, error) {
	entries, err := os.ReadDir(paths.BackupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read build menu backup directory: %w", err)
	}
	updates := make([]buildMenuFileUpdate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".composition.json") {
			continue
		}
		name := entry.Name()
		compositionPath := filepath.Join(paths.DataDir, name)
		originalPath := filepath.Join(paths.BackupDir, name)
		original, err := os.ReadFile(originalPath)
		if err != nil {
			return nil, fmt.Errorf("read original build menu composition %s: %w", name, err)
		}
		visible, exists, _, err := decodeBuildMenuVisibility(original)
		if err != nil {
			return nil, fmt.Errorf("decode original build menu composition %s: %w", name, err)
		}
		if !exists || visible {
			return nil, fmt.Errorf("original build menu composition %s has an invalid visibility value", name)
		}
		current, err := os.ReadFile(compositionPath)
		if err != nil {
			return nil, fmt.Errorf("read build menu composition %s: %w", name, err)
		}
		if !bytes.Equal(current, original) {
			updates = append(updates, buildMenuFileUpdate{
				Path:   compositionPath,
				Before: current,
				After:  original,
			})
		}
	}
	return updates, nil
}

func applyBuildMenuUpdates(updates []buildMenuFileUpdate) ([]buildMenuFileUpdate, error) {
	applied := make([]buildMenuFileUpdate, 0, len(updates))
	for _, update := range updates {
		if bytes.Equal(update.Before, update.After) {
			continue
		}
		if err := writeAtomicClientFile(update.Path, update.After); err != nil {
			if rollbackErr := rollbackBuildMenuUpdates(applied); rollbackErr != nil {
				return nil, fmt.Errorf("write build menu composition: %w; rollback failed: %v", err, rollbackErr)
			}
			return nil, fmt.Errorf("write build menu composition: %w", err)
		}
		applied = append(applied, update)
	}
	return applied, nil
}

func rollbackBuildMenuUpdates(updates []buildMenuFileUpdate) error {
	for i := len(updates) - 1; i >= 0; i-- {
		if err := writeAtomicClientFile(updates[i].Path, updates[i].Before); err != nil {
			return fmt.Errorf("restore %s: %w", updates[i].Path, err)
		}
	}
	return nil
}

func readBuildMenuMode(path string) (buildMenuMode, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return buildMenuModeDefault, nil
	}
	if err != nil {
		return "", fmt.Errorf("read build menu mode: %w", err)
	}
	var state buildMenuModeState
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("decode build menu mode: %w", err)
	}
	if !isValidBuildMenuMode(state.Mode) {
		return "", fmt.Errorf("saved build menu mode %q is invalid", state.Mode)
	}
	return state.Mode, nil
}

func writeBuildMenuMode(path string, mode buildMenuMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create build menu settings directory: %w", err)
	}
	data, err := json.Marshal(buildMenuModeState{Mode: mode})
	if err != nil {
		return fmt.Errorf("encode build menu mode: %w", err)
	}
	return writeAtomicClientFile(path, append(data, '\n'))
}

func isValidBuildMenuMode(mode buildMenuMode) bool {
	return mode == buildMenuModeDefault ||
		mode == buildMenuModeAllHidden ||
		mode == buildMenuModeEventDecorations
}
