// claude-launcher — a small web UI that spawns remote-controlled `claude`
// sessions (via a chosen shell, in a chosen directory) and tracks them.
// Control of the sessions themselves happens at claude.ai/code or the mobile
// Code tab; this server only launches, lists, and kills them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/nodes"
	"github.com/nhomble/claude-launcher/internal/server"
	"github.com/nhomble/claude-launcher/internal/sessions"
	"github.com/nhomble/claude-launcher/internal/shells"
)

// version is stamped at build time (see the Makefile).
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version":
			fmt.Println("claude-launcher", version)
			return
		// The built-in catalog, commented — `--dump-config > config.yaml` is how
		// you start editing the model and shell lists.
		case "--dump-config":
			os.Stdout.Write(catalog.Defaults())
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "claude-launcher:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()

	ring, err := nodes.New(cfg)
	if err != nil {
		return err
	}

	// The catalog (models + shells) is config.yaml if there is one, else the
	// built-in defaults. It reloads itself when the file changes.
	catalogPath, err := catalog.FindConfig(cfg.ConfigFile)
	if err != nil {
		return err
	}
	catalogStore, err := catalog.New(catalogPath)
	if err != nil {
		return err
	}
	reg := shells.NewRegistry(catalogStore)

	mgr := sessions.NewManager(cfg, catalogStore, reg)
	srv, err := server.New(cfg, ring, mgr, catalogStore, reg)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Sessions are owned by this process — kill them on the way out rather than
	// leaving orphaned `claude` processes attached to dead PTYs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("shutting down; killing live sessions")
		mgr.Shutdown()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Println(srv.Banner(cfg.Port))
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
