package main

import (
	"flag"
	"fmt"
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

func normalizeAddr(a string) string {
	if a == "" {
		return a
	}
	if _, err := strconv.Atoi(a); err == nil {
		return "127.0.0.1:" + a
	}
	if strings.HasPrefix(a, ":") {
		return "127.0.0.1" + a
	}
	return a
}
