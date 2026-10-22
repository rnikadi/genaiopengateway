// Command gateway runs GenAIOpenGateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/server"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownGrace = 30 * time.Second

func main() {
	path := flag.String("config", "config.yaml", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*path, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(path string, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var st *store.Store
	if cfg.Store.Driver != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		st, err = store.Open(ctx, cfg.Store.Driver, cfg.Store.DSN.Value())
		cancel()
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer st.Close()
	}
	srv, err := server.New(cfg, log, st)
	if err != nil {
		return err
	}
	bg, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	srv.Start(bg)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version, "tls", cfg.TLS.CertFile != "")
		if cfg.TLS.CertFile != "" {
			errc <- httpSrv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			errc <- httpSrv.ListenAndServe()
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case err := <-errc:
			return err
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				reload(path, cfg, srv, log)
				continue
			}
			log.Info("shutting down", "signal", sig.String())
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			err := httpSrv.Shutdown(ctx)
			srv.Close(ctx)
			if err != nil {
				return fmt.Errorf("shutdown: %w", err)
			}
			return nil
		}
	}
}

// reload applies a changed config file. Listener settings need a restart.
func reload(path string, current *config.Config, srv *server.Server, log *slog.Logger) {
	next, err := config.Load(path)
	if err == nil {
		err = srv.Reload(next)
	}
	if err != nil {
		log.Error("config reload failed; keeping the previous config", "err", err)
		return
	}
	if next.Listen != current.Listen || next.TLS != current.TLS {
		log.Warn("listen and tls changes take effect after a restart")
	}
	log.Info("config reloaded")
}
