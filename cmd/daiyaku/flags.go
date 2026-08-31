package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: daiyaku %s [flags]\n", name)
		fs.PrintDefaults()
	}
	return fs
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// normalizeAddr accepts a bare port, :port, or host:port and returns a full
// listen address. An unusable value is reported here rather than surfacing much
// later as a bare net error from ListenAndServe.
func normalizeAddr(a string) (string, error) {
	if a == "" {
		return a, nil
	}
	if n, err := strconv.Atoi(a); err == nil {
		if n < 1 || n > 65535 {
			return "", fmt.Errorf("port %d out of range (1-65535)", n)
		}
		return "127.0.0.1:" + a, nil
	}
	if strings.HasPrefix(a, ":") {
		a = "127.0.0.1" + a
	}
	_, port, err := net.SplitHostPort(a)
	if err != nil {
		return "", fmt.Errorf("bad address %q: use host:port, :port, or a bare port", a)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("bad port %q in %q", port, a)
	}
	return a, nil
}

// isLoopback reports whether the listen address is reachable only from this
// machine. The mock does not authenticate anything it serves, so binding it
// anywhere else exposes the operator console's request feed to the network.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // an empty host means all interfaces
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// firstSet returns the first non-empty value, which is how the layered defaults
// are resolved: environment, then launch profile, then the built-in.
func firstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
