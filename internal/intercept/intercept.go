package intercept

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State records what a run changed on this machine. It is written before the
// change and removed after it is undone, so a run killed hard leaves a note
// behind: the next start finds it, says the machine is still redirected, and
// offers to put it back. Without this the failure mode is a laptop where every
// Claude tool is broken and nothing says why.
type State struct {
	PID          int       `json:"pid"`
	Started      time.Time `json:"started"`
	Hosts        []string  `json:"hosts"`
	HostsBackup  string    `json:"hosts_backup,omitempty"`
	TrustStore   bool      `json:"trust_store"`
	CertPath     string    `json:"cert_path"`
	DaiyakuAddr  string    `json:"addr"`
	RevertHint   string    `json:"revert_hint"`
	StateVersion int       `json:"state_version"`
}

const stateVersion = 1

func statePath(dir string) string { return filepath.Join(dir, "intercept-state.json") }

// ReadState returns the record left by a previous run, if any.
func ReadState(dir string) (*State, bool) {
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		return nil, false
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return nil, false
	}
	return &s, true
}

func writeState(dir string, s *State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(dir), b, 0o600)
}

func clearState(dir string) { os.Remove(statePath(dir)) }

// Config is what a run needs to take over a vendor address.
type Config struct {
	Dir        string   // where the CA, backups, and state live
	Hosts      []string // vendor names to answer for
	Addr       string   // listen address, normally 127.0.0.1:443
	TrustStore bool     // also install the CA machine-wide
}

// Interceptor owns the machine changes for one run and is responsible for
// undoing every one of them.
type Interceptor struct {
	cfg Config
	CA  *CA

	mu       sync.Mutex
	reverted bool
	applied  bool
	state    State
}

// New prepares an interceptor and its CA without touching the machine yet, so
// preflight can report problems before anything is changed.
func New(cfg Config) (*Interceptor, error) {
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("no hostnames to intercept")
	}
	ca, err := LoadOrCreateCA(cfg.Dir)
	if err != nil {
		return nil, err
	}
	return &Interceptor{cfg: cfg, CA: ca}, nil
}

// Preflight reports every reason this run cannot work, before anything is
// changed, so the operator fixes them all at once instead of one per attempt.
func (in *Interceptor) Preflight() []string {
	var problems []string
	if len(in.hostsNeeded()) > 0 {
		if err := HostsWritable(); err != nil {
			problems = append(problems, fmt.Sprintf("cannot write %s (%v): %s",
				HostsPath(), rootError(err), PermissionHint()))
		}
	}
	ln, err := net.Listen("tcp", in.cfg.Addr)
	if err != nil {
		problems = append(problems, fmt.Sprintf("cannot listen on %s (%v): %s",
			in.cfg.Addr, rootError(err), portHint(in.cfg.Addr)))
	} else {
		ln.Close()
	}
	return problems
}

// rootError strips the layers of wrapping off an OS error so the message the
// operator reads is the one the OS produced.
func rootError(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err
	}
	if oe, ok := err.(*net.OpError); ok {
		return oe.Err
	}
	return err
}

func portHint(addr string) string {
	if strings.HasSuffix(addr, ":443") {
		return "another service holds 443 (IIS, Docker Desktop, a dev server), " +
			"or the port needs privilege on this OS"
	}
	return "the port is in use or needs privilege"
}

// Apply makes the machine changes and records them. It is the only function
// here that modifies anything outside daiyaku's own directory.
func (in *Interceptor) Apply() (*Report, error) {
	in.mu.Lock()
	defer in.mu.Unlock()

	rep := &Report{CertPath: in.CA.CertPath, Addr: in.cfg.Addr}

	if len(in.hostsNeeded()) == 0 {
		// The names already answer here, so there is nothing to change and nothing
		// to undo. Common on a test VM whose DNS already points at the operator.
		rep.NoChangeNeeded = true
		rep.Reused = in.cfg.Hosts
		return rep, nil
	}

	backup, err := backupHosts(in.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("back up hosts file: %w", err)
	}
	rep.HostsBackup = backup

	current, err := os.ReadFile(HostsPath())
	if err != nil {
		return nil, fmt.Errorf("read hosts file: %w", err)
	}
	edit := planHosts(string(current), listenIP(in.cfg.Addr), in.cfg.Hosts)

	// State is written before the edit: a crash between the two leaves a note
	// about a change that did not happen, which is recoverable. The reverse is a
	// change with no note, which is not.
	in.state = State{
		PID: os.Getpid(), Started: time.Now(), Hosts: in.cfg.Hosts,
		HostsBackup: backup, TrustStore: in.cfg.TrustStore, CertPath: in.CA.CertPath,
		DaiyakuAddr: in.cfg.Addr, StateVersion: stateVersion,
		RevertHint: "daiyaku intercept --revert   (or remove the daiyaku-tagged lines from " +
			HostsPath() + ")",
	}
	if err := writeState(in.cfg.Dir, &in.state); err != nil {
		return nil, fmt.Errorf("record intercept state: %w", err)
	}

	if err := writeHosts(edit.content); err != nil {
		clearState(in.cfg.Dir)
		return nil, fmt.Errorf("write hosts file: %w", err)
	}
	in.applied = true
	rep.Added, rep.Reused, rep.Parked = edit.added, edit.already, edit.disabled
	rep.DNS = FlushDNS()

	if in.cfg.TrustStore {
		cmd := TrustCommands(in.CA.CertPath)
		if out, err := runCommand(cmd.Install); err != nil {
			rep.TrustError = fmt.Sprintf("%v: %s", err, out)
		} else {
			rep.TrustInstalled = true
		}
	}
	return rep, nil
}

