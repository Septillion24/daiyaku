package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"daiyaku/internal/intercept"
)

// Default vendor endpoints per provider. Only the inference host is taken over:
// auth, telemetry, and update endpoints keep working normally, which is both
// more faithful and less disruptive than redirecting the whole domain.
var interceptHosts = map[string][]string{
	"anthropic": {"api.anthropic.com"},
	"openai":    {"api.openai.com"},
}

// probePath is an auxiliary endpoint the self-test can call: it is answered
// without going near the operator queue, so verifying the interception never
// costs a turn.
const probePath = "/v1/models"

// configDir is where the CA, hosts backups, and the intercept state record live.
// It is deliberately not the run directory: the CA outlives any single run, and
// the run directory is client evidence.
func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	return filepath.Join(base, "daiyaku"), nil
}

func runIntercept(args []string) error {
	fs := newFlagSet("intercept")
	f := &flags{}
	registerServeFlags(fs, profile{}, f)

	var (
		hostList   string
		port       int
		trustStore bool
		revert     bool
		check      bool
	)
	fs.StringVar(&hostList, "host", "", "comma-separated hostnames to answer for (default: the provider's API host)")
	fs.IntVar(&port, "port", 443, "https port to listen on")
	fs.BoolVar(&trustStore, "trust-store", false,
		"also install daiyaku's CA machine-wide, so the harness needs no environment variable at all")
	fs.BoolVar(&revert, "revert", false, "undo a previous intercept (hosts entries and, if it was used, the CA) and exit")
	fs.BoolVar(&check, "check", false, "report whether this machine is ready to intercept, change nothing, and exit")

	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: daiyaku intercept [flags]

Answers the harness at the vendor's own address instead of asking the harness to
point somewhere else. The harness is given no base URL and no token: it resolves
the API hostname to this machine and is handed a certificate for that name.

This is the faithful mode. A harness can change behaviour when it notices a
non-standard base URL, and the offered tool surface is exactly what Phase 1 is
measuring, so it is worth the extra setup on an engagement.

It redirects the vendor hostname for EVERY program on this machine, not only the
harness under test. Use a test VM, or expect your own tools to be redirected too.

  daiyaku intercept                 anthropic, 127.0.0.1:443, REPL console
  daiyaku intercept -p openai       intercept api.openai.com for Codex
  daiyaku intercept --check         is this machine ready? changes nothing
  daiyaku intercept --revert        put this machine back
  daiyaku intercept --trust-store   no environment variable needed at all

flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := configDir()
	if err != nil {
		return err
	}

	if revert {
		return runInterceptRevert(dir)
	}

	hosts := interceptHosts[f.provider]
	if hostList != "" {
		hosts = splitHosts(hostList)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no default host known for provider %q; name one with -host", f.provider)
	}
	f.addr = fmt.Sprintf("127.0.0.1:%d", port)

	in, err := intercept.New(intercept.Config{
		Dir: dir, Hosts: hosts, Addr: f.addr, TrustStore: trustStore,
	})
	if err != nil {
		return err
	}

	warnStale(dir)

	if problems := in.Preflight(); len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "\ncannot intercept yet. Nothing on this machine was changed.\n\n")
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		fmt.Fprintf(os.Stderr, "\nFix those, or use the base-URL mode instead (less faithful, no admin needed):\n    %s\n\n",
			baseURLHint(f.provider))
		return fmt.Errorf("preflight failed")
	}
	if check {
		fmt.Printf("\n  ready to intercept %s on %s\n", strings.Join(hosts, ", "), f.addr)
		fmt.Printf("  certificate: %s\n", in.CA.CertPath)
		fmt.Printf("  nothing was changed. Run without --check to start.\n\n")
		return nil
	}

	return interceptServe(f, in, hosts, dir, trustStore)
}

// interceptServe applies the machine changes, verifies them, and hands over to
// the normal console loop. Every path out of here restores the machine.
func interceptServe(f *flags, in *intercept.Interceptor, hosts []string, dir string, trustStore bool) error {
	rt, err := setupRuntime(f, map[string]string{
		"intercept":       strings.Join(hosts, ","),
		"intercept_ca":    in.CA.CertPath,
		"intercept_trust": fmt.Sprint(trustStore),
	})
	if err != nil {
		return err
	}
	defer rt.tx.Close()

	report, err := in.Apply()
	if err != nil {
		in.Revert()
		return err
	}
	cleanup := func() {
		if done := in.Revert(); len(done) > 0 {
			fmt.Printf("\nintercept reverted: %s\n", strings.Join(done, "; "))
		}
	}

	rt.srv.SetTLS(in.TLSConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// onReady runs synchronously inside runConsoleLoop, so this needs no lock.
	var selfTestErr error
	ready := func() {
		if err := verify(in, report); err != nil {
			selfTestErr = err
			// A half-working intercept is the exact confusion this mode exists to
			// avoid, so tear it down rather than leave the operator to discover it
			// through the harness.
			fmt.Fprintf(os.Stderr, "\n  ! self-test failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "%s\n", selfTestHelp())
			cleanup()
			cancel()
			return
		}
		interceptBanner(f, in, report, hosts, rt.sessionDir, trustStore)
	}

	err = runConsoleLoop(ctx, cancel, rt.srv, f, rt.eng, rt.sessionDir, ready, cleanup)
	if selfTestErr != nil {
		// Exit non-zero so a script driving this can tell the difference between a
		// finished run and one that never intercepted anything.
		return fmt.Errorf("intercept self-test failed: %w", selfTestErr)
	}
	return err
}

// verify retries the self-test briefly: a resolver that cached the old answer
// just before the hosts file changed is the one failure that fixes itself, and
// retrying beats telling the operator to try again.
func verify(in *intercept.Interceptor, report *intercept.Report) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = in.SelfTest(probePath); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
	}
	return err
}

