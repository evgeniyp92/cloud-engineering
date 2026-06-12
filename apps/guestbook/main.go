// guestbook is a deliberately minimal API backed by Postgres. It exists so the
// course has something STATEFUL: StatefulSet lessons, NetworkPolicy lessons
// (api -> db traffic), and Velero backup/restore (does the data come back?).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type entry struct {
	ID      int       `json:"id"`
	Message string    `json:"message"`
	Created time.Time `json:"created"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://guestbook:guestbook@guestbook-db:5432/guestbook?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}

	// Retry schema creation: the db pod may come up after us. Failing fast and
	// letting Kubernetes restart us is also valid; we retry to show readiness
	// probes doing their job while we are not yet ready.
	ready := false
	go func() {
		for {
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS entries (
				id SERIAL PRIMARY KEY,
				message TEXT NOT NULL,
				created TIMESTAMPTZ NOT NULL DEFAULT now()
			)`)
			if err == nil {
				ready = true
				slog.Info("database ready")
				return
			}
			slog.Warn("database not ready, retrying", "error", err.Error())
			time.Sleep(2 * time.Second)
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Readiness depends on the DB — this is the classic "ready != alive" example.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready || db.Ping() != nil {
			http.Error(w, `{"status":"database unreachable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"status":"ready"}`))
	})

	mux.HandleFunc("GET /entries", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`SELECT id, message, created FROM entries ORDER BY id DESC LIMIT 50`)
		if err != nil {
			slog.Error("query entries", "error", err)
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		entries := []entry{}
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.ID, &e.Message, &e.Created); err == nil {
				entries = append(entries, e)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	})

	mux.HandleFunc("POST /entries", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Message == "" {
			http.Error(w, `{"error":"body must be {\"message\":\"...\"}"}`, http.StatusBadRequest)
			return
		}
		var e entry
		err := db.QueryRow(
			`INSERT INTO entries (message) VALUES ($1) RETURNING id, message, created`,
			in.Message,
		).Scan(&e.ID, &e.Message, &e.Created)
		if err != nil {
			slog.Error("insert entry", "error", err)
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		slog.Info("entry created", "id", e.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(e)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: mux}

	go func() {
		slog.Info("guestbook starting", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	slog.Warn("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}
