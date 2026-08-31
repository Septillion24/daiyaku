package intercept

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// useFakeHosts points every hosts-file operation at a temporary copy, so the
// full apply and revert cycle runs without editing the machine under test.
func useFakeHosts(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := hostsPathOverride
	hostsPathOverride = path
	t.Cleanup(func() { hostsPathOverride = prev })
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The whole cycle: mint a CA, redirect a name, serve TLS for it, prove the
// self-test verifies the chain a harness would be offered, then put the file
// back byte for byte. "localhost" stands in for the vendor name because it
// already resolves to loopback, so the test never depends on the machine's real
// name resolution.
func TestApplyServeSelfTestRevert(t *testing.T) {
	original := "127.0.0.1\tlocalhost\n# keep me\n"
	path := useFakeHosts(t, original)
	dir := t.TempDir()
	port := freePort(t)

	in, err := New(Config{Dir: dir, Hosts: []string{"localhost"},
		Addr: fmt.Sprintf("127.0.0.1:%d", port)})
	if err != nil {
		t.Fatal(err)
	}
	if problems := in.Preflight(); len(problems) > 0 {
		t.Fatalf("preflight: %v", problems)
	}

	rep, err := in.Apply()
	if err != nil {
		t.Fatal(err)
	}
	// localhost already answers at 127.0.0.1, so the machine needs no change at
	// all: the interception still has to work, and nothing may be edited or left
	// behind. This is the test-VM case, where DNS already points at the operator.
	if !rep.NoChangeNeeded {
		t.Errorf("edited the hosts file for a name that already resolved here: %+v", rep)
	}
	if before, _ := os.ReadFile(path); string(before) != original {
		t.Errorf("hosts file was modified needlessly: %q", before)
	}
	if _, ok := ReadState(dir); ok {
		t.Error("state was recorded for a run that changed nothing")
	}

	srv := &http.Server{
		Addr: in.cfg.Addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("x-daiyaku", "test")
			w.Write([]byte("{}"))
		}),
		TLSConfig: in.TLSConfig(),
	}
	ln, err := net.Listen("tcp", in.cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	waitForPort(t, in.cfg.Addr)

	if err := in.SelfTest("/v1/models"); err != nil {
		t.Fatalf("self-test failed against a working intercept: %v", err)
	}

	// Nothing was changed, so there is nothing to undo.
	in.Revert()
	after, _ := os.ReadFile(path)
	if string(after) != original {
		t.Errorf("hosts file not restored:\nwant %q\ngot  %q", original, after)
	}
	if _, ok := ReadState(dir); ok {
		t.Error("state file survived revert, so the next run would report a false stale intercept")
	}
}

// Cleanup runs from a deferred call, a signal handler, and the next startup, so
// calling it repeatedly, or without a successful apply, must be harmless.
func TestRevertIsIdempotentAndSafeWithoutApply(t *testing.T) {
	original := "127.0.0.1\tlocalhost\n"
	path := useFakeHosts(t, original)
	dir := t.TempDir()

	in, err := New(Config{Dir: dir, Hosts: []string{"api.example.test"}, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if done := in.Revert(); len(done) != 0 {
		t.Errorf("revert without apply did work: %v", done)
	}
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	first := in.Revert()
	second := in.Revert()
	if len(first) == 0 {
		t.Error("first revert did nothing")
	}
	if len(second) != 0 {
		t.Errorf("second revert repeated work: %v", second)
	}
	after, _ := os.ReadFile(path)
	if string(after) != original {
		t.Errorf("hosts not restored: %q", after)
	}
}

// A crashed run leaves the redirect and a state file. Recovery has to work from
// disk alone, without the Interceptor that made the change.
func TestRevertMachineRecoversFromACrash(t *testing.T) {
	original := "127.0.0.1\tlocalhost\n"
	path := useFakeHosts(t, original)
	dir := t.TempDir()

	in, err := New(Config{Dir: dir, Hosts: []string{"api.example.test"}, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(); err != nil {
		t.Fatal(err)
	}
	// The process "dies" here: no Revert is called, and in is discarded.
	st, ok := ReadState(dir)
	if !ok {
		t.Fatal("no state to recover from")
	}
	if st.RevertHint == "" {
		t.Error("state carries no instruction for a human recovering by hand")
	}
	if only, _ := HostsContains(); !only {
		t.Error("HostsContains did not see the leftover entry a new run must warn about")
	}

	if done := RevertMachine(dir); len(done) == 0 {
		t.Fatal("RevertMachine did nothing")
	}
	after, _ := os.ReadFile(path)
	if string(after) != original {
		t.Errorf("crash recovery did not restore the file: %q", after)
	}
}

// The certificate offered for an intercepted name must verify against the CA a
// harness would be told to trust, including for a name minted on demand.
func TestMintedCertificateVerifies(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.anthropic.com", "api.openai.com"} {
		leaf, err := ca.leafFor(host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		cert, err := x509.ParseCertificate(leaf.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots: ca.Pool(), DNSName: host,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("%s: a harness would reject this certificate: %v", host, err)
		}
	}
}

// The CA is reused across runs. Regenerating it every start would make the
// operator re-trust a new certificate every session.
func TestCAIsStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cert.SerialNumber.Cmp(second.Cert.SerialNumber) != 0 {
		t.Error("a second run minted a new CA, invalidating the trust the operator established")
	}
	// Unix permission bits do not translate on Windows, where Go reports 0666 for
	// any writable file regardless of its actual ACL. There the key is protected
	// by living under the per-user config directory, which is already restricted.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "ca.key"))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("ca.key mode is %v: the signing key must not be group or world readable", mode)
		}
	}
}

// The self-test must fail loudly when something other than daiyaku answers,
// which is what a stale DNS answer or a competing service looks like.
func TestSelfTestRejectsAStranger(t *testing.T) {
	useFakeHosts(t, "127.0.0.1\tlocalhost\n")
	dir := t.TempDir()
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	in, err := New(Config{Dir: dir, Hosts: []string{"localhost"}, Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	// Something else on the port, serving a valid certificate but not daiyaku.
	srv := &http.Server{
		Addr:      addr,
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }),
		TLSConfig: in.TLSConfig(),
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	waitForPort(t, addr)

	err = in.SelfTest("/v1/models")
	if err == nil {
		t.Fatal("self-test passed against a server that is not daiyaku")
	}
	if !strings.Contains(err.Error(), "not daiyaku") {
		t.Errorf("unhelpful failure message: %v", err)
	}
}

func waitForPort(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}