// Report is what Apply changed, for the banner and for the record.
type Report struct {
	Added, Reused, Parked []string
	HostsBackup           string
	DNS                   string
	CertPath              string
	Addr                  string
	TrustInstalled        bool
	TrustError            string
	// NoChangeNeeded means the names already resolved here, so the machine was
	// left completely untouched.
	NoChangeNeeded bool
}

// Revert undoes everything Apply did. Safe to call more than once and safe to
// call when Apply never ran, because cleanup runs from a deferred call, a signal
// handler, and the next start, and all three may fire for the same run.
func (in *Interceptor) Revert() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	// Nothing applied yet means nothing to undo, and must NOT latch: cleanup can
	// be called before Apply on an early error path and still has to work later.
	if in.reverted || !in.applied {
		return nil
	}
	in.reverted = true
	return revertWithCert(in.cfg.Dir, in.cfg.TrustStore, in.CA.CertPath)
}

// RevertMachine undoes a previous run's changes using only what is on disk, so
// it works after a crash, from a different shell, or from a rebuilt binary.
func RevertMachine(dir string) []string {
	st, ok := ReadState(dir)
	trust := ok && st.TrustStore
	certPath := filepath.Join(dir, "ca.crt")
	if ok && st.CertPath != "" {
		certPath = st.CertPath
	}
	return revertWithCert(dir, trust, certPath)
}

func revertWithCert(dir string, trust bool, certPath string) []string {
	var done []string
	current, err := os.ReadFile(HostsPath())
	if err != nil {
		return []string{fmt.Sprintf("could not read %s: %v", HostsPath(), rootError(err))}
	}
	restored, changed := planRevert(string(current))
	if changed {
		if err := writeHosts(restored); err != nil {
			return []string{fmt.Sprintf("could not restore %s (%v): %s. "+
				"Remove the daiyaku-tagged lines by hand to put this machine back.",
				HostsPath(), rootError(err), PermissionHint())}
		}
		done = append(done, "hosts entries removed")
		FlushDNS()
	}
	if trust {
		cmd := TrustCommands(certPath)
		if out, err := runCommand(cmd.Remove); err != nil {
			done = append(done, fmt.Sprintf("could not remove the CA from the trust store (%v: %s); "+
				"remove it by hand with: %s", err, out, strings.Join(cmd.Remove, " ")))
		} else {
			done = append(done, "CA removed from the trust store")
		}
	}
	clearState(dir)
	return done
}

func listenIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

// TLSConfig is the server configuration that answers for the intercepted names.
func (in *Interceptor) TLSConfig() *tls.Config {
	return in.CA.TLSConfig(in.cfg.Hosts[0])
}

// SelfTest proves the whole path works before the harness is ever started: it
// resolves the vendor name, connects, verifies the certificate against this CA,
// and calls an endpoint that does not consume an operator turn. A failure here
// names the broken step instead of leaving the operator to read a Node TLS error
// later, halfway through a session.
func (in *Interceptor) SelfTest(probePath string) error {
	host := in.cfg.Hosts[0]
	_, port, err := net.SplitHostPort(in.cfg.Addr)
	if err != nil {
		port = "443"
	}
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: in.CA.Pool(), MinVersion: tls.VersionTLS12},
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 5 * time.Second,
		},
	}
	url := "https://" + net.JoinHostPort(host, port) + probePath
	if port == "443" {
		url = "https://" + host + probePath
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("%s did not reach daiyaku: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("x-daiyaku") == "" {
		return fmt.Errorf("%s was answered by something that is not daiyaku: "+
			"the name may still resolve to the real vendor", url)
	}
	return nil
}

// alreadyResolves reports whether host already answers at ip, in which case no
// hosts entry is needed and no privilege either. That happens on a test VM whose
// DNS is already pointed at the operator's box, and it is the difference between
// needing an elevated shell and not.
func alreadyResolves(host, ip string) bool {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a == ip {
			return true
		}
	}
	return false
}

// hostsNeeded returns the names that do not already point at ip. An existing
// daiyaku entry does not count as "already resolves": this run has to own and
// clean up any line carrying the tag, so it goes through the edit path.
func (in *Interceptor) hostsNeeded() []string {
	if tagged, err := HostsContains(); err == nil && tagged {
		return in.cfg.Hosts
	}
	ip := listenIP(in.cfg.Addr)
	var need []string
	for _, h := range in.cfg.Hosts {
		if !alreadyResolves(h, ip) {
			need = append(need, h)
		}
	}
	return need
}
