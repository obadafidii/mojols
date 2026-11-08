// Command mojols is a robust language server for Mojo. It supervises the
// official mojo-lsp-server and adds workspace-wide features on top of it.
//
// Usage:
//
//	mojols [serve] [flags] [-- extra mojo-lsp-server args]   speak LSP on stdio (default)
//	mojols doctor [dir]                                      show which mojo-lsp-server would be used
//	mojols version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/tdadadavid/mojols/server/internal/discover"
	"github.com/tdadadavid/mojols/server/internal/proxy"
	"github.com/tdadadavid/mojols/server/internal/trace"
)

// version is set at release time with -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		os.Exit(serve(args))
	case "doctor":
		os.Exit(doctor(args))
	case "version":
		fmt.Println("mojols", fullVersion())
	default:
		fmt.Fprintf(os.Stderr, "mojols: unknown command %q (want serve, doctor or version)\n", cmd)
		os.Exit(2)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	backend := fs.String("backend", "", "path to mojo-lsp-server (default: discover; see `mojols doctor`)")
	logFile := fs.String("log-file", "", "write logs here instead of stderr")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	traceFile := fs.String("trace-file", "", "record every LSP message as JSON lines (for bug reports and replay)")
	var includes multiFlag
	fs.Var(&includes, "I", "include directory passed to mojo-lsp-server (repeatable)")
	var extra []string
	for i, a := range args {
		if a == "--" {
			args, extra = args[:i], args[i+1:]
			break
		}
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The backend runs with the workspace as its working directory, so a
	// relative --backend must be made absolute against ours first.
	if strings.ContainsRune(*backend, filepath.Separator) && !filepath.IsAbs(*backend) {
		if abs, err := filepath.Abs(*backend); err == nil {
			*backend = abs
		}
	}

	logger, closeLog, err := newLogger(*logFile, *logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mojols:", err)
		return 2
	}
	defer closeLog()

	var tracer *trace.Tracer
	if *traceFile != "" {
		f, err := os.OpenFile(*traceFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			logger.Error("cannot open trace file", "err", err)
		} else {
			tracer = trace.New(f)
		}
	}

	backendArgs := []string{}
	for _, dir := range includes {
		backendArgs = append(backendArgs, "-I", dir)
	}
	backendArgs = append(backendArgs, extra...)

	srv := proxy.New(os.Stdin, os.Stdout, proxy.Options{
		Logger:  logger,
		Tracer:  tracer,
		Version: fullVersion(),
		Resolve: func(roots []string) (proxy.Command, error) {
			res, err := discover.Find(*backend, roots, discover.OSEnv())
			if err != nil {
				return proxy.Command{}, fmt.Errorf("%w\n%s", err, discover.Report(res, err))
			}
			logger.Info("using backend", "path", res.Path, "source", res.Source)
			dir := ""
			if len(roots) > 0 {
				dir = roots[0]
			}
			return proxy.Command{Path: res.Path, Args: backendArgs, Dir: dir}, nil
		},
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("mojols starting", "version", fullVersion(), "pid", os.Getpid())
	return srv.Run(ctx)
}

func newLogger(path, level string) (*slog.Logger, func(), error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, nil, fmt.Errorf("bad --log-level %q", level)
	}
	var w io.Writer = os.Stderr
	closer := func() {}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w, closer = f, func() { f.Close() }
	}
	// Never stdout: it carries the protocol.
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), closer, nil
}

func doctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	backend := fs.String("backend", "", "explicit mojo-lsp-server path to check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mojols:", err)
		return 2
	}
	fmt.Printf("mojols %s\nworkspace %s\n\n", fullVersion(), abs)
	res, err := discover.Find(*backend, []string{abs}, discover.OSEnv())
	fmt.Print(discover.Report(res, err))
	if err != nil {
		return 1
	}
	return 0
}

func fullVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev+" + s.Value[:7]
			}
		}
	}
	return version
}
