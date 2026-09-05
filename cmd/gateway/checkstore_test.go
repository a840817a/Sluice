package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deploy/bootstrap.sh runs `/sluice -config ... -check-store` inside the real
// image, against the real mount, before it installs a systemd unit. That makes
// three properties of this flag a deployment contract rather than an internal
// detail, and none of them is visible from a unit test of CheckWritable:
//
//   - the flag exists and is spelled -check-store;
//   - it exits non-zero on an unwritable directory and zero on a writable one;
//   - it runs BEFORE the admin-password policy. The preflight happens on a host
//     that has not been configured yet, so a password requirement there would
//     make the check fail for a reason that has nothing to do with the volume —
//     and it would fail identically whether the mount was good or bad.
//
// So this builds the binary and runs it, which is the only way to observe an
// exit code and the order of two early-exit branches.
func TestCheckStoreFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the gateway binary")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so the failure case cannot be provoked")
	}

	bin := filepath.Join(t.TempDir(), "sluice")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// No admin password anywhere: not in the config, not in the environment.
	// That is the state of a host being bootstrapped, and the check must still
	// answer the question it was asked.
	run := func(t *testing.T, dataDir string) (string, error) {
		t.Helper()
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		cfg := "server:\n  addr: \":8080\"\nstore:\n  data_dir: " + dataDir + "\n"
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cmd := exec.Command(bin, "-config", cfgPath, "-check-store")
		cmd.Env = append(os.Environ(), "SLUICE_ADMIN_PASSWORD=")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("writable directory exits zero", func(t *testing.T) {
		out, err := run(t, t.TempDir())
		if err != nil {
			t.Fatalf("exit %v on a writable directory, want 0\n%s", err, out)
		}
	})

	t.Run("unwritable directory exits non-zero", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.Chmod(dataDir, 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })

		out, err := run(t, dataDir)
		if err == nil {
			t.Fatalf("exit 0 on an unwritable directory, want non-zero\n%s", out)
		}
		// A failure for the wrong reason would still exit non-zero, and the
		// script could not tell the difference. Pin that the reported cause is
		// the directory, not the password policy the check runs ahead of.
		if !strings.Contains(out, dataDir) {
			t.Errorf("output does not name the data directory: %s", out)
		}
		if strings.Contains(strings.ToLower(out), "password") {
			t.Errorf("check-store failed on the password policy instead of the data directory: %s", out)
		}
	})
}
