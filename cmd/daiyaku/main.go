package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"daiyaku/internal/adapter"
	_ "daiyaku/internal/adapter/anthropic"
	_ "daiyaku/internal/adapter/openai"
	"daiyaku/internal/console"
	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/sequence"
	"daiyaku/internal/server"
)

// Overridable at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

func main() {
	server.SetVersion(version)
	args := os.Args[1:]
	if len(args) == 0 {
		serveOrExit(profile{}, nil)
		return
	}
	switch args[0] {
	case "serve":
		serveOrExit(profile{}, args[1:])
	case "env":
		runEnv(args[1:])
	case "intercept":
		if err := runIntercept(args[1:]); err != nil {
			if err != flag.ErrHelp {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			os.Exit(1)
		}
	case "report":
		if err := runReport(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "providers":
		printProviders()
	case "version", "-v", "--version":
		fmt.Printf("daiyaku %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		runDefault(args)
	}
}

func printProviders() {
	ps := adapter.Providers()
	sort.Strings(ps) // registry is a map; unsorted output changes run to run
	for _, p := range ps {
		fmt.Println(p)
	}
}

// runDefault handles a first argument that is not a known subcommand: a launch
// profile name, bare serve flags (leading '-'), or an unknown command.
func runDefault(args []string) {
	if prof, ok := profiles[args[0]]; ok {
		serveOrExit(prof, args[1:])
		return
	}
	if strings.HasPrefix(args[0], "-") {
		serveOrExit(profile{}, args)
		return
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
	usage()
	os.Exit(2)
}

// A one-word launch preset: provider + conventional port, so codex (8790) and
// claude (8787) can run at once. A preset only seeds defaults, so precedence
// stays flag > env > profile > built-in default: a DAIYAKU_ADDR you exported for
// the session is not silently overruled by typing "daiyaku codex".
type profile struct {
	provider string
	addr     string
}

var profiles = map[string]profile{
	"codex":     {provider: "openai", addr: "8790"},
	"openai":    {provider: "openai", addr: "8790"},
	"claude":    {provider: "anthropic", addr: "8787"},
	"anthropic": {provider: "anthropic", addr: "8787"},
}

func serveOrExit(prof profile, args []string) {
	if err := runServe(prof, args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`daiyaku ` + version + ` - operator-in-the-loop harness testing

USAGE:
  daiyaku [serve] [flags]    run the mock server + console (default command)
  daiyaku <profile> [flags]  serve with a preset: codex | claude
  daiyaku intercept [flags]  answer at the vendor's own address; the harness gets no config
  daiyaku env [flags]        print harness-redirection setup and revert commands
  daiyaku report <run-dir>   summarize a session + emit a replayable sequence
  daiyaku providers          list available provider adapters
  daiyaku version

COMMON:
  daiyaku                    REPL, anthropic, 127.0.0.1:8787
  daiyaku codex              REPL for Codex/OpenAI on 127.0.0.1:8790
  daiyaku -m tui             full-screen TUI console

Short flags:   -p provider, -a addr (host:port, :port, or bare port), -m mode
Profiles:      codex, claude (preset provider + conventional port)
Env defaults:  DAIYAKU_PROVIDER, DAIYAKU_ADDR, DAIYAKU_MODE
               (set one to make it your default, then just run 'daiyaku')
Precedence:    flag > env > profile > built-in default

Run 'daiyaku -h' for all serve flags.
`)
}

type flags struct {
	provider   string
	addr       string
	mode       string
	seqFile    string
	record     string
	delay      time.Duration
	fallback   bool
	runsDir    string
	upstream   string
	classGrade int
}

func parseServeFlags(prof profile, args []string) (*flags, error) {
	f := &flags{}
	fs := newFlagSet("serve")

	registerServeFlags(fs, prof, f)

	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: daiyaku [serve] [flags]

Starts the mock inference server + operator console. 'serve' is the default
command, so 'daiyaku -m repl' == 'daiyaku serve -m repl'.

flags:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
examples:
  daiyaku                    REPL, anthropic, 127.0.0.1:8787
  daiyaku codex              REPL for Codex/OpenAI on 127.0.0.1:8790
  daiyaku -m tui             full-screen TUI console

profiles:   codex, claude preset provider + port.
env:        DAIYAKU_PROVIDER, DAIYAKU_ADDR, DAIYAKU_MODE seed defaults.
precedence: flag > env > profile > built-in default.
`)
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	var err error
	if f.addr, err = normalizeAddr(f.addr); err != nil {
		return nil, err
	}
	return f, nil
}

// runtimeParts is everything a run needs once the flags are settled: the
// evidence directory, the broker, and the mock. Both serve and intercept build
// it the same way, so the classifier wiring and the transcript layout cannot
// drift between the two.
type runtimeParts struct {
	srv        *server.Server
	eng        *engine.Engine
	tx         *server.Transcript
	sessionDir string
}

func setupRuntime(f *flags, extraNotes map[string]string) (*runtimeParts, error) {
	a, ok := adapter.New(f.provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (have: %s)", f.provider, strings.Join(adapter.Providers(), ", "))
	}
	sessionDir, err := server.NewSessionDir(f.runsDir, time.Now())
	if err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	tx, err := server.NewTranscript(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	note := map[string]string{
		"provider": f.provider, "addr": f.addr, "mode": f.mode, "version": version,
	}
	for k, v := range extraNotes {
		note[k] = v
	}
	tx.Note("session-start", note)

	eng := engine.New(0)
	if f.classGrade >= 0 {
		grade := f.classGrade
		eng.Auto = func(req *neutral.Request) (neutral.Action, bool) {
			if !req.IsSafetyClassifier() {
				return neutral.Action{}, false
			}
			return neutral.Action{
				Kind: neutral.ActionEnd,
				Text: fmt.Sprintf("<severity>%d</severity>", grade),
			}, true
		}
	}
	return &runtimeParts{
		srv: server.New(a, eng, tx, f.addr), eng: eng, tx: tx, sessionDir: sessionDir,
	}, nil
}

func runServe(prof profile, args []string) error {
	f, err := parseServeFlags(prof, args)
	if err != nil {
		return err
	}
	rt, err := setupRuntime(f, nil)
	if err != nil {
		return err
	}
	defer rt.tx.Close()
	srv, eng, sessionDir := rt.srv, rt.eng, rt.sessionDir

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if f.mode == "passthrough" {
		if f.upstream == "" {
			return fmt.Errorf("passthrough mode requires -upstream <base-url>")
		}
		// Nothing is authored in passthrough: the real model answers. Accepting
		// -record here would silently produce no file at all.
		if f.record != "" {
			return fmt.Errorf("-record has no meaning in passthrough mode: no actions are authored there. " +
				"The exchange is captured in the transcript; run 'daiyaku report <run-dir>' afterwards for a replayable sequence")
		}
		srv.SetProxy(server.NewProxy(f.upstream))
		return runPassthrough(ctx, cancel, srv, f, sessionDir)
	}

	return runConsoleLoop(ctx, cancel, srv, f, eng, sessionDir,
		func() { banner(f, sessionDir) }, nil)
}

// runConsoleLoop starts the mock and the operator console and blocks until one
// of them stops or the process is signalled. onReady prints whatever banner the
// caller wants once the server is up. cleanup always runs before returning,
// including on a forced second signal, because intercept mode has machine
// changes that must not outlive the process.
func runConsoleLoop(ctx context.Context, cancel context.CancelFunc, srv *server.Server,
	f *flags, eng *engine.Engine, sessionDir string, onReady, cleanup func()) error {

	con, err := buildConsole(f, eng, sessionDir)
	if err != nil {
		return err
	}

	var once sync.Once
	runCleanup := func() {
		if cleanup != nil {
			once.Do(cleanup)
		}
	}
	defer runCleanup()

	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		fmt.Fprintln(os.Stderr, "\nshutting down...")
		cancel()
		<-sigc       // a second signal forces exit even if a console is blocked on stdin
		runCleanup() // never leave the machine redirected, even on a forced exit
		os.Exit(1)
	}()

	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe(ctx) }()

	if onReady != nil {
		onReady()
	}

	conErr := make(chan error, 1)
	go func() { conErr <- con.Run(ctx) }()

	if err := waitForShutdown(ctx, cancel, srvErr, conErr); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond) // let the server drain before the process exits
	runCleanup()
	fmt.Printf("\nlog saved to %s\n", absPath(sessionDir))
	return nil
}

// waitForShutdown blocks until the context is cancelled or the server or console
// goroutine returns, cancelling the context on a console exit or a server error.
func waitForShutdown(ctx context.Context, cancel context.CancelFunc, srvErr, conErr <-chan error) error {
	select {
	case <-ctx.Done():
	case err := <-srvErr:
		if err != nil {
			cancel()
			return fmt.Errorf("server: %w", err)
		}
	case err := <-conErr:
		cancel()
		if err != nil {
			return fmt.Errorf("console: %w", err)
		}
	}
	return nil
}

func runPassthrough(ctx context.Context, cancel context.CancelFunc, srv *server.Server, f *flags, sessionDir string) error {
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		fmt.Fprintln(os.Stderr, "\nshutting down...")
		cancel()
		<-sigc // a second signal forces exit
		os.Exit(1)
	}()

	fmt.Printf("\n  daiyaku %s - PASSTHROUGH / CAPTURE\n", version)
	fmt.Printf("  listening: http://%s\n", f.addr)
	fmt.Printf("  upstream : %s\n", f.upstream)
	fmt.Printf("  log: %s\n", sessionDir)
	fmt.Printf("  proxying the real harness<->API and logging exact wire shapes.\n")
	fmt.Printf("  Ctrl+C to stop.\n\n")

	err := srv.ListenAndServe(ctx)
	fmt.Printf("\nlog saved to %s\n", absPath(sessionDir))
	return err
}

func absPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func buildConsole(f *flags, eng *engine.Engine, sessionDir string) (console.Console, error) {
	switch f.mode {
	case "repl":
		return console.NewREPL(eng, f.provider, sessionDir, f.record), nil
	case "tui":
		return console.NewTUI(eng, f.provider, sessionDir, f.record), nil
	case "canned":
		if f.seqFile == "" {
			return nil, fmt.Errorf("canned mode requires -sequence <file>")
		}
		sf, err := sequence.Load(f.seqFile)
		if err != nil {
			return nil, fmt.Errorf("load sequence: %w", err)
		}
		var fb *console.REPL
		if f.fallback {
			fb = console.NewREPL(eng, f.provider, sessionDir, f.record)
		}
		return console.NewCanned(eng, f.provider, sf, f.delay, f.record, fb), nil
	default:
		return nil, fmt.Errorf("unknown mode %q (tui, repl, canned)", f.mode)
	}
}

func banner(f *flags, sessionDir string) {
	fmt.Printf("\n  daiyaku %s\n", version)
	fmt.Printf("  provider : %s\n", f.provider)
	fmt.Printf("  listening: http://%s\n", f.addr)
	fmt.Printf("  mode     : %s\n", f.mode)
	if f.classGrade >= 0 {
		fmt.Printf("  classifier: auto-grade severity %d (safety classifier answered without you)\n", f.classGrade)
	} else {
		fmt.Printf("  classifier: manual (operator answers the safety classifier)\n")
	}
	fmt.Printf("  log: %s\n", sessionDir)
	if !isLoopback(f.addr) {
		fmt.Printf("  ! WARNING: %s is not loopback. The mock authenticates nothing and\n", f.addr)
		fmt.Printf("    serves the harness conversation to anyone who can reach this port.\n")
	}
	fmt.Printf("  run 'daiyaku env' for setup.\n")
}

// registerServeFlags declares the flags every serving mode shares, so intercept
// takes the same console, recording, and classifier options as serve without a
// second copy that can drift.
func registerServeFlags(fs *flag.FlagSet, prof profile, f *flags) {
	// flag > env > profile > built-in default.
	provider := firstSet(os.Getenv("DAIYAKU_PROVIDER"), prof.provider, "anthropic")
	addr := firstSet(os.Getenv("DAIYAKU_ADDR"), prof.addr, "127.0.0.1:8787")
	mode := envOr("DAIYAKU_MODE", "repl")

	fs.StringVar(&f.provider, "provider", provider, "provider adapter (anthropic, openai)")
	fs.StringVar(&f.provider, "p", provider, "alias for -provider")
	fs.StringVar(&f.addr, "addr", addr, "listen address: host:port, :port, or bare port")
	fs.StringVar(&f.addr, "a", addr, "alias for -addr")
	fs.StringVar(&f.mode, "mode", mode, "operator console: tui, repl, canned, passthrough")
	fs.StringVar(&f.mode, "m", mode, "alias for -mode")
	fs.StringVar(&f.seqFile, "sequence", "", "canned mode: path to a sequence JSON file")
	fs.StringVar(&f.seqFile, "s", "", "alias for -sequence")
	fs.StringVar(&f.record, "record", "", "append authored actions to this sequence file")
	fs.StringVar(&f.record, "r", "", "alias for -record")
	fs.DurationVar(&f.delay, "delay", 400*time.Millisecond, "canned mode: delay between steps")
	fs.BoolVar(&f.fallback, "fallback", true, "canned mode: hand the tail to the operator when the sequence runs out (false: end the turn and stop)")
	fs.StringVar(&f.runsDir, "runs-dir", "runs", "base directory for per-session evidence")
	fs.StringVar(&f.upstream, "upstream", "", "passthrough mode: real upstream base URL to proxy to (e.g. https://api.anthropic.com)")
	fs.StringVar(&f.upstream, "u", "", "alias for -upstream")
	fs.IntVar(&f.classGrade, "classifier-severity", 0, "canned grade for the harness safety-classifier side-call, 0-100 (50 is its allow/block line, so 0 allows and 80 blocks); -1 leaves the calls to the operator")
}
