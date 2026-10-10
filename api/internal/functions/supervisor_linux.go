//go:build linux

package functions

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ChildProcess owns the entire cgroup even after the main Node process exits.
// A detached grandchild cannot keep running after Stop returns successfully.
type ChildProcess struct {
	command *exec.Cmd
	group   *ChildGroup
	done    chan struct{}
	mu      sync.Mutex
	stopped bool
}

func startChild(command *exec.Cmd, group *ChildGroup) (*ChildProcess, error) {
	if err := command.Start(); err != nil {
		return nil, errors.New("function child could not start")
	}
	child := &ChildProcess{command: command, group: group, done: make(chan struct{})}
	go func() { _ = command.Wait(); close(child.done) }()
	return child, nil
}

func (c *ChildProcess) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil
	}
	// Signal the dedicated child process group, never PID 1 or the manager.
	_ = syscall.Kill(-c.command.Process.Pid, syscall.SIGINT)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	if err := c.group.Kill(); err != nil {
		return errors.New("function process tree stop failed")
	}
	// Main process reaping and cgroup population are distinct obligations.
	select {
	case <-c.done:
	case <-time.After(time.Second):
		return errors.New("function main process did not stop")
	}
	for i := 0; i < 100; i++ {
		content, err := os.ReadFile(filepath.Join(c.group.path, "cgroup.events"))
		if err != nil {
			return errors.New("function process tree observation failed")
		}
		if stringsContainLine(string(content), "populated 0") {
			c.stopped = true
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("function process tree remains populated")
}

// cappedLog retains at most 1 MiB in a root-owned file. Customer output is not
// printed to platform stdout or counted as trusted control/health evidence.
type cappedLog struct {
	mu   sync.Mutex
	file *os.File
	left int
}

func (w *cappedLog) Write(content []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := len(content)
	if len(content) > w.left {
		content = content[:w.left]
	}
	if len(content) > 0 {
		n, err := w.file.Write(content)
		w.left -= n
		if err != nil {
			return 0, err
		}
	}
	return size, nil
}

func stringsContainLine(text, line string) bool {
	for _, value := range strings.Split(text, "\n") {
		if value == line {
			return true
		}
	}
	return false
}

// GuestBootError exposes only a fixed boot stage; bootstrap/private errors stay
// out of the platform journal.
type GuestBootError struct{ Stage string }

func (e *GuestBootError) Error() string { return "function guest stage failed: " + e.Stage }

// ServeGuest performs fail-closed privileged boot and then serves one immutable
// instance. It is not a Kubernetes-host helper or a shared Node execution pool.
func ServeGuest(ctx context.Context) (result error) {
	stage := "bootstrap"
	defer func() {
		if result != nil {
			result = &GuestBootError{Stage: stage}
		}
	}()
	if os.Geteuid() != 0 {
		return errors.New("function guest requires root supervisor")
	}
	marker, err := os.ReadFile("/etc/neon-function-guest")
	if err != nil || string(marker) != "isolated-neonvm-functions-v1\n" {
		return errors.New("function guest marker required")
	}
	file, err := os.Open("/run/function-bootstrap/config.json")
	if err != nil {
		return errors.New("function bootstrap unavailable")
	}
	content, readErr := io.ReadAll(io.LimitReader(file, MaxBootstrapBytes+1))
	_ = file.Close()
	if readErr != nil {
		return errors.New("function bootstrap could not be read")
	}
	b, err := DecodeBootstrap(content)
	if err != nil {
		return err
	}
	identity, err := b.ManagerTLS(time.Now())
	if err != nil {
		return err
	}
	// An irreversible instance lifecycle cannot be restarted in the same guest
	// after customer execution. Worker creates a fresh VM generation instead.
	if err = os.MkdirAll("/run/neon-function", 0700); err != nil {
		return errors.New("guest supervisor state unavailable")
	}
	pid, err := os.OpenFile("/run/neon-function/supervisor.pid", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("fresh function guest boot required")
	}
	_, err = pid.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_ = pid.Close()
	if err != nil {
		return errors.New("guest supervisor state persistence failed")
	}
	stage = "artifact"
	archive, err := b.FetchBundle(ctx)
	if err != nil {
		return err
	}
	stage = "bundle"
	if err = os.MkdirAll("/srv/artifacts", 0700); err != nil {
		return errors.New("guest bundle parent unavailable")
	}
	installed, err := InstallBundle(archive, "/srv/artifacts")
	if err != nil {
		return err
	}
	if err = os.Chmod("/srv/artifacts", 0755); err != nil {
		return errors.New("guest bundle parent sealing failed")
	}
	if err = os.Symlink("/srv/artifacts/bundle-"+installed.Digest, "/srv/function"); err != nil {
		return errors.New("fresh guest entry directory required")
	}
	if err = os.MkdirAll("/etc/neon-function", 0755); err != nil {
		return errors.New("guest SQL trust directory unavailable")
	}
	if err = os.WriteFile("/etc/neon-function/sql-ca.crt", []byte(b.SQLCA), 0444); err != nil {
		return errors.New("guest SQL trust persistence failed")
	}
	// DHCP cannot alter this explicit branch TLS destination or UID allowlist.
	if err = os.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost\n"+b.ProxyIP+" "+b.ProxyHostname+"\n"), 0644); err != nil {
		return errors.New("guest SQL hostname mapping failed")
	}
	stage = "boundary"
	if err = DetachBootstrap("/run/function-bootstrap"); err != nil {
		return err
	}
	if err = HideGuestBlockDevices(); err != nil {
		return err
	}
	if err = HardenGuestCgroups(); err != nil {
		return err
	}
	boundary := GuestBoundary{ProxyIP: netip.MustParseAddr(b.ProxyIP), DNSIP: netip.MustParseAddr(b.DNSIP), ProxyPort: b.ProxyPort, AllowPublicHTTPS: b.AllowPublicHTTPS}
	if err = boundary.Prepare(ctx); err != nil {
		return err
	}
	group, err := NewChildGroup()
	if err != nil {
		return err
	}
	defer group.Close()
	if err = os.MkdirAll("/tmp/function", 0700); err != nil || os.Chown("/tmp/function", 65532, 65532) != nil {
		return errors.New("guest child scratch directory unavailable")
	}
	if err = os.MkdirAll("/var/log/neon-function", 0700); err != nil {
		return errors.New("guest log directory unavailable")
	}
	output, err := os.OpenFile("/var/log/neon-function/child.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("fresh guest log required")
	}
	defer output.Close()
	stage = "child"
	command, err := group.Command(context.Background(), "/opt/neon/functions/runtime/boot.mjs", b.Environment, b.DatabaseURL)
	if err != nil {
		return err
	}
	command.Args = append(command.Args, "/srv/function/"+installed.Entry)
	writer := &cappedLog{file: output, left: 1 << 20}
	command.Stdout = writer
	command.Stderr = writer
	child, err := startChild(command, group)
	if err != nil {
		return err
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = child.Stop(stop)
	}()
	managerKey, _ := secretKey(b.ManagerKey)
	stopped := make(chan struct{}, 1)
	stop := func(ctx context.Context) error {
		if err := child.Stop(ctx); err != nil {
			return err
		}
		// NeonVM init must not respawn this root supervisor while Worker retires VM.
		if err := os.Remove("/neonvm/vmstart.allowed"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("guest respawn inhibition failed")
		}
		select {
		case stopped <- struct{}{}:
		default:
		}
		return nil
	}
	stage = "manager"
	manager, err := NewManager(b.Scope, managerKey, "http://127.0.0.1:8081", stop)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:9090")
	if err != nil {
		return errors.New("manager listener unavailable")
	}
	server := &http.Server{Handler: manager, TLSConfig: identity, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, identity)) }()
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := stop(stopCtx); err != nil {
			return err
		}
	case <-stopped:
	case <-done:
		return errors.New("manager server stopped unexpectedly")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		return errors.New("manager drain failed")
	}
	return nil
}
