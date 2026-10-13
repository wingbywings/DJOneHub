package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayadmin"
	"github.com/iniwex5/vohive/internal/relaystatus"
	"github.com/iniwex5/vohive/internal/webadmin"
)

var buildVersion = "web-management-v3"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	flags := flag.NewFlagSet("djonehub-relay-admin", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:18444", "loopback Web administration listener")
	origin := flags.String("public-origin", "", "exact HTTPS administration origin")
	relayOrigin := flags.String("relay-origin", "", "existing native Relay HTTPS origin")
	state := flags.String("state-dir", "", "existing private Relay identity state directory")
	adminDir := flags.String("admin-state-dir", "", "separate private administration directory")
	tenant := flags.String("tenant", "", "tenant administered by this service")
	basePath := flags.String("base-path", "/admin", "administration URL path prefix")
	version := flags.Bool("version", false, "print build version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *version {
		fmt.Println(buildVersion)
		return nil
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || flags.NArg() != 0 {
		return errors.New("administration listener must be explicit loopback")
	}
	if !identity.HTTPSOrigin(*origin) || !identity.HTTPSOrigin(*relayOrigin) {
		return errors.New("administration and Relay HTTPS origins required")
	}
	if _, err := relayadmin.NormalizeBasePath(*basePath); err != nil {
		return err
	}
	auth, err := webadmin.Open(webadmin.Options{Directory: *adminDir, Origin: *origin, Tenant: *tenant, CookieName: "djonehub_relay_admin", LoopbackOnly: true})
	if err != nil {
		return err
	}
	defer auth.Close()
	store, err := identity.Open(*state)
	if err != nil {
		return err
	}
	defer store.Close()
	handler, err := relayadmin.New(relayadmin.Options{Auth: auth, Identity: store, Directory: *adminDir, RelayOrigin: *relayOrigin, Build: buildVersion, BasePath: *basePath, RuntimeStatus: func() relaystatus.Observation {
		return relaystatus.Read(filepath.Join(*state, relaystatus.Filename), time.Now())
	}})
	if err != nil {
		return err
	}
	defer handler.Close()
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Printf("Relay administration listening on %s; private initialization file: %s", *listen, auth.SetupPath())
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
