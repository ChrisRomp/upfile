package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"upfile/internal/app"
	"upfile/internal/auth"
	"upfile/web"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func integer(name string, fallback int64) (int64, error) {
	if v := os.Getenv(name); v != "" {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		return n, nil
	}
	return fallback, nil
}
func nativeInteger(name string, fallback int) (int, error) {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		return n, nil
	}
	return fallback, nil
}
func run() error {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		_, port, err := net.SplitHostPort(env("UPFILE_PUBLIC_ADDR", ":8080"))
		if err != nil {
			return fmt.Errorf("invalid public listen address: %w", err)
		}
		r, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
		if err != nil {
			return err
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return errors.New("service is not ready")
		}
		return nil
	}
	c := app.Config{DataDir: env("UPFILE_DATA_DIR", "/data"), PublicOrigin: os.Getenv("UPFILE_PUBLIC_ORIGIN"), AdminOrigin: os.Getenv("UPFILE_ADMIN_ORIGIN")}
	numbers := map[string]*int64{"UPFILE_CHUNK_BYTES": &c.ChunkBytes, "UPFILE_HEADROOM_BYTES": &c.HeadroomBytes}
	for name, p := range numbers {
		v, e := integer(name, 0)
		if e != nil {
			return e
		}
		*p = v
	}
	maxRecords, err := nativeInteger("UPFILE_MAX_RECORDS", 100000)
	if err != nil {
		return err
	}
	maxActive, err := nativeInteger("UPFILE_MAX_ACTIVE", 8)
	if err != nil {
		return err
	}
	c.MaxRecords, c.MaxActive = maxRecords, maxActive
	for name, p := range map[string]*time.Duration{"UPFILE_LEASE_SECONDS": &c.Lease, "UPFILE_SESSION_SECONDS": &c.SessionTTL, "UPFILE_RETENTION_SECONDS": &c.Retention} {
		v, e := integer(name, 0)
		if e != nil {
			return e
		}
		if v < 0 || v > 315360000 {
			return fmt.Errorf("%s outside supported range", name)
		}
		*p = time.Duration(v) * time.Second
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	verify, err := auth.New(ctx, os.Getenv("UPFILE_AUTH_ISSUER"), os.Getenv("UPFILE_AUTH_AUDIENCE"), &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	a, err := app.New(c, verify, web.Assets())
	if err != nil {
		return err
	}
	defer a.Close()
	public := &http.Server{Addr: env("UPFILE_PUBLIC_ADDR", ":8080"), Handler: a.Public(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	admin := &http.Server{Addr: env("UPFILE_ADMIN_ADDR", ":8081"), Handler: a.Admin(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	errs := make(chan error, 2)
	go func() { errs <- public.ListenAndServe() }()
	go func() { errs <- admin.ListenAndServe() }()
	slog.Info("upfile started", "public", public.Addr, "admin", admin.Addr)
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownServers(shutdown, public, admin)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func shutdownServers(ctx context.Context, servers ...*http.Server) {
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Shutdown(ctx); err != nil {
				slog.Warn("forcing listener shutdown", "address", server.Addr, "error", err)
				if err := server.Close(); err != nil {
					slog.Error("force-close listener failed", "address", server.Addr, "error", err)
				}
			}
		}()
	}
	wg.Wait()
}

func main() {
	if err := run(); err != nil {
		slog.Error("upfile stopped", "error", err)
		os.Exit(1)
	}
}
