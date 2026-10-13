package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/iniwex5/vohive/internal/desktopopen"
	"github.com/iniwex5/vohive/internal/relayprofile"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func runWebOpen(args []string) error {
	f := flag.NewFlagSet("web-open", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	path := f.String("native-control-file", "", "private launcher bootstrap")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return errors.New("invalid activation arguments")
	}
	var bootstrap nativeBootstrap
	if relayprofile.ReadPrivateJSON(*path, &bootstrap) != nil || bootstrap.Version != 1 || len(bootstrap.Token) != 64 {
		return errors.New("invalid native bootstrap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link, err := requestWebTicket(ctx, bootstrap)
	if err != nil {
		return err
	}
	return desktopopen.Open(link)
}
func loopbackOrigin(link string) bool {
	u, err := url.Parse(link)
	return err == nil && u.Scheme == "http" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Path == "" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()
}
func requestWebTicket(ctx context.Context, bootstrap nativeBootstrap) (string, error) {
	if !loopbackOrigin(bootstrap.BaseURL) {
		return "", relayprofile.ErrInvalid
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, bootstrap.BaseURL+"/web-ticket", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	r.Header.Set("Authorization", "Bearer "+bootstrap.Token)
	r.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return relayprofile.ErrInvalid }}
	response, err := client.Do(r)
	if err != nil {
		return "", relayprofile.ErrInvalid
	}
	defer response.Body.Close()
	var body struct {
		URL string `json:"url"`
	}
	d := json.NewDecoder(io.LimitReader(response.Body, 2049))
	d.DisallowUnknownFields()
	if response.StatusCode != 200 || d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF || len(body.URL) > 2048 {
		return "", relayprofile.ErrInvalid
	}
	u, err := url.Parse(body.URL)
	if err != nil || u.Path != "/remote/" || !strings.HasPrefix(u.Fragment, "remote-ticket=") || len(strings.TrimPrefix(u.Fragment, "remote-ticket=")) != 43 {
		return "", relayprofile.ErrInvalid
	}
	u.Path, u.Fragment = "", ""
	if !loopbackOrigin(u.String()) {
		return "", relayprofile.ErrInvalid
	}
	return body.URL, nil
}
