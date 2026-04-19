package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/wiesty/bmw-cardata-bridge/internal/bmw"
)

func RegisterHandlers(mux *http.ServeMux, cache *bmw.Cache) {
	mux.HandleFunc("/health", healthHandler(cache))
	mux.HandleFunc("/vehicle", vehicleHandler(cache))
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(openapiSpec))
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(swaggerUI))
	})
}

func healthHandler(cache *bmw.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data := cache.Get()
		if data == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"status": "starting", "last_update": nil})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"last_update": data.LastUpdate.Format(time.RFC3339),
		})
	}
}

func vehicleHandler(cache *bmw.Cache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data := cache.Get()
		if data == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "no data available yet"})
			return
		}
		json.NewEncoder(w).Encode(data)
	}
}
