package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// configPrefsPath resolves a read-only preferences file (schema, defaults) under
// the bundled config dir. userPrefsPath resolves the writable user overrides under
// the data dir — separated so a packaged build can read defaults from read-only app
// resources while writing user prefs to a writable user-data dir.
func configPrefsPath(name string) string { return filepath.Join(configDir, "preferences", name) }
func userPrefsPath() string              { return filepath.Join(dataDir, "preferences", "user-preferences.json") }

func handlePreferencesSchema(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(configPrefsPath("preferences.schema.json"))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "Failed to load schema: "+err.Error())
		return
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Invalid schema JSON: "+err.Error())
		return
	}
	jsonOK(w, v)
}

func handlePreferencesGet(w http.ResponseWriter, r *http.Request) {
	defaults, err := loadPrefsFile(configPrefsPath("default-preferences.json"))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "Failed to load default preferences: "+err.Error())
		return
	}
	user, _ := loadPrefsFile(userPrefsPath())
	merged := deepMerge(defaults, user)
	jsonOK(w, merged)
}

func handlePreferencesPut(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	path := userPrefsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Failed to create prefs dir: "+err.Error())
		return
	}
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Failed to save preferences: "+err.Error())
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}

func loadPrefsFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}, err
	}
	return m, nil
}

func deepMerge(base, overrides map[string]any) map[string]any {
	result := make(map[string]any, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overrides {
		if bv, ok := result[k]; ok {
			if bMap, ok := bv.(map[string]any); ok {
				if oMap, ok := v.(map[string]any); ok {
					result[k] = deepMerge(bMap, oMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}
