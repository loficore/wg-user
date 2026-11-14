// Command wg-user runs the userspace WireGuard tunnel, either over the
// kernel's UDP sockets ([Transport] Mode = off) or through a wstunnel
// WebSocket (Mode = wstunnel), and keeps the run/state file the shell CLI and
// WebUI read.
//
// Subcommands:
//
//	wg-user [run] [-c FILE] [-v]
//	wg-user configtest [-c FILE]
//	wg-user version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/loficore/wg-user/internal/config"
	"github.com/loficore/wg-user/internal/device"
	"github.com/loficore/wg-user/internal/state"
	"github.com/loficore/wg-user/internal/wgbind"
	"github.com/loficore/wg-user/internal/wstunnel"
	"golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
)

// version is the build version, overridden at link time with
// -ldflags "-X main.version=$(VERSION)".
var version = "dev"

// Exit codes. A config or usage problem is distinct from a runtime failure so
// the shell CLI can tell "your file is wrong" from "the tunnel died".
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `usage:
  wg-user [run] [-c FILE] [-v]
  wg-user configtest [-c FILE]
  wg-user version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// realMain dispatches one wg-user invocation and returns its exit code. All
// human output goes to stdout or stderr so tests can capture it.
func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// No argument, or a leading flag, means the default subcommand: run.
	subcommand := "run"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcommand = args[0]
		rest = args[1:]
	}

	switch subcommand {
	case "run":
		return run(ctx, rest, stderr)
	case "configtest":
		return configtest(rest, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "wg-user %s\n", version)
		return exitOK
	default:
		fmt.Fprintf(stderr, "wg-user: unknown subcommand %q\n%s", subcommand, usage)
		return exitUsage
	}
}

// options are the flags the subcommands accept. run adds -v.
type options struct {
	configPath string
	verbose    bool
}

// parseOptions parses the flags of subcommand name. verbose is accepted only
// when withVerbose is true. A parse failure or a stray positional argument is
// a usage error; the caller returns exitUsage.
func parseOptions(name string, args []string, withVerbose bool, stderr io.Writer) (options, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	// The flag package's own messages lack the wg-user prefix, so they are
	// suppressed and re-printed below in the same shape as every other error.
	fs.SetOutput(io.Discard)

	opts := options{configPath: defaultConfigPath()}
	fs.StringVar(&opts.configPath, "c", opts.configPath, "path to the configuration file")
	if withVerbose {
		fs.BoolVar(&opts.verbose, "v", false, "enable verbose wireguard-go logging")
	}

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "wg-user: %v\n%s", err, usage)
		return options{}, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "wg-user: unexpected argument %q\n%s", fs.Arg(0), usage)
		return options{}, false
	}
	return opts, true
}

// defaultConfigPath is <exe>/../etc/wireproxy.conf, relative to the installed
// binary.
func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join("etc", "wireproxy.conf")
	}
	return filepath.Join(filepath.Dir(exe), "..", "etc", "wireproxy.conf")
}

// configtest loads the configuration and reports whether it is valid without
// touching the network or the state file.
func configtest(args []string, stdout, stderr io.Writer) int {
	opts, ok := parseOptions("configtest", args, false, stderr)
	if !ok {
		return exitUsage
	}

	if _, err := config.Load(opts.configPath); err != nil {
		// config.Load prefixes its errors with "config <path>:".
		fmt.Fprintf(stderr, "wg-user: %v\n", err)
		return exitUsage
	}

	fmt.Fprintln(stdout, "Config OK")
	return exitOK
}

// run brings the device up, serves cfg.WireProxy.Routines and blocks until ctx
// is cancelled or the transport fails permanently.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	opts, ok := parseOptions("run", args, true, stderr)
	if !ok {
		return exitUsage
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(stderr, "wg-user: %v\n", err)
		return exitUsage
	}

	logLevel := wgdevice.LogLevelError
	if opts.verbose {
		logLevel = wgdevice.LogLevelVerbose
	}

	// Pick the transport before the device comes up: wireguard-go takes the
	// conn.Bind at construction time. off mode is the kernel's UDP bind and
	// never changes; wstunnel mode is one Client whose State drives the state
	// file.
	transport := cfg.Transport.Mode
	detail := "udp"
	transportState := "connected"
	var client *wstunnel.Client
	var bind conn.Bind

	if transport == config.ModeWstunnel {
		// config.Load guarantees exactly one [Peer] with a non-nil Endpoint.
		client = wstunnel.New(wstunnel.Config{
			URL:       cfg.Transport.URL,
			Path:      cfg.Transport.Path,
			Target:    *cfg.WireProxy.Device.Peers[0].Endpoint,
			Username:  cfg.Transport.Username,
			Password:  cfg.Transport.Password,
			Insecure:  cfg.Transport.Insecure,
			Keepalive: cfg.Transport.Keepalive,
		})
		bind = wgbind.New(client)
		detail = cfg.Transport.URL
		transportState = client.State()
	} else {
		bind = conn.NewDefaultBind()
	}

	vt, err := device.Create(cfg.WireProxy, bind, logLevel)
	if err != nil {
		if client != nil {
			client.Close()
		}
		fmt.Fprintf(stderr, "wg-user: %v\n", err)
		return exitError
	}

	for _, routine := range cfg.WireProxy.Routines {
		go routine.SpawnRoutine(vt)
	}

	// The supervisor gets a context of its own: cancelling ctx must not make
	// Run close the transport while wireguard-go is still reading from it. The
	// teardown below closes the device first, then stops this context.
	clientCtx, stopClient := context.WithCancel(ctx)
	defer stopClient()

	var (
		runErrCh    chan error
		runReturned bool
	)
	if client != nil {
		runErrCh = make(chan error, 1)
		go func() { runErrCh <- client.Run(clientCtx) }()
		// Run may already have published a connection; report that instead of
		// the state the Client was constructed with.
		transportState = client.State()
	}

	// The daemon can be started before the shell has created run/.
	statePath := statePathFor(cfg.Path)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		fmt.Fprintf(stderr, "wg-user: state: %v\n", err)
	} else if err := writeState(statePath, transport, transportState, detail); err != nil {
		fmt.Fprintf(stderr, "wg-user: %v\n", err)
	}

	// The state file caches client.State: rewrite it only on a real change,
	// once a second, so shell readers see reconnects without the daemon
	// touching the file every tick.
	stopPoll := make(chan struct{})
	var pollDone chan struct{}
	if client != nil {
		pollDone = make(chan struct{})
		go func() {
			defer close(pollDone)

			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			last := transportState
			for {
				select {
				case <-ctx.Done():
					return
				case <-stopPoll:
					return
				case <-ticker.C:
					current := client.State()
					if current == last {
						continue
					}
					last = current
					if err := writeState(statePath, transport, current, detail); err != nil {
						fmt.Fprintf(stderr, "wg-user: %v\n", err)
					}
				}
			}
		}()
	}

	var runErr error
	if client != nil {
		select {
		case <-ctx.Done():
		case runErr = <-runErrCh:
			runReturned = true
			// A permanent failure (for example a TLS certificate error) must
			// not look like a clean signal exit.
			if runErr != nil {
				fmt.Fprintf(stderr, "wg-user: %v\n", runErr)
			}
		}
	} else {
		<-ctx.Done()
	}

	// Shutdown order is load-bearing: Dev.Close ends wireguard-go's receive
	// routines and the bind's generation before the transport they read from
	// goes away. Closing the client first would kill the receive path while
	// the device still depends on it.
	vt.Dev.Close()

	if client != nil {
		stopClient()
		client.Close()
		if !runReturned {
			// Both cancellation and Close end Run; wait for it so the state
			// poller and the file removal below cannot race it.
			if err := <-runErrCh; err != nil && runErr == nil {
				fmt.Fprintf(stderr, "wg-user: %v\n", err)
				runErr = err
			}
		}
	}

	close(stopPoll)
	if pollDone != nil {
		<-pollDone
	}

	if err := os.Remove(statePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "wg-user: %v\n", err)
	}

	if runErr != nil {
		return exitError
	}
	return exitOK
}

// statePathFor returns <config dir>/../run/state, the file the shell CLI and
// WebUI read. The path is cleaned so the parent hop does not leak into it.
func statePathFor(configPath string) string {
	return filepath.Clean(filepath.Join(filepath.Dir(configPath), "..", "run", "state"))
}

// writeState replaces the state file with one snapshot of the daemon.
func writeState(path, transport, transportState, detail string) error {
	return state.Write(path, state.State{
		PID:             os.Getpid(),
		Transport:       transport,
		TransportState:  transportState,
		TransportDetail: detail,
		Updated:         time.Now(),
	})
}
