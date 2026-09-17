package main

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"time"
)

type server struct {
	db *sql.DB
}

func main() {
	addr := getenv("GT_ADDR", ":7105")
	dbPath := getenv("GT_DB_PATH", "gongtong.db")

	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if getenv("GT_SEED", "true") == "true" {
		if err := seed(db); err != nil {
			log.Fatalf("seed: %v", err)
		}
	}

	s := &server{db: db}
	mux := http.NewServeMux()

	// 公开
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": nowUTC()})
	})

	// 认证
	mux.HandleFunc("POST /api/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.requireAuth(s.handleMe))
	mux.HandleFunc("GET /api/sites", s.requireAuth(s.handleListSites))
	mux.HandleFunc("GET /api/dashboard", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("POST /api/alerts/{id}/read", s.requireAuth(s.handleReadAlert))
	mux.HandleFunc("POST /api/alerts/read-all", s.requireAuth(s.handleReadAllAlerts))
	mux.HandleFunc("GET /api/workers", s.requireAuth(s.handleListWorkers))
	mux.HandleFunc("GET /api/workers/me", s.requireAuth(s.handleMyWorker))
	mux.HandleFunc("GET /api/workers/{id}", s.requireAuth(s.handleGetWorker))
	mux.HandleFunc("POST /api/workers/{id}/transition", s.requireAuth(s.handleTransition))
	mux.HandleFunc("POST /api/workers/{id}/reveal-name", s.requireAuth(s.handleRevealName))
	mux.HandleFunc("POST /api/sync/batch", s.requireAuth(s.handleSyncBatch))

	handler := corsMiddleware(logMiddleware(mux))
	log.Printf("工瞳后端启动，监听 %s", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
