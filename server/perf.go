package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type perfMark struct {
	Name string `json:"name"`
	Ms   int    `json:"ms"`
}

type perfEntry struct {
	Label string     `json:"label"`
	Marks []perfMark `json:"marks"`
	Ts    string     `json:"ts"`
}

func handlePerf(w http.ResponseWriter, r *http.Request) {
	var entry perfEntry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		jsonErr(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if entry.Ts == "" {
		entry.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	}

	logPath := filepath.Join(logsDir, "perf.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err == nil {
		line, _ := json.Marshal(map[string]any{
			"ts":    entry.Ts,
			"label": entry.Label,
			"marks": entry.Marks,
		})
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			f.Write(line)
			f.WriteString("\n")
			f.Close()
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
