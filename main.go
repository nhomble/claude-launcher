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

	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/nodes"
	"github.com/nhomble/claude-launcher/internal/server"
	"github.com/nhomble/claude-launcher/internal/sessions"
)

// version is stamped at build time (see the Makefile).
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-v" || os.Args[1] == "--version") {
		fmt.Println("claude-launcher", version)
		return
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

	mgr := sessions.NewManager(cfg)
	srv, err := server.New(cfg, ring, mgr)
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
