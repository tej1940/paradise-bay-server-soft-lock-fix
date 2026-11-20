package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	worldSnowComponent    = "env_trade_snow"
	worldSnowBackupSuffix = ".before-snow-ground-disable"
)

type worldSnowSettings struct {
	Enabled bool `json:"enabled"`
}

func (s *Store) getWorldSnowSettings() (worldSnowSettings, error) {
	compositionPath, _, err := s.worldLandCompositionPaths()
	if err != nil {
		return worldSnowSettings{}, err
	}
	data, err := os.ReadFile(compositionPath)
	if err != nil {
		return worldSnowSettings{}, fmt.Errorf("read island composition: %w", err)
	}
	_, components, err := decodeWorldLandComposition(data)
	if err != nil {
		return worldSnowSettings{}, err
	}
	_, enabled := components[worldSnowComponent]
	return worldSnowSettings{Enabled: enabled}, nil
}

func (s *Store) setWorldSnowEnabled(enabled bool) error {
	s.clientConfigMu.Lock()
	defer s.clientConfigMu.Unlock()

	compositionPath, backupPath, err := s.worldLandCompositionPaths()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(compositionPath)
	if err != nil {
		return fmt.Errorf("read island composition: %w", err)
	}
	root, components, err := decodeWorldLandComposition(data)
	if err != nil {
		return err
	}
	_, currentlyEnabled := components[worldSnowComponent]
	if currentlyEnabled == enabled {
		return nil
	}

	if enabled {
		original, err := os.ReadFile(backupPath)
		if err != nil {
			return fmt.Errorf("read original island composition backup: %w", err)
		}
		_, originalComponents, err := decodeWorldLandComposition(original)
		if err != nil {
			return fmt.Errorf("decode original island composition backup: %w", err)
		}
		snowComponent, exists := originalComponents[worldSnowComponent]
		if !exists {
			return fmt.Errorf("original island composition backup does not contain %s", worldSnowComponent)
		}
		components[worldSnowComponent] = snowComponent
	} else {
		if err := ensureWorldLandCompositionBackup(backupPath, data); err != nil {
			return err
		}
		delete(components, worldSnowComponent)
	}

	encodedComponents, err := json.Marshal(components)
	if err != nil {
		return fmt.Errorf("encode island composition components: %w", err)
	}
	root["componentConfigs"] = encodedComponents
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode island composition: %w", err)
	}
	updated = append(updated, '\n')
	if err := writeAtomicClientFile(compositionPath, updated); err != nil {
		return err
	}
	return nil
}

func (s *Store) worldLandCompositionPaths() (string, string, error) {
	if s.appxPackageDir == "" {
		return "", "", fmt.Errorf("game package directory is not configured")
	}
	compositionPath := filepath.Join(s.appxPackageDir, "bundle", "data", "env_playerLand.composition.json")
	if _, err := os.Stat(compositionPath); err != nil {
		return "", "", fmt.Errorf("locate island composition %s: %w", compositionPath, err)
	}
	return compositionPath, compositionPath + worldSnowBackupSuffix, nil
}

func decodeWorldLandComposition(data []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("decode island composition: %w", err)
	}
	var components map[string]json.RawMessage
	if err := json.Unmarshal(root["componentConfigs"], &components); err != nil {
		return nil, nil, fmt.Errorf("decode island composition components: %w", err)
	}
	if components == nil {
		return nil, nil, fmt.Errorf("island composition has no componentConfigs object")
	}
	return root, components, nil
}

func ensureWorldLandCompositionBackup(path string, original []byte) error {
	backup, err := os.ReadFile(path)
	if err == nil {
		_, components, decodeErr := decodeWorldLandComposition(backup)
		if decodeErr != nil {
			return fmt.Errorf("decode island composition backup: %w", decodeErr)
		}
		if _, ok := components[worldSnowComponent]; !ok {
			return fmt.Errorf("island composition backup does not contain %s", worldSnowComponent)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read island composition backup: %w", err)
	}
	if _, components, err := decodeWorldLandComposition(original); err != nil {
		return err
	} else if _, ok := components[worldSnowComponent]; !ok {
		return fmt.Errorf("island composition does not contain %s to back up", worldSnowComponent)
	}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		return fmt.Errorf("save original island composition backup: %w", err)
	}
	return nil
}

func writeAtomicClientFile(path string, contents []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".client-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary client configuration: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return fmt.Errorf("write temporary client configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary client configuration: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace client configuration %s: %w", path, err)
	}
	return nil
}
