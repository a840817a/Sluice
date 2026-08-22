package main

import (
	"crypto/x509"
	"net"
	"strings"
	"testing"
)

// parseSelfSigned generates a certificate and returns the parsed leaf.
func parseSelfSigned(t *testing.T, configured string) *x509.Certificate {
	t.Helper()
	cert, err := selfSignedCert(configured)
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return leaf
}

func hasIP(leaf *x509.Certificate, want string) bool {
	target := net.ParseIP(want)
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}

// Loopback is unconditional: a self-signed cert must be usable on the machine
// that made it without configuring anything.
func TestSelfSignedAlwaysCoversLoopback(t *testing.T) {
	leaf := parseSelfSigned(t, "")
	for _, want := range []string{"127.0.0.1", "::1"} {
		if !hasIP(leaf, want) {
			t.Errorf("SAN missing %s: %v", want, leaf.IPAddresses)
		}
	}
	if len(leaf.DNSNames) == 0 || leaf.DNSNames[0] != "localhost" {
		t.Errorf("SAN missing localhost: %v", leaf.DNSNames)
	}
}

// The configured value is additive. Before this it was the only entry, so
// setting it removed loopback and every extra address needed its own restart.
func TestSelfSignedConfiguredIsAdditive(t *testing.T) {
	leaf := parseSelfSigned(t, "203.0.113.5")
	if !hasIP(leaf, "203.0.113.5") {
		t.Errorf("configured IP missing: %v", leaf.IPAddresses)
	}
	if !hasIP(leaf, "127.0.0.1") {
		t.Errorf("configuring an IP dropped loopback: %v", leaf.IPAddresses)
	}
}

// A list, because one gateway is reached at several addresses at once, and DNS
// names because not every deployment is addressed by IP.
func TestSelfSignedAcceptsListAndDNSNames(t *testing.T) {
	leaf := parseSelfSigned(t, "203.0.113.5, 198.51.100.7 ,gw.example.com")

	for _, want := range []string{"203.0.113.5", "198.51.100.7"} {
		if !hasIP(leaf, want) {
			t.Errorf("SAN missing %s: %v", want, leaf.IPAddresses)
		}
	}
	var found bool
	for _, n := range leaf.DNSNames {
		if n == "gw.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("SAN missing DNS name: %v", leaf.DNSNames)
	}
}

// Duplicates would otherwise accumulate when a configured address is also an
// interface address, which is the common case on a bare-metal host.
func TestSelfSignedDeduplicates(t *testing.T) {
	leaf := parseSelfSigned(t, "203.0.113.5,203.0.113.5")
	var n int
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "203.0.113.5" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("203.0.113.5 appears %d times, want 1: %v", n, leaf.IPAddresses)
	}
}

// Interface enumeration is a fallback for bare metal. It cannot see the host's
// LAN address from inside a container, so this asserts only that it contributes
// something on a machine that has a non-loopback interface — not that any
// particular address is present.
func TestSelfSignedIncludesInterfaceAddresses(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("no interface addresses available")
	}
	var want string
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			want = n.IP.String()
			break
		}
	}
	if want == "" {
		t.Skip("host has no routable interface address")
	}
	if !hasIP(parseSelfSigned(t, ""), want) {
		t.Errorf("interface address %s not in SAN", want)
	}
}

func TestSanEntriesIgnoresBlanks(t *testing.T) {
	ips, dns := sanEntries(" , ,")
	if len(dns) != 1 || dns[0] != "localhost" {
		t.Errorf("blank entries became DNS names: %v", dns)
	}
	for _, ip := range ips {
		if strings.TrimSpace(ip.String()) == "" {
			t.Error("blank entry became an IP")
		}
	}
}
