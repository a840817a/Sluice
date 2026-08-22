package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/a840817a/sluice/internal/config"
)

// sanEntries builds the Subject Alternative Name set for the self-signed
// certificate.
//
// Loopback is always present, so a self-signed cert is usable on the machine
// that generated it without configuring anything.
//
// configured is a comma-separated list of IP addresses and DNS names, and is
// *additive* — it used to be the only source, which meant one entry per
// certificate and a config edit plus a restart for every new test address. A
// comma-separated string rather than a YAML sequence because the same value has
// to arrive through SLUICE_TLS_IP, and environment variables have no lists.
//
// Interface addresses are a fallback, not the mechanism. Inside a container the
// interfaces are the container's: the bridge address (172.x), never the host's
// LAN address that a phone would actually connect to. Anything reachable from
// another machine has to come from configured — see docs/plans for why
// detection belongs on the host.
func sanEntries(configured string) (ips []net.IP, dnsNames []string) {
	ips = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	dnsNames = []string{"localhost"}

	seen := map[string]bool{}
	addIP := func(ip net.IP) {
		if ip == nil || seen[ip.String()] {
			return
		}
		seen[ip.String()] = true
		ips = append(ips, ip)
	}

	for _, entry := range strings.Split(configured, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			addIP(ip)
		} else {
			dnsNames = append(dnsNames, entry)
		}
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips, dnsNames
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		addIP(ipNet.IP)
	}
	return ips, dnsNames
}

// selfSignedCert generates an in-memory self-signed TLS certificate covering
// every name in sanEntries. The browser still shows a trust warning — that is
// inherent to self-signing — but a name mismatch, which cannot be clicked
// through, no longer happens for an address that was configured.
func selfSignedCert(configured string) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	ips, dnsNames := sanEntries(configured)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"DASH Gateway"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     dnsNames,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}

func main() {
	// SLUICE_CONFIG is the env form of -config. Both exist because a container
	// image sets environment variables far more easily than it rewrites the
	// command line — Kubernetes in particular. The flag still wins when given.
	defaultCfg := "config.yaml"
	if v, ok := os.LookupEnv("SLUICE_CONFIG"); ok && v != "" {
		defaultCfg = v
	}
	cfgPath := flag.String("config", defaultCfg, "path to config file (env: SLUICE_CONFIG)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// The image ships a config so it runs with nothing mounted, which means
	// nobody is made to look at admin.password before starting. Refuse the
	// built-in one on any address that is not loopback rather than serve an
	// admin API that can create and delete channels behind admin/admin.
	if err := cfg.CheckAdminPassword(); err != nil {
		slog.Error("refusing to start", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Everything the gateway is made of — store, manager, callbacks, restored
	// channels, router — is composed in newGateway (gateway.go), which is where
	// the tests can reach it.
	gw, err := newGateway(ctx, *cfg)
	if err != nil {
		slog.Error("gateway init failed", "err", err)
		os.Exit(1)
	}

	// --- HTTP server ---
	httpServer := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      gw.handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.Server.Addr)
		var serveErr error
		tlsCfg := cfg.Server.TLS
		switch {
		case tlsCfg.SelfSigned:
			cert, err := selfSignedCert(tlsCfg.IP)
			if err != nil {
				slog.Error("self-signed cert generation failed", "err", err)
				cancel()
				return
			}
			httpServer.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
			ips, dnsNames := sanEntries(tlsCfg.IP)
			slog.Info("TLS: using self-signed certificate",
				"san_ips", ips, "san_dns", dnsNames, "base_url", cfg.Server.BaseURL)
			serveErr = httpServer.ListenAndServeTLS("", "")
		case tlsCfg.CertFile != "" && tlsCfg.KeyFile != "":
			slog.Info("TLS: using provided certificate", "cert", tlsCfg.CertFile)
			serveErr = httpServer.ListenAndServeTLS(tlsCfg.CertFile, tlsCfg.KeyFile)
		default:
			serveErr = httpServer.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			slog.Error("http server error", "err", serveErr)
			cancel()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	// Drain in-flight HTTP requests first, then stop ingest workers.
	// This order prevents handlers from referencing channels that are already
	// cancelled: HTTP requests finish against live state, then ingest stops.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown error", "err", err)
	}

	gw.manager.StopAll()
}
