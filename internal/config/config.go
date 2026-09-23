// Package config loads the launcher's settings from the environment and an
// optional .env file sitting next to the binary (or in the working directory).
//
// Precedence: real environment wins, .env fills the gaps. There is no
// repo-wide config file — a standalone install is configured by one .env.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Role string

const (
	Leader   Role = "leader"
	Follower Role = "follower"
)

type Config struct {
	Port          int
	Host          string // hostname shown in the startup banner (cosmetic)
	ClaudeBin     string // override when `claude` isn't on PATH for spawned shells
	DefaultDir    string // pre-filled directory in the UI form
	DataDir       string // session logs + recents.json (gitignored)
	ConfigFile    string // explicit path to config.yaml; empty = search
	Role          Role
	NodeID        string // stable routing key, unique in the ring
	NodeLabel     string // pretty name shown in the UI
	FollowersSpec string // raw `id|label|url,…`; parsed in package nodes

	// TurnTimeout bounds one API-submitted turn (a `claude -p --resume`
	// process). A turn that hits it is killed and reported as state:"timeout".
	TurnTimeout time.Duration
}

// Load reads .env (working directory first, then alongside the executable) and
// layers the process environment on top.
func Load() Config {
	for _, p := range envCandidates() {
		loadDotEnv(p)
	}

	dataDir := env("CLAUDE_LAUNCHER_DATA_DIR", "")
	if dataDir == "" {
		dataDir = filepath.Join(baseDir(), "data")
	}

	role := Leader
	if strings.EqualFold(env("CLAUDE_LAUNCHER_ROLE", ""), string(Follower)) {
		role = Follower
	}

	return Config{
		Port:          envInt("CLAUDE_LAUNCHER_PORT", 8922),
		Host:          env("CLAUDE_LAUNCHER_HOST", "localhost"),
		ClaudeBin:     env("CLAUDE_BIN", "claude"),
		DefaultDir:    env("CLAUDE_LAUNCHER_DEFAULT_DIR", ""),
		DataDir:       dataDir,
		ConfigFile:    env("CLAUDE_LAUNCHER_CONFIG", ""),
		Role:          role,
		NodeID:        env("CLAUDE_LAUNCHER_NODE_ID", "local"),
		NodeLabel:     env("CLAUDE_LAUNCHER_NODE_LABEL", ""),
		FollowersSpec: env("CLAUDE_LAUNCHER_FOLLOWERS", ""),
		TurnTimeout:   envDuration("CLAUDE_LAUNCHER_TURN_TIMEOUT", 15*time.Minute),
	}
}

// baseDir is the directory holding the executable, falling back to the working
// directory. Data lands next to the binary so a `go run` and an installed
// binary both behave predictably.
func baseDir() string {
	if exe, err := os.Executable(); err == nil {
		if dir, err := filepath.EvalSymlinks(filepath.Dir(exe)); err == nil {
			// `go run` drops the binary in a temp dir — data there would vanish.
			if !strings.Contains(dir, "go-build") {
				return dir
			}
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func envCandidates() []string {
	paths := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), ".env"))
	}
	return paths
}

// loadDotEnv applies KEY=VALUE lines from path without overriding anything
// already set in the environment. Supports #-comments, `export ` prefixes and
// single/double quoted values. Missing files are not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envDuration accepts a Go duration ("15m", "90s"); a bare number is read as
// seconds so "900" also works.
func envDuration(key string, def time.Duration) time.Duration {
	v := env(key, "")
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}