func selfTestHelp() string {
	return `
  The machine was put back, so nothing is left redirected. Common causes:

    - a resolver still holding the old address: wait a moment and retry
    - a VPN or corporate DNS client answering ahead of the hosts file
    - security software reverting hosts file edits
    - something else already listening on the port

  Check what the name resolves to now:
    ` + resolveHint()
}

func resolveHint() string {
	if runtime.GOOS == "windows" {
		return "Resolve-DnsName api.anthropic.com   (or: ping api.anthropic.com)"
	}
	return "getent hosts api.anthropic.com   (or: dig +short api.anthropic.com)"
}

func runInterceptRevert(dir string) error {
	done := intercept.RevertMachine(dir)
	if len(done) == 0 {
		fmt.Printf("\n  nothing to revert: %s has no daiyaku entries.\n\n", intercept.HostsPath())
		return nil
	}
	fmt.Printf("\n  reverted: %s\n\n", strings.Join(done, "; "))
	return nil
}

// warnStale reports a previous run that never cleaned up. The redirect may still
// be live at this point, so the operator is told exactly what state the machine
// is in before a new run absorbs it. A record with a clean hosts file is a
// half-finished shutdown rather than a live redirect, and says so.
func warnStale(dir string) {
	st, ok := intercept.ReadState(dir)
	if !ok {
		return
	}
	live, err := intercept.HostsContains()
	fmt.Printf("\n  ! a previous intercept did not shut down cleanly (started %s, pid %d).\n",
		st.Started.Format("2006-01-02 15:04:05"), st.PID)
	switch {
	case err != nil:
		fmt.Printf("    Could not read %s to check (%v).\n", intercept.HostsPath(), err)
	case live:
		fmt.Printf("    %s is still redirected for: %s\n",
			intercept.HostsPath(), strings.Join(st.Hosts, ", "))
		fmt.Printf("    This run takes it over and cleans up on exit. To undo it instead: daiyaku intercept --revert\n")
	default:
		fmt.Printf("    The hosts file is already clean, so nothing is redirected. Clearing the record.\n")
		intercept.RevertMachine(dir)
	}
}

func interceptBanner(f *flags, in *intercept.Interceptor, rep *intercept.Report,
	hosts []string, sessionDir string, trustStore bool) {

	fmt.Printf("\n  daiyaku %s  INTERCEPT\n", version)
	fmt.Printf("  provider : %s\n", f.provider)
	fmt.Printf("  answering: %s on %s   [verified]\n", strings.Join(hosts, ", "), rep.Addr)
	fmt.Printf("  mode     : %s\n", f.mode)
	if f.classGrade >= 0 {
		fmt.Printf("  classifier: auto-grade severity %d\n", f.classGrade)
	} else {
		fmt.Printf("  classifier: manual (operator answers the safety classifier)\n")
	}
	fmt.Printf("  log: %s\n", sessionDir)
	if rep.NoChangeNeeded {
		fmt.Printf("\n  nothing on this machine was changed: %s already resolves here.\n",
			strings.Join(hosts, ", "))
	} else {
		fmt.Printf("\n  changed on this machine (undone automatically on exit):\n")
		if len(rep.Added) > 0 {
			fmt.Printf("    + hosts entries: %s\n", strings.Join(rep.Added, ", "))
		}
		if len(rep.Reused) > 0 {
			fmt.Printf("    · hosts entries already present: %s\n", strings.Join(rep.Reused, ", "))
		}
		for _, p := range rep.Parked {
			fmt.Printf("    ! an existing hosts entry for %s was commented out and will be restored\n", p)
		}
		fmt.Printf("    · dns cache: %s\n", rep.DNS)
		fmt.Printf("    · hosts backup: %s\n", rep.HostsBackup)
		if trustStore {
			if rep.TrustInstalled {
				fmt.Printf("    + CA installed machine-wide (removed on exit)\n")
			} else {
				fmt.Printf("    ! CA install failed: %s\n", rep.TrustError)
				fmt.Printf("      falling back to the environment variable below\n")
			}
		}
	}

	if !trustStore || !rep.TrustInstalled {
		fmt.Printf("\n  In the shell you start the harness from, trust daiyaku's certificate:\n\n")
		fmt.Printf("    %s\n", caEnvLine(rep.CertPath))
		fmt.Printf("\n  It must be the same shell you then run the harness in.\n")
	}
	fmt.Printf("\n  Start the harness with no other configuration. Ctrl+C here puts the machine back.\n")
	fmt.Printf("  If this process is killed hard, run: daiyaku intercept --revert\n\n")
}

// caEnvLine is the one line the operator copies. Node reads NODE_EXTRA_CA_CERTS,
// which covers Claude Code without touching the machine trust store.
func caEnvLine(certPath string) string {
	if runtime.GOOS == "windows" {
		// Not %q: it escapes the backslashes in a Windows path, and the operator is
		// going to paste this line verbatim.
		return fmt.Sprintf(`$env:NODE_EXTRA_CA_CERTS = "%s"`, certPath)
	}
	return fmt.Sprintf(`export NODE_EXTRA_CA_CERTS="%s"`, certPath)
}

func baseURLHint(provider string) string {
	if provider == "openai" {
		return "daiyaku codex"
	}
	return "daiyaku"
}

func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}
