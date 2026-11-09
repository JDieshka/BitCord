package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bitcord/server/internal/config"
	"bitcord/server/internal/db"
	"bitcord/server/internal/httpapi"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()
	logger.Info("starting bitcord server", "addr", cfg.Addr, "public_url", cfg.PublicURL, "version", httpapi.ServerVersion)

	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DBURL)
	if err != nil {
		logger.Error("database connection failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	logger.Info("connected to database")

	if err := db.Migrate(ctx, pool, "migrations"); err != nil {
		logger.Error("migration failed", "err", err)
		os.Exit(1)
	}
	logger.Info("migrations applied")

	srv := httpapi.New(cfg, pool)
	router := srv.BuildRouter()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("http server listening", "addr", cfg.Addr, "tls", cfg.TLSCert != "")
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			if err := httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				os.Exit(1)
			}
		} else {
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				os.Exit(1)
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
}
