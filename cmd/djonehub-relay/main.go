package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iniwex5/vohive/internal/relay"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/turncred"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: djonehub-relay serve|provision|lookup|revoke [flags]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	directory := flags.String("state-dir", "", "absolute private directory for SQLite state")
	listen := flags.String("listen", "127.0.0.1:8443", "relay listen address")
	cert := flags.String("tls-cert", "", "TLS certificate file")
	key := flags.String("tls-key", "", "TLS private key file")
	insecure := flags.Bool("insecure-loopback", false, "explicit development/reverse-proxy HTTP on a literal loopback address")
	tenant := flags.String("tenant", "", "tenant ID for offline Hub enrollment")
	modules := flags.String("modules", "", "comma-separated module IDs for offline Hub enrollment")
	output := flags.String("out", "", "new 0600 enrollment output file; never stdout")
	id := flags.String("id", "", "identity ID to revoke locally")
	turnFile := flags.String("turn-config", "", "private JSON coturn REST configuration file")
	fingerprint := flags.String("key-fingerprint", "", "SHA-256 public key fingerprint for offline recovery lookup")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*directory) {
		return errors.New("--state-dir must be absolute; unexpected positional arguments are rejected")
	}
	if args[0] != "serve" && args[0] != "provision" && args[0] != "revoke" && args[0] != "lookup" {
		return errors.New("unknown relay command")
	}
	if *turnFile != "" && args[0] != "serve" {
		return errors.New("--turn-config applies only to serve")
	}
	if args[0] == "serve" {
		if err := validateTransport(*listen, *cert, *key, *insecure); err != nil {
			return err
		}
	}
	store, err := identity.Open(*directory)
	if err != nil {
		return err
	}
	defer store.Close()
	switch args[0] {
	case "lookup":
		items, err := store.AdminLookupFingerprint(context.Background(), *fingerprint)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(items)
	case "provision":
		if !filepath.IsAbs(*output) {
			return errors.New("--out must be an absolute new private file")
		}
		file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		ok := false
		defer func() {
			file.Close()
			if !ok {
				os.Remove(*output)
			}
		}()
		grant, err := store.ProvisionEnrollment(context.Background(), *tenant, strings.Split(*modules, ","))
		if err != nil {
			return err
		}
		if err = json.NewEncoder(file).Encode(grant); err == nil {
			err = file.Sync()
		}
		if err != nil {
			return err
		}
		ok = true
		log.Print("one-time Hub enrollment written to the private output file; expires in 10 minutes")
		return nil
	case "revoke":
		return store.AdminRevoke(context.Background(), *id)
	}
	var issuer *turncred.Issuer
	if *turnFile != "" {
		issuer, err = turncred.ReadConfig(*turnFile)
		if err != nil {
			return err
		}
	}
	handler := relay.NewServer(store, relay.WithTURN(issuer))
	defer handler.Close()
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() {
		if *cert != "" {
			result <- server.ListenAndServeTLS(*cert, *key)
		} else {
			result <- server.ListenAndServe()
		}
	}()
	log.Printf("DJOneHub relay identity and online signaling listening on %s; VoIP push is not enabled", *listen)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		handler.Close() // net/http Shutdown does not close hijacked WebSockets.
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
func validateTransport(listen, cert, key string, insecure bool) error {
	if cert != "" && key != "" {
		return nil
	}
	if cert != "" || key != "" {
		return errors.New("both --tls-cert and --tls-key are required")
	}
	host, _, err := net.SplitHostPort(listen)
	if !insecure || err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("TLS is required; plaintext requires --insecure-loopback and a literal loopback listen address")
	}
	return nil
}
