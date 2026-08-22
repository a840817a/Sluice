package config

import (
	"os"
	"path/filepath"
	"testing"
)

// minimalYAML is a valid non-empty document. Tests that assert Load fails must
// not use "": an empty document is accepted (see TestEmptyFileIsNotAnError), so
// an empty fixture would make them pass without exercising anything.
const minimalYAML = "worker:\n  fetch_workers: 4\n"

// write drops a config file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvPrecedence(t *testing.T) {
	const yaml = "admin:\n  password: \"from-yaml\"\nserver:\n  addr: \"127.0.0.1:9999\"\n"

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"yaml wins when no env is set", nil, "from-yaml"},
		{"env beats yaml", map[string]string{"SLUICE_ADMIN_PASSWORD": "from-env"}, "from-env"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := Load(write(t, yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Admin.Password != tc.want {
				t.Errorf("password = %q, want %q", cfg.Admin.Password, tc.want)
			}
		})
	}
}

// Ordering: applyEnv runs before applyDefaults.
//
// A variable that is set but empty is what pins this. When the environment is
// applied first, an empty value leaves the field empty and applyDefaults fills
// it; with the two swapped, the default is installed first and then wiped back
// to empty. Overriding a field with a *non-empty* value cannot detect the swap
// at all — applyEnv overwrites unconditionally, so both orders agree — which is
// why the obvious version of this test passes against the bug.
func TestEnvIsAppliedBeforeDefaults(t *testing.T) {
	t.Setenv("SLUICE_ADMIN_USERNAME", "")
	cfg, err := Load(write(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Username != "admin" {
		t.Errorf("username = %q, want %q — applyDefaults must run after applyEnv",
			cfg.Admin.Username, "admin")
	}
}

// A plain override of a field that also has a default. Order-insensitive by
// itself (see above), but it is the case operators actually rely on.
func TestEnvOverridesDefaultedField(t *testing.T) {
	t.Setenv("SLUICE_STORE_DATA_DIR", "/data")
	cfg, err := Load(write(t, "worker:\n  fetch_workers: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.DataDir != "/data" {
		t.Errorf("data_dir = %q, want /data", cfg.Store.DataDir)
	}
}

// Secrets arrive from Docker and Kubernetes as mounted files, not variables.
func TestSecretFromFile(t *testing.T) {
	sec := filepath.Join(t.TempDir(), "pw")
	// Trailing newline is what `echo secret > file` produces, and is not part
	// of the password.
	if err := os.WriteFile(sec, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLUICE_ADMIN_PASSWORD_FILE", sec)
	t.Setenv("SLUICE_ADMIN_PASSWORD", "from-env")

	cfg, err := Load(write(t, "admin:\n  password: \"from-yaml\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Password != "from-file" {
		t.Errorf("password = %q, want from-file", cfg.Admin.Password)
	}
}

func TestSecretFileMissingIsAnError(t *testing.T) {
	t.Setenv("SLUICE_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := Load(write(t, minimalYAML)); err == nil {
		t.Error("a missing _FILE path must fail loudly, not fall back silently")
	}
}

func TestMalformedBoolRejected(t *testing.T) {
	t.Setenv("SLUICE_TLS_SELF_SIGNED", "yes-please")
	if _, err := Load(write(t, minimalYAML)); err == nil {
		t.Error("malformed boolean accepted")
	}
}

func TestCheckAdminPassword(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		pass    string
		wantErr bool
	}{
		{"default password on all interfaces", ":8080", DefaultAdminPassword, true},
		{"default password on a public address", "0.0.0.0:8080", DefaultAdminPassword, true},
		{"default password on loopback is fine", "127.0.0.1:8080", DefaultAdminPassword, false},
		{"default password on localhost is fine", "localhost:8080", DefaultAdminPassword, false},
		{"default password on IPv6 loopback is fine", "[::1]:8080", DefaultAdminPassword, false},
		{"real password anywhere", ":8080", "s3cret", false},
		// Explicitly configuring "admin" is no safer than defaulting to it.
		{"explicitly configured default", ":8080", "admin", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.Server.Addr = tc.addr
			c.Admin.Password = tc.pass
			err := c.CheckAdminPassword()
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// A mounted but empty config file means "override nothing" — it is how an empty
// ConfigMap or a placeholder arrives — and must not be a parse error.
func TestEmptyFileIsNotAnError(t *testing.T) {
	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatalf("empty config file rejected: %v", err)
	}
	if cfg.Store.DataDir != "./data" {
		t.Errorf("data_dir = %q, want the default", cfg.Store.DataDir)
	}
}
