package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// retireWardenSettings removes the retired controller's settings only. Keeping
// unknown keys makes older/future files safe; warden-runs/ stays untouched.
func retireWardenSettings(dataDir string) error {
	path := filepath.Join(dataDir, "warden_settings.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings map[string]json.RawMessage
	if err = json.Unmarshal(raw, &settings); err != nil {
		return err
	}
	changed := false
	for _, key := range []string{"enabled", "autoFlags", "model", "triageAfterMin", "maxRunsPerHour"} {
		if _, ok := settings[key]; ok {
			delete(settings, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if len(settings) == 0 {
		if err = os.Remove(path); err != nil {
			return err
		}
		return syncDir(dataDir)
	}
	cleaned, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(path, cleaned, 0600)
}
