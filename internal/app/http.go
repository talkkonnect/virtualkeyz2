package app

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
)

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		log.Printf("WARNING: HTTP JSON encode: %v", err)
	}
}

func tokenAuthAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validates tokens for the REST HTTP API (placeholder — extend when an API token is configured).
		next.ServeHTTP(w, r)
	})
}

func handleAPIRemoteControl(w http.ResponseWriter, r *http.Request) {
	_ = r
	writeJSONResponse(w, http.StatusOK, map[string]string{"status": "door_opened"})
}

func handleAdminPage(w http.ResponseWriter, r *http.Request) {
	_ = r
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "Local Configuration Interface")
}

func startWebServer(ctx *AppContext) *http.Server {
	_ = ctx
	mux := http.NewServeMux()
	mux.Handle("POST /api/remote-control", tokenAuthAPI(http.HandlerFunc(handleAPIRemoteControl)))
	mux.HandleFunc("GET /admin", handleAdminPage)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	go func() {
		log.Println("INFO: Starting Web Server on port 8080")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("CRITICAL: Web server: %v", err)
		}
	}()
	return srv
}
