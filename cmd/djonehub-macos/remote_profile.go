package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/callsignal"
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relayclient"
	"github.com/iniwex5/vohive/internal/relayprofile"
	"github.com/iniwex5/vohive/internal/remotemedia"
	"github.com/iniwex5/vohive/internal/remotemedia/opus"
	"github.com/pion/webrtc/v4"
)

func systemRemoteProfiles(options relayclient.Options) (*relayprofile.Store, error) {
	vault, err := relayprofile.SystemVault()
	if err != nil {
		return nil, err
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return relayprofile.New(vault, filepath.Join(directory, "DJOneHub", "remote-profile-locks"), options), nil
}

// Native credential management shares the main executable's Keychain identity.
// It exits before modem discovery and is never exposed through the Web UI.
func runRemoteCommand(ctx context.Context, args []string, out io.Writer, factory func(relayclient.Options) (*relayprofile.Store, error)) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(out, `DJOneHub remote identity commands (no modem access):
  enroll --profile NAME --relay https://HOST --enrollment-file /private/grant.json
  status --profile NAME
  pair --profile NAME --modules dji-ID[,dji-ID] --out /private/new-pairing.json
  installations --profile NAME
  revoke --profile NAME --installation ios_ID
  check --profile NAME
  media-check --profile NAME --module dji-ID
Default profile: default. Development HTTP/WS requires --insecure-loopback.
Secrets are saved in macOS Keychain or private output files, never printed.`)
		return nil
	}
	if len(args) == 0 {
		return errors.New("usage: djonehub-macos remote enroll|status|pair|installations|revoke|check|media-check [flags]")
	}
	flags := flag.NewFlagSet("remote "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "default", "local Keychain profile name")
	relayURL := flags.String("relay", "", "Relay HTTPS origin for initial enrollment")
	input := flags.String("enrollment-file", "", "absolute private enrollment file")
	output := flags.String("out", "", "absolute NEW private pairing output file")
	modules := flags.String("modules", "", "comma-separated permitted module IDs")
	module := flags.String("module", "", "module ID for authenticated TURN media diagnostics")
	installation := flags.String("installation", "", "installation ID to revoke")
	insecure := flags.Bool("insecure-loopback", false, "explicit local development HTTP/WS")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return errors.New("invalid remote command arguments")
	}
	if !callsignal.ID(*profile) || len(*profile) > 64 {
		return relayprofile.ErrInvalid
	}
	switch args[0] {
	case "enroll", "status", "pair", "installations", "revoke", "check", "media-check":
	default:
		return errors.New("unknown remote command")
	}
	// Do not silently accept enrollment/file/action flags on another subcommand.
	allowed := map[string]bool{"profile": true, "insecure-loopback": true}
	switch args[0] {
	case "enroll":
		allowed["relay"], allowed["enrollment-file"] = true, true
	case "pair":
		allowed["modules"], allowed["out"] = true, true
	case "revoke":
		allowed["installation"] = true
	case "media-check":
		allowed["module"] = true
	}
	valid := true
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			valid = false
		}
	})
	if !valid {
		return errors.New("flag does not apply to this remote command")
	}
	if args[0] == "media-check" && (!callsignal.ID(*module) || !opus.Available()) {
		return errors.New("media-check requires --module and a remoteopus build")
	}
	store, err := factory(relayclient.Options{AllowInsecureLoopback: *insecure})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch args[0] {
	case "enroll":
		var grant identity.Grant
		if err := relayprofile.ReadPrivateJSON(*input, &grant); err != nil {
			return errors.New("enrollment file must be an owned private regular file with valid JSON")
		}
		if err := store.Enroll(ctx, *profile, *relayURL, grant); err != nil {
			return err
		}
		fmt.Fprintln(out, "Hub identity enrolled and saved in Keychain.")
	case "status":
		status, err := store.Status(*profile)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(status)
	case "pair":
		if !filepath.IsAbs(*output) || *modules == "" {
			return errors.New("pair requires --modules and a new absolute --out path")
		}
		status, err := store.Status(*profile)
		if err != nil {
			return err
		}
		grant, err := store.Pairing(ctx, *profile, strings.Split(*modules, ","))
		if err != nil {
			return err
		}
		if err := relayprofile.WritePrivateJSON(*output, struct {
			Version  int            `json:"version"`
			RelayURL string         `json:"relay_url"`
			Grant    identity.Grant `json:"grant"`
		}{1, status.RelayURL, grant}); err != nil {
			return errors.New("cannot create new private pairing output; generated grant expires in five minutes")
		}
		fmt.Fprintln(out, "One-time pairing written to the private output file; expires in five minutes.")
	case "installations":
		items, err := store.Installations(ctx, *profile)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(items)
	case "revoke":
		if !callsignal.ID(*installation) {
			return errors.New("revoke requires --installation")
		}
		if err := store.RevokeInstallation(ctx, *profile, *installation); err != nil {
			return err
		}
		fmt.Fprintln(out, "Installation revoked.")
	case "check":
		config, err := store.ConnectionConfig(ctx, *profile)
		if err != nil {
			return err
		}
		client, err := callsignal.Dial(ctx, config)
		if err != nil {
			return err
		}
		client.Close()
		fmt.Fprintln(out, "Relay identity and WSS handshake verified; no call was created.")
	case "media-check":
		credentials, err := store.ICE(ctx, *profile, *module)
		if err != nil {
			return err
		}
		if err := credentials.Validate(time.Now()); err != nil {
			return err
		}
		if !credentials.Enabled {
			return errors.New("TURN is not configured")
		}
		passed := true
		for _, server := range credentials.Servers {
			for _, endpoint := range server.URLs {
				transport := "udp"
				if strings.HasPrefix(endpoint, "turns:") {
					transport = "tls"
				} else if strings.HasSuffix(endpoint, "transport=tcp") {
					transport = "tcp"
				}
				probeCtx, cancelProbe := context.WithTimeout(ctx, 8*time.Second)
				result := remotemedia.ProbeRelay(probeCtx, []webrtc.ICEServer{{URLs: []string{endpoint}, Username: server.Username, Credential: server.Credential}})
				cancelProbe()
				if err := json.NewEncoder(out).Encode(struct {
					Transport string `json:"transport"`
					remotemedia.RelayProbe
				}{transport, result}); err != nil {
					return err
				}
				passed = passed && result.Connected && result.SelectedRelayPair
			}
		}
		if !passed {
			return errors.New("TURN media check failed; see stage results")
		}
	}
	return nil
}
