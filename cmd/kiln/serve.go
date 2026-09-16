package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alternayte/kiln/internal/api"
	"github.com/alternayte/kiln/internal/hostca"
	"github.com/alternayte/kiln/internal/ingress"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/reconcile"
	"github.com/alternayte/kiln/internal/runtime"
	"github.com/alternayte/kiln/internal/sandbox"
	"github.com/alternayte/kiln/internal/snapshot"
	"github.com/alternayte/kiln/internal/store"
	"github.com/alternayte/kiln/internal/template"
)

// cmdServe runs the API server, store, VM supervisor, reconciler, viewer
// login and preview ingress from config.json.
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
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	sbx := sandbox.New(sandbox.Config{
		Root:       root,
		Store:      st,
		Runtime:    rt,
		Network:    nm,
		Pages:      func() snapshot.PageFaultSource { return &snapshot.Process{Binary: exe} },
		KernelPath: mgr.KernelPath,
		Secrets:    cfg.Secrets,
		Zone:       cfg.Zone,
	})

	// The sweep runs before the listener: a restart adopts the microVMs the
	// host still has and destroys what no row owns, so the first request never
	// sees half of a previous daemon.
	reconciler := &reconcile.Reconciler{
		Root:      root,
		Store:     st,
		Sandboxes: sbx,
		Network:   nm,
		Templates: mgr,
		Runtime:   rt,
	}
	if report, err := reconciler.Sweep(ctx); err != nil {
		return fmt.Errorf("serve: reconcile: %w", err)
	} else if n := len(report.Adopted) + len(report.Failed) + len(report.Swept) + len(report.Stuck); n > 0 {
		fmt.Printf("kiln serve: swept: %d adopted, %d failed, %d destroyed, %d templates\n",
			len(report.Adopted), len(report.Failed), len(report.Swept), len(report.Stuck))
	}
	go reconciler.Run(ctx)

	// The viewer login and the preview ingress share this process. The
	// ingress needs a zone and an ACME email; without them the control
	// listener still serves every other endpoint.
	var viewer *ingress.Viewers
	if cfg.Zone != "" {
		auth, authDB, err := ingress.NewAuth(ctx, filepath.Join(root, "kiln.db"), cfg.Zone)
		if err != nil {
			return fmt.Errorf("serve: viewer login: %w", err)
		}
		defer authDB.Close()
		viewer = auth
	}

	apiServer := &api.Server{
		Store:              st,
		Templates:          mgr,
		Sandboxes:          sbx,
		Token:              cfg.BearerToken,
		FirecrackerVersion: firecrackerVersion,
		Root:               root,
		Base:               ctx,
	}
	if viewer != nil {
		apiServer.Viewers = viewer
	}
	handler := apiServer.Handler()

	// The gateway listener is the only way in from another machine, and it
	// demands a client certificate this host signed.
	if cfg.GatewayAddr != "" {
		names := cfg.GatewayNames
		if len(names) == 0 {
			if host := hostnameOf(cfg.GatewayAddr); host != "" {
				names = []string{host}
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("serve: gateway_addr %s binds every address, so config.json needs gateway_names with the hostname or IP a gateway dials", cfg.GatewayAddr)
		}
		tlsConfig, err := hostca.New(root).ServerTLS(names...)
		if err != nil {
			return fmt.Errorf("serve: gateway listener: %w", err)
		}
		gateway := &http.Server{Addr: cfg.GatewayAddr, Handler: handler, TLSConfig: tlsConfig}
		go func() {
			fmt.Printf("kiln serve: gateway on %s (mTLS)\n", cfg.GatewayAddr)
			if err := gateway.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("serve: gateway listener: %v", err)
			}
		}()
		defer gateway.Close()
	}

	srv := &http.Server{
		Addr:              cfg.ControlAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	fmt.Printf("kiln serve: control on %s\n", cfg.ControlAddr)

	var ingressSrv *http.Server
	if cfg.Zone != "" && cfg.ACME.Email != "" && len(cfg.ACME.Credentials) > 0 {
		magic, err := ingress.NewCertMagic(ctx, root, cfg.Zone, ingress.ACMEConfig{
			Email:       cfg.ACME.Email,
			DNSProvider: cfg.ACME.DNSProvider,
			Credentials: cfg.ACME.Credentials,
		})
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		preview := ingress.New(ingress.Config{Zone: cfg.Zone, Store: st, Sandboxes: sbx, Auth: viewer})
		addr := cfg.IngressAddr
		if addr == "" {
			addr = "0.0.0.0:443"
		}
		public, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("serve: ingress: %w", err)
		}
		ingressSrv = &http.Server{
			Addr:              addr,
			Handler:           preview.Handler(),
			TLSConfig:         magic.TLSConfig(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			// ServeTLS adds the HTTP protocols to the certificate manager's
			// ALPN list, which bare tls.NewListener would not do.
			if err := ingressSrv.ServeTLS(public, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "kiln serve: ingress: %v\n", err)
			}
		}()
		fmt.Printf("kiln serve: ingress on %s for *.%s\n", addr, cfg.Zone)
	} else if cfg.Zone != "" {
		fmt.Printf("kiln serve: ingress disabled: config.json has no acme account\n")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if ingressSrv != nil {
			_ = ingressSrv.Shutdown(shutdownCtx)
		}
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

// hostnameOf returns the host part of a listen address, so the certificate
// names what a gateway dials. A bare port answers for every name the CA
// signed, and the gateway verifies the CA, not the name.
func hostnameOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return ""
	}
	return host
}
