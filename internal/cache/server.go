package cache

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"yadcc-go/internal/buildinfo"
)

type Server struct {
	Addr  string
	Store Store
}

func (s Server) ListenAndServe() error {
	store := s.Store
	if store == nil {
		store = NewMemoryStore()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/cache/stats", func(w http.ResponseWriter, r *http.Request) {
		handleStats(w, r, store)
	})
	mux.HandleFunc("/cache/entry", func(w http.ResponseWriter, r *http.Request) {
		handleEntry(w, r, store)
	})
	mux.HandleFunc("/cache/bloom", func(w http.ResponseWriter, r *http.Request) {
		handleBloom(w, r, store)
	})
	return http.ListenAndServe(s.Addr, mux)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleStats(w http.ResponseWriter, r *http.Request, store Store) {
	stats := store.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": buildinfo.String(),
		"entries": stats.Entries,
		"bytes":   stats.Bytes,
	})
}

func handleEntry(w http.ResponseWriter, r *http.Request, store Store) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing key"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		value, err := store.Get(key)
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(value)
	case http.MethodPut:
		value, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := store.Put(key, value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func handleBloom(w http.ResponseWriter, r *http.Request, store Store) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	keys, err := store.Keys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	filter := NewBloomFilterForKeys(keys)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Yadcc-Bloom-Hashes", strconv.FormatUint(uint64(filter.NumHashes()), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(filter.Bytes())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
