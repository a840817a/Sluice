package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/a840817a/sluice/internal/config"
)

// runHealthcheck probes this gateway's own /healthz and reports whether it is
// serving.
//
// It lives in the gateway binary because the runtime image is distroless: there
// is no curl or wget to call from HEALTHCHECK, and adding one would give up the
// small, shell-less base image that is the reason for choosing it. The binary
// is already in the image, so it costs nothing.
func runHealthcheck(cfg *config.Config) error {
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}

	// A self-signed certificate cannot verify against anything, and this
	// connection never leaves loopback — the check is "is my own listener
	// answering", not "is this peer who it claims to be".
	if cfg.Server.TLS.SelfSigned || (cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "") {
		scheme = "https"
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback self-check
		}
	}

	resp, err := client.Get(scheme + "://" + healthcheckHostPort(cfg.Server.Addr) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned %s", resp.Status)
	}
	return nil
}

// healthcheckHostPort turns a listen address into one that can be dialled from
// inside the same container. A wildcard listen address (":8080", "0.0.0.0:8080",
// "[::]:8080") is not a destination, so it becomes loopback.
func healthcheckHostPort(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1" + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
