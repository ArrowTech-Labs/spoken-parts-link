// Command spoken-parts-link is the Spoken Parts network link.
//
// It listens for Bambu Lab printers on the local network, reads their status
// over MQTT with the access codes the owner typed on its Home Assistant page,
// uploads the prints Spoken Parts sends to the printer's storage over FTPS,
// and reports status and progress to Spoken Parts over signed HTTPS
// requests. It opens no port to the internet and never sends access codes
// anywhere but to the printer they belong to.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/app"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/config"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/printers"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/privdrop"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/ssdp"
	"github.com/ArrowTech-Labs/spoken-parts-link/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.2.0-dev"

// The unprivileged user the link runs as after start.
const (
	runUID = 65532
	runGID = 65532
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spoken-parts-link:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	red := redact.New()
	lg := logx.New(os.Stderr, red, logx.ParseLevel(cfg.LogLevel))
	log.SetFlags(0)
	log.SetOutput(lg.Writer(logx.Warning))

	// Give up root before any listener starts or any untrusted input is read.
	dropped, err := privdrop.Drop(cfg.DataDir, runUID, runGID)
	if err != nil {
		return fmt.Errorf("dropping privileges: %w", err)
	}
	if dropped {
		lg.Info("running as an unprivileged user", "uid", runUID)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	bundle, err := printers.LoadBundle()
	if err != nil {
		return fmt.Errorf("printer CA bundle: %w", err)
	}
	if cfg.Server.String() != "https://spokenparts.com" {
		lg.Warn("using a custom Spoken Parts server", "server", cfg.Server.String())
	}
	a, err := app.New(ctx, app.Config{
		DataDir: cfg.DataDir,
		Version: version,
		Server:  cfg.Server,
		Log:     lg,
		Bundle:  bundle,
		// Off unless SP_LINK_AUTOSTART=true (Developer Mode printers only).
		AutoStart: cfg.AutoStart,
	})
	if err != nil {
		return err
	}
	defer a.Close()

	srv, err := web.New(a, web.Options{Allow: cfg.Allow, Log: lg, Red: red})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("web listener on %s: %w", cfg.Listen, err)
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(lg.Writer(logx.Debug), "", 0),
	}
	lg.Info("link page listening", "addr", ln.Addr().String())
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("web server stopped", "err", err)
			stop()
		}
	}()

	conn, err := ssdp.Listen(cfg.SSDP, true, lg)
	if err != nil {
		lg.Error("printer discovery is off: could not listen", "addr", cfg.SSDP, "err", err)
	} else {
		lg.Info("listening for printers", "addr", conn.LocalAddr().String())
		go func() {
			if err := ssdp.NewListener(conn, lg, a.Observe).Run(ctx); err != nil {
				lg.Error("printer discovery stopped", "err", err)
			}
		}()
	}

	a.Run(ctx)

	lg.Info("stopping")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return nil
}
