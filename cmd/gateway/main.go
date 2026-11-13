// Command gateway runs GenAIOpenGateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/kms"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/server"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownGrace = 50 * time.Second

func main() {
	config.RegisterUnwrapper(kms.Unwrap)
	if ok, code := subcommand(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(code)
	}
	path := flag.String("config", "config.yaml", "path to the config file; without one the gateway starts with zero config")
	dataDir := flag.String("data", envOr("GOG_DATA_DIR", config.DefaultDataDir), "data directory for a start without a config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	// The image's entrypoint passes -config before any arguments, so a
	// subcommand can also follow the global flags, which it inherits.
	if flag.NArg() > 0 {
		args := append([]string{flag.Arg(0), "-config", *path, "-data", *dataDir}, flag.Args()[1:]...)
		if ok, code := subcommand(args, os.Stdout, os.Stderr); ok {
			os.Exit(code)
		}
	}
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*path, *dataDir, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(path, dataDir string, log *slog.Logger) error {
	cfg, boot, err := loadConfig(path, dataDir)
	if err != nil {
		return err
	}
	if boot != nil {
		path = "" // nothing to reload on SIGHUP
	}
	mcpgw.Version = version
	server.Version = version
	shutdownTracing, tracing, err := telemetry.SetupTracing(context.Background(), version)
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			log.Error("flushing traces failed", "err", err)
		}
	}()

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

	errc := make(chan error, 2)
	var metricsSrv *http.Server
	if cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", srv.MetricsHandler())
		metricsSrv = &http.Server{Addr: cfg.Metrics.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("serving metrics", "addr", cfg.Metrics.Listen)
			if err := metricsSrv.ListenAndServe(); err != http.ErrServerClosed {
				errc <- fmt.Errorf("metrics listener: %w", err)
			}
		}()
	}
	if boot != nil {
		announceZeroConfig(os.Stderr, cfg, boot, log)
	}
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", version, "tls", cfg.TLS.CertFile != "", "tracing", tracing)
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
			srv.Drain()
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			err := httpSrv.Shutdown(ctx)
			if metricsSrv != nil {
				_ = metricsSrv.Shutdown(ctx)
			}
			srv.Close(ctx)
			if err != nil {
				return fmt.Errorf("shutdown: %w", err)
			}
			return nil
		}
	}
}

// loadConfig reads the config file, or, when there is none, builds the
// zero config with its data directory (docs/quickstart.md).
func loadConfig(path, dataDir string) (*config.Config, *config.Bootstrap, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config.ZeroConfig(dataDir)
	}
	return cfg, nil, err
}

// announceZeroConfig tells the person who just started the gateway where
// the UI is and how to sign in. The token is printed only on the start
// that generated it.
func announceZeroConfig(w *os.File, cfg *config.Config, b *config.Bootstrap, log *slog.Logger) {
	log.Warn("no config file; started with zero config", "data_dir", b.DataDir, "bootstrap_file", b.File)
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	url := fmt.Sprintf("http://%s/ui/", net.JoinHostPort(host, port))
	token := "saved in " + b.File
	if b.Created {
		token = b.AdminToken.Value() + "  (also saved in " + b.File + ")"
	}
	fmt.Fprintf(w, "\nGenAIOpenGateway %s is running with no config file.\n\n", version)
	fmt.Fprintf(w, "  Admin UI:     %s\n", url)
	fmt.Fprintf(w, "  Admin token:  %s\n", token)
	fmt.Fprintf(w, "  Data:         %s (SQLite, in-memory limits)\n\n", b.DataDir)
	fmt.Fprintf(w, "Open the UI, sign in with the token, and the setup wizard adds your first provider.\n")
	fmt.Fprintf(w, "For production, write a config file instead: see docs/config.md.\n\n")
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// reload applies a changed config file. Listener settings need a restart.
func reload(path string, current *config.Config, srv *server.Server, log *slog.Logger) {
	if path == "" {
		log.Info("started with zero config; nothing to reload")
		return
	}
	next, err := config.Load(path)
	if err == nil {
		err = srv.Reload(next)
	}
	if err != nil {
		log.Error("config reload failed; keeping the previous config", "err", err)
		return
	}
	if next.Listen != current.Listen || next.TLS != current.TLS || next.Metrics != current.Metrics {
		log.Warn("listen, tls and metrics changes take effect after a restart")
	}
	log.Info("config reloaded")
}
