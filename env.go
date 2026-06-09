package main

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// We load a .env file ourselves (Go has no built-in loader, and we want zero
// dependencies). The tricky bit: config like secretKey is read at package-init
// time, *before* main() runs — so we can't just load .env at the top of main().
// Instead every getenv() call funnels through ensureEnv(), and sync.Once makes
// the file load exactly once, on the very first read, no matter who reads first.
var envOnce sync.Once

func ensureEnv() { envOnce.Do(loadDotEnv) }

// loadDotEnv parses KEY=VALUE lines from .env (or $ENV_FILE) into the process
// environment. Real, already-set environment variables ALWAYS win, so PM2's env
// block / shell exports override the file — same precedence as dotenv libraries.
func loadDotEnv() {
	path := os.Getenv("ENV_FILE")
	if path == "" {
		path = ".env"
	}
	f, err := os.Open(path)
	if err != nil {
		return // no .env is perfectly fine
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue // blank or comment
		}
		line = strings.TrimPrefix(line, "export ") // tolerate `export KEY=VAL`

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			continue
		}
		// Strip a single pair of surrounding quotes if present.
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		// Don't clobber a real env var that's already set.
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
