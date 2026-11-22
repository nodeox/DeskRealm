package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"deskrealm/internal/m1"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "deskrealm:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Println("deskrealm migrate | bootstrap-ticket | serve")
		return nil
	}
	if len(args) != 1 {
		return errors.New("expected migrate, bootstrap-ticket or serve")
	}
	dsn := os.Getenv("DESKREALM_DATABASE_URL")
	if args[0] == "migrate" {
		if override := os.Getenv("DESKREALM_MIGRATION_DATABASE_URL"); override != "" {
			dsn = override
		}
		if dsn == "" {
			return errors.New("database URL is required")
		}
		if err := m1.Migrate(ctx, dsn); err != nil {
			return errors.New("database migration failed")
		}
		return nil
	}
	if args[0] != "bootstrap-ticket" && args[0] != "serve" {
		return errors.New("expected migrate, bootstrap-ticket or serve")
	}
	if dsn == "" {
		return errors.New("DESKREALM_DATABASE_URL is required")
	}
	dev := os.Getenv("DESKREALM_DEV_HTTP") == "1"
	origin, err := m1.ParseOrigin(os.Getenv("DESKREALM_PUBLIC_ORIGIN"), dev)
	if err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer db.Close()
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.Ping(check); err != nil {
		return errors.New("database unavailable")
	}
	store := m1.Store{DB: db}
	if args[0] == "bootstrap-ticket" {
		ticket, err := store.IssueTicket(check)
		if err != nil {
			return errors.New("cannot issue initialization ticket; check migrations and initialization status")
		}
		fmt.Printf("%s/initialize#ticket=%s\n", origin, ticket)
		return nil
	}
	if _, err := store.Initialized(check); err != nil {
		return errors.New("database schema unavailable; run deskrealm migrate")
	}
	addr := os.Getenv("DESKREALM_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if origin.Scheme == "http" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || !m1.Loopback(host) {
			return errors.New("development HTTP must bind to loopback")
		}
	}
	var web fs.FS
	if path := os.Getenv("DESKREALM_WEB_DIR"); path != "" {
		web = os.DirFS(path)
		if info, err := fs.Stat(web, "index.html"); err != nil || !info.Mode().IsRegular() {
			return errors.New("browser build is missing; check DESKREALM_WEB_DIR")
		}
	}
	server := &http.Server{
		Addr: addr, Handler: m1.NewHandler(store, origin, web),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: time.Minute,
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return nil
}
