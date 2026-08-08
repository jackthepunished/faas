package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
	"github.com/onebox-faas/faas/pkg/releasebundle"
)

type hostRuntime struct {
	unitDir      string
	databaseURL  string
	serviceOrder []string
	readyTimeout time.Duration
}

func (r hostRuntime) Preflight(_ context.Context, manifest releasebundle.Manifest, releaseRoot string) error {
	if manifest.Target != "linux/amd64" {
		return fmt.Errorf("unsupported release target %q", manifest.Target)
	}
	for _, path := range []string{
		filepath.Join(releaseRoot, "bin", "migrate"),
		filepath.Join(releaseRoot, "systemd"),
		"/etc/systemd/system",
		"/run/faas",
		"/srv/fc/base",
		"/dev/shm",
	} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("required path %s: %w", path, err)
		}
	}
	return nil
}

func (r hostRuntime) Migrate(ctx context.Context, manifest releasebundle.Manifest, releaseRoot, _ string) error {
	migrate := filepath.Join(releaseRoot, "bin", "migrate")
	if _, err := os.Stat(migrate); err != nil {
		return fmt.Errorf("migration binary: %w", err)
	}
	command := fmt.Sprintf("DATABASE_URL=%q %q", r.databaseURL, migrate)
	return runCommand(ctx, "su", "-", "faas", "-s", "/bin/bash", "-c", command)
}

func (r hostRuntime) Activate(ctx context.Context, releaseRoot string) error {
	if err := cleanupBaseScratch("/dev/shm/faas-base-staging"); err != nil {
		return fmt.Errorf("cleanup base scratch: %w", err)
	}
	units := filepath.Join(releaseRoot, "systemd")
	if err := filepath.WalkDir(units, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(units, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return installAtomic(path, filepath.Join(r.unitDir, rel), info.Mode().Perm())
	}); err != nil {
		return fmt.Errorf("install units: %w", err)
	}
	tmpfiles := filepath.Join(releaseRoot, "tmpfiles.d", "faas.conf")
	if info, err := os.Stat(tmpfiles); err == nil && info.Mode().IsRegular() {
		if err := installAtomic(tmpfiles, "/etc/tmpfiles.d/faas.conf", info.Mode().Perm()); err != nil {
			return fmt.Errorf("install tmpfiles rule: %w", err)
		}
		if err := runCommand(ctx, "systemd-tmpfiles", "--create", "/etc/tmpfiles.d/faas.conf"); err != nil {
			return err
		}
	}
	if err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for _, service := range daemonunitspec.ActivationOrder() {
		if err := runCommand(ctx, "systemctl", "enable", "faas-"+service+".service"); err != nil {
			return err
		}
	}
	return nil
}

func cleanupBaseScratch(root string) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "faas-base-mkfs-") || !strings.HasSuffix(entry.Name(), ".ext4") {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (r hostRuntime) Restart(ctx context.Context, _ releasebundle.Manifest) error {
	for _, service := range r.serviceOrder {
		if err := runCommand(ctx, "systemctl", "reset-failed", "faas-"+service+".service"); err != nil {
			return err
		}
		if err := r.ensureRunFaasOwnership(ctx); err != nil {
			return err
		}
		if err := runCommand(ctx, "systemctl", "restart", "faas-"+service+".service"); err != nil {
			return err
		}
		if err := r.waitReady(ctx, service); err != nil {
			return err
		}
	}
	return nil
}

// ensureRunFaasOwnership pins /run/faas to root:faas 0775 before each
// service restart. The directory is owned by faas-vmmd's systemd
// RuntimeDirectory=faas, which re-creates it as root:root 0755 on every
// vmmd start (before ExecStartPre runs) and would otherwise leave the
// other daemons (schedd, apid, ...) unable to bind their sockets after
// a restart that recycles vmmd. Mirrors the cd-controlplane pre-restart
// chown (PR-M.2) that closes the same race during the workflow deploy.
func (r hostRuntime) ensureRunFaasOwnership(ctx context.Context) error {
	if err := runCommand(ctx, "chown", "root:faas", "/run/faas"); err != nil {
		return err
	}
	return runCommand(ctx, "chmod", "0775", "/run/faas")
}

func (r hostRuntime) Healthy(ctx context.Context, _ releasebundle.Manifest) error {
	if err := r.waitHTTP(ctx, "http://127.0.0.1:9090/healthz"); err != nil {
		return err
	}
	return nil
}

func (r hostRuntime) waitReady(ctx context.Context, service string) error {
	for _, entry := range daemonunitspec.Registry {
		if entry.Name != service {
			continue
		}
		switch entry.Lifecycle.Probe {
		case daemonunitspec.ProbeUnix:
			return waitPath(ctx, entry.Lifecycle.ProbeTarget, r.readyTimeout)
		case daemonunitspec.ProbeTCP:
			return waitTCP(ctx, entry.Lifecycle.ProbeTarget, r.readyTimeout)
		case daemonunitspec.ProbeSystemd:
			return waitSystemdActive(ctx, "faas-"+service+".service", r.readyTimeout)
		default:
			return fmt.Errorf("unknown readiness probe for %s", service)
		}
	}
	return fmt.Errorf("unknown service %q", service)
}

func (r hostRuntime) waitHTTP(ctx context.Context, address string) error {
	deadline := time.Now().Add(r.readyTimeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err == nil {
			response, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		if err := sleepReady(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("health check timed out: %s", address)
}

func waitPath(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode().Type()&os.ModeSocket != 0 {
			return nil
		}
		if err := sleepReady(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("readiness path timed out: %s", path)
}

func waitTCP(ctx context.Context, address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		if err := sleepReady(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("readiness TCP address timed out: %s", address)
}

func waitSystemdActive(ctx context.Context, service string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := runCommand(ctx, "systemctl", "is-active", "--quiet", service); err == nil {
			return nil
		}
		if err := sleepReady(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("systemd readiness timed out: %s", service)
}

func sleepReady(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func installAtomic(source, destination string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".deploy-unit-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(mode.Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, destination)
}

func defaultHostRuntime() hostRuntime {
	return hostRuntime{
		unitDir:      "/etc/systemd/system",
		databaseURL:  "postgres:///faas?host=/run/postgresql&user=faas",
		serviceOrder: daemonunitspec.ActivationOrder(),
		readyTimeout: 60 * time.Second,
	}
}
