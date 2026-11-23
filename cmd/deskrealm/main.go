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
		fmt.Println("deskrealm migrate | bootstrap-ticket | oidc-bind USERNAME SUBJECT | serve")
		return nil
	}
	if len(args) != 1 && !(len(args) == 3 && args[0] == "oidc-bind") {
		return errors.New("expected migrate, bootstrap-ticket, oidc-bind USERNAME SUBJECT or serve")
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
	if args[0] != "bootstrap-ticket" && args[0] != "oidc-bind" && args[0] != "serve" {
		return errors.New("expected migrate, bootstrap-ticket, oidc-bind USERNAME SUBJECT or serve")
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
	if args[0] == "oidc-bind" {
		if len(args) != 3 {
			return errors.New("usage: deskrealm oidc-bind USERNAME SUBJECT")
		}
		issuer := os.Getenv("DESKREALM_OIDC_ISSUER")
		if err := m1.ValidateIssuer(issuer, dev); err != nil {
			return err
		}
		if err := store.BindOIDC(check, issuer, args[2], args[1]); err != nil {
			return errors.New("OIDC binding failed; confirm active account and unique subject")
		}
		return nil
	}
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
	totp, err := m1.ParseTOTPKey(os.Getenv("DESKREALM_TOTP_KEY"))
	if err != nil {
		return err
	}
	managedKey, err := m1.LoadManagedSigningKey(os.Getenv("DESKREALM_SESSION_SIGNING_KEY_FILE"), os.Getenv("DESKREALM_SESSION_SIGNING_KID"))
	if err != nil {
		return err
	}
	transport, err := m1.ConfigureManagedTransport(os.Getenv("DESKREALM_MANAGED_RENDEZVOUS_TOKEN"),
		os.Getenv("DESKREALM_MANAGED_RELAY_ID"), os.Getenv("DESKREALM_MANAGED_RELAY_TOKEN"))
	if err != nil {
		return err
	}
	provider, err := m1.OpenOIDC(check, os.Getenv("DESKREALM_OIDC_ISSUER"),
		os.Getenv("DESKREALM_OIDC_CLIENT_ID"), os.Getenv("DESKREALM_OIDC_CLIENT_SECRET"), origin.String(), dev)
	if err != nil {
		return err
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
	tlsCert, tlsKey := os.Getenv("DESKREALM_TLS_CERT_FILE"), os.Getenv("DESKREALM_TLS_KEY_FILE")
	if (tlsCert == "") != (tlsKey == "") || (tlsCert != "" && origin.Scheme != "https") {
		return errors.New("TLS requires paired certificate and key files and an HTTPS public origin")
	}
	var web fs.FS
	if path := os.Getenv("DESKREALM_WEB_DIR"); path != "" {
		web = os.DirFS(path)
		if info, err := fs.Stat(web, "index.html"); err != nil || !info.Mode().IsRegular() {
			return errors.New("browser build is missing; check DESKREALM_WEB_DIR")
		}
	}
	server := &http.Server{
		Addr: addr, Handler: m1.NewHandler(store, origin, web, m1.AuthConfig{TOTP: totp, OIDC: provider, ManagedKey: managedKey, Transport: transport}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: time.Minute,
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		store.RunTaskWorker(workerCtx)
	}()
	defer func() {
		cancelWorker()
		<-workerDone
	}()
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
	if tlsCert != "" {
		err = server.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return nil
}
