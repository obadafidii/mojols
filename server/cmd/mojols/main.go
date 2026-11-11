// Command mojols is a robust language server for Mojo. It supervises the
// official mojo-lsp-server and adds workspace-wide features on top of it.
//
// Usage:
//
//	mojols [serve] [flags] [-- extra mojo-lsp-server args]   speak LSP on stdio (default)
//	mojols doctor [dir]                                      show which mojo-lsp-server would be used
//	mojols stdlib [dir]                                      download the stdlib sources for the installed Mojo now
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
	"time"

	"github.com/obadafidii/mojols/server/internal/discover"
	"github.com/obadafidii/mojols/server/internal/proxy"
	pkgroots "github.com/obadafidii/mojols/server/internal/roots"
	"github.com/obadafidii/mojols/server/internal/stdlib"
	"github.com/obadafidii/mojols/server/internal/trace"
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
	case "stdlib":
		os.Exit(fetchStdlib(args))
	case "version":
		fmt.Println("mojols", fullVersion())
	case "debug-parse": // undocumented: mojols' own parse as JSON
		os.Exit(debugParse(args))
	case "debug-inspect": // undocumented: validate inspections on files
		os.Exit(debugInspect(os.Args[2:]))
	case "debug-organize": // undocumented: organize imports in files
		os.Exit(debugOrganize(args))
	case "debug-resolve": // undocumented: what each identifier resolves to
		os.Exit(debugResolve(args))
	default:
		fmt.Fprintf(os.Stderr, "mojols: unknown command %q (want serve, doctor, stdlib or version)\n", cmd)
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
	inferRoots := fs.Bool("infer-roots", true, "find package roots in the workspace and pass them as -I")
	refresh := fs.Bool("refresh-dependents", true, "re-check open files when a file they import is saved or changes on disk")
	maxResident := fs.Int("max-resident-docs", 20, "documents kept open in mojo-lsp-server at once (~31 MB each); others reopen when touched; -1 = no cap")
	memLimit := fs.Int("memory-limit-mb", 3072, "recycle mojo-lsp-server when its resident memory passes this; -1 = never")
	settle := fs.Bool("settle-requests", false, "hold a request on a file until mojo-lsp-server has parsed its latest edit (works around a crash in the server)")
	useIndex := fs.Bool("index", true, "answer definition, references, rename and workspace symbols from mojols' own workspace index (works across files and while mojo-lsp-server is down)")
	stdlibSrc := fs.String("stdlib-sources", "auto", "where to find the stdlib's sources for go to definition into std: auto (matching the installed Mojo, downloaded once from GitHub into the user cache), off, or a directory containing std/")
	verifyRefs := fs.Bool("verify", true, "confirm references the index cannot resolve on its own with a separate mojo-lsp-server, started on demand and stopped when idle")
	inspect := fs.Bool("inspections", true, "report unused imports and offer quick fixes (add a missing import, remove an unused one, discard an unused variable)")
	isolate := fs.Bool("isolate-completion", true, "run completion and signature help in a second mojo-lsp-server, so its crashes never reach the main one")
	crashDir := fs.String("crash-dir", "", "save a replayable bundle (requests in flight, document texts) for every mojo-lsp-server crash here")
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

	if *crashDir != "" {
		if abs, err := filepath.Abs(*crashDir); err == nil {
			*crashDir = abs
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

	// Same reason as --backend: the backend's working directory is the
	// workspace, not ours.
	explicit := make([]string, 0, len(includes))
	for _, dir := range includes {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		explicit = append(explicit, dir)
	}

	srv := proxy.New(os.Stdin, os.Stdout, proxy.Options{
		Logger:            logger,
		Tracer:            tracer,
		Version:           fullVersion(),
		NoRefresh:         !*refresh,
		MaxResidentDocs:   *maxResident,
		MemoryLimitMB:     *memLimit,
		SettleRequests:    *settle,
		CrashDir:          *crashDir,
		IsolateCompletion: *isolate,
		NoIndex:           !*useIndex,
		NoVerify:          !*verifyRefs,
		NoInspect:         !*inspect,
		StdlibSources:     *stdlibSrc,
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
			// Inferred again before every start, so a package created while
			// the editor is open is picked up on the next restart.
			var inferred []string
			if *inferRoots {
				r := pkgroots.Infer(roots, pkgroots.Options{})
				inferred = r.Roots
				logger.Info("inferred package roots", "roots", r.Roots, "visited", r.Visited, "fixtures", r.Fixtures, "elapsed", r.Elapsed.Round(time.Millisecond), "limit", r.Limit)
				for _, sk := range r.Skipped {
					logger.Warn("package root skipped", "dir", sk.Dir, "reason", sk.Reason)
				}
			}
			dirs := includeDirs(explicit, inferred)
			return proxy.Command{Path: res.Path, Args: backendArgs(dirs, extra), IncludeDirs: dirs, Dir: dir}, nil
		},
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("mojols starting", "version", fullVersion(), "pid", os.Getpid())
	return srv.Run(ctx)
}

// includeDirs puts explicit include dirs first (they win), then inferred
// ones not already given.
func includeDirs(explicit, inferred []string) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, list := range [][]string{explicit, inferred} {
		for _, dir := range list {
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

// backendArgs turns include dirs into -I flags, then adds raw extra arguments.
func backendArgs(dirs, extra []string) []string {
	args := []string{}
	for _, dir := range dirs {
		args = append(args, "-I", dir)
	}
	return append(args, extra...)
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

	r := pkgroots.Infer([]string{abs}, pkgroots.Options{})
	fmt.Printf("\npackage roots (passed as -I; scanned %d entries in %v, %d test-fixture dirs not scanned):\n", r.Visited, r.Elapsed.Round(time.Millisecond), r.Fixtures)
	if len(r.Roots) == 0 {
		fmt.Println("  none found: imports resolve only against the toolchain and each file's own directory")
	}
	for _, root := range r.Roots {
		fmt.Println("  -I", root)
	}
	for _, sk := range r.Skipped {
		fmt.Printf("  skipped %s: %s\n", sk.Dir, sk.Reason)
	}
	switch r.Limit {
	case "roots":
		fmt.Println("  (kept the first 32 roots, shallowest first; pass any others you need with -I)")
	case "entries", "time":
		fmt.Printf("  (scan stopped at its %s limit; some roots may be missing, pass them with -I)\n", r.Limit)
	}
	fmt.Println()
	if dir := stdlib.InWorkspace([]string{abs}); dir != "" {
		fmt.Printf("stdlib sources: the workspace's own checkout, %s\n", dir)
	} else if err == nil {
		v, verr := stdlib.DetectVersion(context.Background(), res.Path)
		switch {
		case verr != nil:
			fmt.Printf("stdlib sources: %v\n", verr)
		default:
			fmt.Printf("stdlib sources for Mojo %s: ", v.Number)
			if dir := stdlib.Cached(v); dir != "" {
				fmt.Printf("cached in %s\n", dir)
			} else {
				fmt.Println("not downloaded yet (mojols fetches them from GitHub on first use; --stdlib-sources=off to opt out)")
			}
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

// fetchStdlib downloads (or finds cached) the stdlib sources for the Mojo
// that serves the given workspace, and prints where they are.
func fetchStdlib(args []string) int {
	fs := flag.NewFlagSet("stdlib", flag.ContinueOnError)
	backend := fs.String("backend", "", "explicit mojo-lsp-server path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	abs, _ := filepath.Abs(dir)
	res, err := discover.Find(*backend, []string{abs}, discover.OSEnv())
	if err != nil {
		fmt.Fprintln(os.Stderr, "mojols:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	v, err := stdlib.DetectVersion(ctx, res.Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mojols:", err)
		return 1
	}
	fmt.Printf("Mojo %s (%s)\n", v.Number, res.Path)
	start := time.Now()
	root, err := stdlib.Ensure(ctx, v, stdlib.Options{Token: os.Getenv("GITHUB_TOKEN")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mojols:", err)
		return 1
	}
	fmt.Printf("stdlib sources: %s (%v)\n", root, time.Since(start).Round(time.Millisecond))
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
