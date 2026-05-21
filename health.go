package main

// File-based healthcheck for distroless containers.
//
// Docker's HEALTHCHECK needs a command inside the container. Distroless
// images have no curl/wget/shell, so the canonical approach is to
// re-invoke the app binary with a `health` subcommand that probes the
// application's liveness. This app uses a file marker at healthMarkerPath:
// the running process touches the file at lifecycle points, the probe
// process stats it.
//
// Failure modes:
//   - If the marker directory is not writable (typically compose declares
//     `read_only: true` without a `tmpfs: /tmp` mount), the constructor
//     logs one Warn with a fix hint and enters degraded mode. In degraded
//     mode the long-running process treats Set / Cleanup as no-ops. The
//     probe process independently detects the same condition and reports
//     healthy, because the container is alive and the only broken piece
//     is the signaling channel. Reporting unhealthy would trigger a
//     Docker restart loop that cannot fix a compose misconfiguration.
//   - Transient failures (full tmpfs, directory churn) during Set are
//     logged at Warn but do not change the marker's mode. They surface
//     at the next probe interval as an unhealthy signal.
//
// Thread-safe; Set may be called from any goroutine.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// healthMarkerPath is the default marker location. Docker healthchecks
// stat this path; the app creates and removes it at lifecycle points.
// /tmp is conventional because strict-tier compose services mount
// /tmp as tmpfs (see base.yaml -strict templates).
const healthMarkerPath = "/tmp/.healthy"

// healthMarker implements the file-based distroless healthcheck pattern.
// Use newHealthMarker to construct it; call Set(bool) at lifecycle points;
// defer Cleanup on shutdown; call runProbe from main when os.Args[1] is
// "health".
type healthMarker struct {
	path     string
	mu       sync.Mutex
	known    bool // true once Set has been called at least once
	healthy  bool // last value passed to Set
	degraded bool // true when marker dir is not writable
}

// newHealthMarker constructs a marker for path and probes the parent
// directory for writability. On failure it logs a single Warn with a
// fix hint and returns a marker in degraded mode; callers need not
// branch on the result.
func newHealthMarker(path string) *healthMarker {
	m := &healthMarker{path: path}
	if err := probeHealthDir(path); err != nil {
		m.degraded = true
		slog.Warn("health marker directory not writable, "+
			"container will report healthy in degraded mode",
			"dir", filepath.Dir(path),
			"error", err,
			"hint", "compose.yaml with read_only:true requires "+
				"`tmpfs: [\"/tmp:size=1m,mode=1777,noexec,nosuid,nodev\"]`")
	}
	return m
}

// Set records the current liveness state and touches or removes the
// marker accordingly. Edge transitions (true↔false) are logged; repeated
// calls with the same value are silent. Safe to call from any goroutine.
// In degraded mode Set is a no-op.
func (m *healthMarker) Set(ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.degraded {
		return
	}

	changed := !m.known || m.healthy != ok
	m.known = true
	m.healthy = ok

	if ok {
		if err := writeMarker(m.path); err != nil {
			slog.Warn("failed to create health marker",
				"path", m.path, "error", err)
			return
		}
		if changed {
			slog.Info("health state changed", "healthy", true)
		}
		return
	}

	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("failed to remove health marker",
			"path", m.path, "error", err)
		return
	}
	if changed {
		slog.Warn("health state changed", "healthy", false)
	}
}

// Cleanup removes the marker. Typically called via defer at shutdown.
// In degraded mode Cleanup is a no-op.
func (m *healthMarker) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.degraded {
		return
	}
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("failed to remove health marker on cleanup",
			"path", m.path, "error", err)
	}
}

// runProbe runs in the separate `health` subcommand process. It exits
// 0 if the marker is present or the marker directory is unwritable
// (degraded mode: the long-running process cannot signal through the
// filesystem, so the probe falls back to "alive"). It exits 1 when
// the marker is absent from a writable directory, which is the real
// unhealthy signal.
func runProbe(path string) {
	code := probeCheck(path)
	if code != 0 {
		fmt.Fprintln(os.Stderr, "unhealthy: marker absent")
	}
	os.Exit(code)
}

// probeCheck implements the health-probe decision without calling
// os.Exit, so it can be unit-tested. Returns 0 for healthy or
// degraded, 1 for unhealthy.
func probeCheck(path string) int {
	if _, err := os.Stat(path); err == nil {
		return 0
	}
	if err := probeHealthDir(path); err != nil {
		return 0
	}
	return 1
}

// --- helpers ---

// writeMarker atomically touches the marker file. A fresh os.Create is
// sufficient; the file is empty and the healthcheck only tests existence.
func writeMarker(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if closeErr := f.Close(); closeErr != nil {
		return fmt.Errorf("close: %w", closeErr)
	}
	return nil
}

// probeHealthDir verifies the marker's parent directory is writable by
// creating and deleting a temp file. Returns the underlying error on
// failure so callers can log with context.
func probeHealthDir(path string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".health-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close probe: %w", closeErr)
	}
	if rmErr := os.Remove(name); rmErr != nil {
		return fmt.Errorf("remove probe: %w", rmErr)
	}
	return nil
}
