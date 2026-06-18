package evals

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func evalEnv(xdgHome, homeDir, shimDir, repoRoot, realStripeBin, stripeLog string, port int) []string {
	hostEnv := envMap(os.Environ())
	env := cleanEvalEnv(hostEnv)
	hostHome := os.Getenv("HOME")
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" && hostHome != "" {
		codexHome = filepath.Join(hostHome, ".codex")
	}
	env = append(env,
		"XDG_CONFIG_HOME="+xdgHome,
		"HOME="+homeDir,
		"COOP_EVAL_REPO_ROOT="+repoRoot,
		"COOP_EVAL_REAL_STRIPE="+realStripeBin,
		"COOP_EVAL_STRIPE_LOG="+stripeLog,
		fmt.Sprintf("COOP_EVAL_PORT=%d", port),
		fmt.Sprintf("PORT=%d", port),
		"SSH_TTY=coop-eval",
		"SSH_CONNECTION=coop-eval",
		"SSH_CLIENT=coop-eval",
		"BROWSER=coop-eval-browser-disabled",
		"COOP_EVAL_BROWSER_AUTOMATION=disabled",
		"CHROME_BIN="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"CHROME_PATH="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"GOOGLE_CHROME_BIN="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"PUPPETEER_EXECUTABLE_PATH="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	return env
}

func envMap(values []string) map[string]string {
	env := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok && key != "" {
			env[key] = val
		}
	}
	return env
}

func cleanEvalEnv(hostEnv map[string]string) []string {
	allowed := []string{
		"CI",
		"COLORTERM",
		"DOCKER_CONTEXT",
		"DOCKER_HOST",
		"LANG",
		"LC_ALL",
		"LC_CTYPE",
		"NO_COLOR",
		"STRIPE_API_KEY",
		"STRIPE_PUBLISHABLE_KEY",
		"STRIPE_SECRET_KEY",
		"STRIPE_WEBHOOK_SECRET",
		"TERM",
		"TMPDIR",
	}
	var env []string
	for _, key := range allowed {
		if value, ok := hostEnv[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range hostEnv {
		if strings.HasPrefix(key, "LC_") && key != "LC_ALL" && key != "LC_CTYPE" {
			env = append(env, key+"="+value)
		}
	}
	sort.Strings(env)
	return env
}

func reserveEvalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return addr.Port, nil
}
