package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// cmdServe runs the API server, store and VM supervisor on the control
// listener from config.json.
func cmdServe() error {
	root := os.Getenv("KILN_ROOT")
	if root == "" {
		root = defaultRoot
	}
	cfgPath := filepath.Join(root, "config.json")
	cfg, err := readConfig(cfgPath)
	if err != nil {
		return err
	}
	if cfg.BearerToken == "" {
		return fmt.Errorf("serve: %s has no bearer_token; run kiln init", cfgPath)
	}
	if cfg.ControlAddr == "" {
		cfg.ControlAddr = "127.0.0.1:8080"
	}
	kilninit := filepath.Join(root, "bin", "kilninit")
	if _, err := os.Stat(kilninit); err != nil {
		return fmt.Errorf("serve: %w; run kiln init", err)
	}

	st, err := store.Open(filepath.Join(root, "kiln.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	rt, err := runtime.New(runtime.Config{Root: root})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	nm := network.New()
	if err := nm.EnsureBase(ctx); err != nil {
		return err
	}
	mgr := &template.Manager{
		Root:       root,
		Store:      st,
		Runtime:    rt,
		Network:    nm,
		Builder:    template.Builder{KilninitPath: kilninit},
		KernelPath: filepath.Join(root, "kernel", "vmlinux-"+kernelVersion),
	}

	srv := &http.Server{
		Addr:              cfg.ControlAddr,
		Handler:           (&api.Server{Store: st, Templates: mgr, Token: cfg.BearerToken, Base: ctx}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	fmt.Printf("kiln serve: control on %s\n", cfg.ControlAddr)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// readConfig reads config.json written by kiln init.
func readConfig(path string) (configFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return configFile{}, fmt.Errorf("serve: %w; run kiln init", err)
	}
	var cfg configFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		return configFile{}, fmt.Errorf("serve: %s: %w", path, err)
	}
	return cfg, nil
}
