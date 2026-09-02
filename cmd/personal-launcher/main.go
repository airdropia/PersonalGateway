// Package main implements the personal-launcher, a small Windows GUI
// helper that starts personal-gateway.exe as a hidden child process,
// waits for it to report ready, opens the dashboard in the default
// browser, and forwards shutdown signals.
//
// The launcher is intentionally minimal: it does not own a system tray
// (per plan §11.5 the tray integration can be added later through a
// companion PowerShell script), it does not modify the user's
// filesystem, and it does not require any third-party GUI dependency.
// Cross-compiled with -H=windowsgui -ldflags="-s -w" so the binary does
// not open its own console window.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

const (
	// probeInterval is how often the launcher polls the gateway health
	// endpoint after starting the child.
	probeInterval = 250 * time.Millisecond
	// probeTimeout is the upper bound on how long the launcher waits for
	// the gateway to become ready before giving up.
	probeTimeout = 30 * time.Second
)

// gatewayExeName is the gateway binary the launcher starts. The
// launcher expects the exe to live in the same directory.
const gatewayExeName = "personal-gateway.exe"

// findGatewayExe returns the path to the gateway binary. The launcher
// is built with go build -o personal-launcher.exe so the two binaries
// share a directory in the release payload. Falls back to the current
// working directory for development.
func findGatewayExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate launcher executable: %w", err)
	}
	dir := filepath.Dir(exe)
	candidate := filepath.Join(dir, gatewayExeName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	// Fall back to the working directory: lets developers run the
	// launcher from the repo root during smoke tests.
	if cwd, err := os.Getwd(); err == nil {
		alt := filepath.Join(cwd, gatewayExeName)
		if _, err := os.Stat(alt); err == nil {
			return alt, nil
		}
	}
	return "", fmt.Errorf("%s not found next to %s and not in the working directory", gatewayExeName, exe)
}

// startGateway launches the gateway as a child process. On Windows the
// child runs without its own console window so the launcher's hidden
// process tree stays invisible. On other platforms (Linux/macOS) the
// child inherits the launcher's stdio so the operator can still see the
// gateway logs in the terminal where they started the launcher.
func startGateway(ctx context.Context, exe string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, exe)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if runtime.GOOS == "windows" {
		// CREATE_NO_WINDOW = 0x08000000. Combined with the windowsgui
		// build mode, this keeps the launcher's own console invisible
		// and prevents the gateway from spawning a sibling console
		// either. On Windows the gateway's diagnostics land in the
		// user's data dir, not on a console.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow: true,
			CreationFlags: 0x08000000,
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", gatewayExeName, err)
	}
	return cmd, nil
}

// waitForReady polls the gateway health endpoint until it returns 200
// or the probe timeout elapses. The gateway's --ready flag makes this
// reliable; --health is a liveness probe that returns 200 as soon as
// the HTTP server is up.
func waitForReady(ctx context.Context, addr string) error {
	deadline := time.Now().Add(probeTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return errors.New("gateway did not become ready within the probe timeout")
		}
		ready, err := probeOnce(client, addr)
		if err == nil && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(probeInterval):
		}
	}
}

// probeOnce hits /health/ready and returns true on HTTP 200. We try
// /health/ready first because it covers startup, then fall back to
// /health which is the upstream liveness probe.
func probeOnce(client *http.Client, addr string) (bool, error) {
	for _, path := range []string{"/health/ready", "/health"} {
		resp, err := client.Get(addr + path)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true, nil
		}
	}
	return false, nil
}

// openBrowser launches the default browser to the dashboard URL using
// the platform's native mechanism. We deliberately do not import a
// browser library; the OS already knows the operator's preference.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		// rundll32 is the documented no-deps way to invoke the default
		// browser on Windows. URL is the file parameter; it does not
		// need to be a real file.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// listenForShutdown watches the parent process for SIGINT / Ctrl+C and
// returns when one arrives. The child gateway is killed by the cancel
// function propagated to startGateway through CommandContext.
func listenForShutdown(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-sigCh:
		}
		out <- struct{}{}
	}()
	return out
}

func main() {
	exe, err := findGatewayExe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "personal-launcher: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := startGateway(ctx, exe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "personal-launcher: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("personal-launcher: started %s (pid=%d)\n", gatewayExeName, cmd.Process.Pid)

	// The gateway binds to 127.0.0.1:8080 by default. We do not read
	// the configured port here because the launcher's only job is to
	// open the browser once the gateway is up; the gateway itself
	// logs the actual URL on startup and the dashboard redirect from
	// the admin root resolves the host.
	addr := "http://127.0.0.1:8080"
	if err := waitForReady(ctx, addr); err != nil {
		fmt.Fprintf(os.Stderr, "personal-launcher: %v\n", err)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		os.Exit(1)
	}

	fmt.Printf("personal-launcher: gateway ready at %s\n", addr)
	if err := openBrowser(addr + "/admin/dashboard"); err != nil {
		fmt.Fprintf(os.Stderr, "personal-launcher: open browser: %v\n", err)
	}

	<-listenForShutdown(ctx)
	fmt.Println("personal-launcher: shutting down")

	// Cancel propagates to the child via CommandContext, which sends
	// SIGKILL on Unix and TerminateProcess on Windows.
	cancel()
	if err := cmd.Wait(); err != nil {
		// Exit code != 0 from a child we asked to terminate is not an
		// error the operator needs to see.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "personal-launcher: gateway exit: %v\n", err)
		}
	}
}
