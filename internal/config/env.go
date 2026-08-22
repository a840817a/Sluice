package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// DefaultAdminPassword is what applyDefaults installs when admin.password is
// left empty. It is a placeholder, not a credential: CheckAdminPassword refuses
// to let the gateway start with it on a reachable address.
const DefaultAdminPassword = "admin"

// Environment overrides exist for two categories only: secrets, which must not
// sit in a mounted file, and deployment topology, which differs between hosts
// running the same image. Tuning knobs (worker.*, window.*, poll_interval,
// cleanup) stay YAML-only — a field earns a variable here only by belonging to
// one of those two categories.
//
// Precedence is YAML, then these, then applyDefaults. Defaults run last and
// only fill values that are still empty, so an override is never clobbered.
func (c *Config) applyEnv() error {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}

	// A _FILE variant reads the value from a path, because Docker and
	// Kubernetes secrets arrive as mounted files rather than as variables.
	// Only secrets get one; nothing else is worth the second lookup.
	secret := func(key string, dst *string) error {
		if path, ok := os.LookupEnv(key + "_FILE"); ok {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("%s_FILE: %w", key, err)
			}
			*dst = strings.TrimSpace(string(b))
			return nil
		}
		str(key, dst)
		return nil
	}

	boolean := func(key string, dst *bool) error {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %q is not a boolean", key, v)
		}
		*dst = b
		return nil
	}

	str("SLUICE_SERVER_ADDR", &c.Server.Addr)
	str("SLUICE_SERVER_BASE_URL", &c.Server.BaseURL)
	str("SLUICE_ADMIN_USERNAME", &c.Admin.Username)
	str("SLUICE_STORE_DATA_DIR", &c.Store.DataDir)
	str("SLUICE_UPSTREAM_AUTH_HEADER", &c.Upstream.AuthHeader)
	str("SLUICE_TLS_IP", &c.Server.TLS.IP)
	str("SLUICE_TLS_CERT_FILE", &c.Server.TLS.CertFile)
	str("SLUICE_TLS_KEY_FILE", &c.Server.TLS.KeyFile)

	if err := secret("SLUICE_ADMIN_PASSWORD", &c.Admin.Password); err != nil {
		return err
	}
	if err := secret("SLUICE_UPSTREAM_AUTH_VALUE", &c.Upstream.AuthValue); err != nil {
		return err
	}
	return boolean("SLUICE_TLS_SELF_SIGNED", &c.Server.TLS.SelfSigned)
}

// CheckAdminPassword refuses the built-in admin password on any address that is
// not loopback-only.
//
// The image ships a config so that `run` works with nothing mounted, which is
// the point of distributing an image at all. The cost of that convenience is
// that nobody is forced to look at admin.password before starting, and the
// admin API it guards can create and delete channels. Failing at startup is
// cheaper than discovering it later; loopback is exempt so local development is
// unaffected.
//
// A password explicitly configured as "admin" is refused too. It is equally
// guessable however it got there, and distinguishing the two would mean
// recording whether applyDefaults supplied it — state that exists only to
// weaken the check.
func (c *Config) CheckAdminPassword() error {
	if c.Admin.Password != DefaultAdminPassword {
		return nil
	}
	if isLoopbackAddr(c.Server.Addr) {
		return nil
	}
	return fmt.Errorf(
		"admin password is the built-in default and %q is reachable off-loopback: "+
			"set SLUICE_ADMIN_PASSWORD (or admin.password in the config file)",
		c.Server.Addr)
}

// isLoopbackAddr reports whether addr binds only to the loopback interface.
// An empty host (":8080") means every interface, which is not loopback-only.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
