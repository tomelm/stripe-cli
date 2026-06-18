package evals

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func runLoggedCommand(ctx context.Context, name string, args []string, cwd string, env []string, stdoutPath, stderrPath string) commandRecord {
	started := time.Now()
	record := commandRecord{Name: name, Args: args, Cwd: cwd, StartedAt: started.UTC(), Stdout: stdoutPath, Stderr: stderrPath}
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		record.ExitCode = -1
		return record
	}
	defer stdout.Close()
	stderr, err := os.Create(stderrPath)
	if err != nil {
		record.ExitCode = -1
		return record
	}
	defer stderr.Close()
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	prepareProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		record.DurationMS = time.Since(started).Milliseconds()
		record.ExitCode = exitCode(err)
		return record
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		terminateProcessGroup(cmd.Process.Pid)
		waitErr = <-done
		if waitErr == nil {
			waitErr = ctx.Err()
		}
	}
	record.DurationMS = time.Since(started).Milliseconds()
	record.ExitCode = exitCode(waitErr)
	return record
}

func runCommandChecks(ctx context.Context, checks []CommandCheck, workspace, resultDir string, env []string, result *CaseResult) ([]commandRecord, map[string]string) {
	var records []commandRecord
	artifacts := map[string]string{}
	if len(checks) == 0 {
		return records, artifacts
	}
	checkDir := filepath.Join(resultDir, "checks")
	if err := os.MkdirAll(checkDir, 0755); err != nil {
		result.Checks = append(result.Checks, CheckResult{
			Name:    "functional_check",
			Passed:  false,
			Message: fmt.Sprintf("creating check artifact directory: %v", err),
			Weight:  10,
		})
		return records, artifacts
	}

	for _, check := range checks {
		slug := sanitizeFileName(strings.ToLower(strings.ReplaceAll(check.Name, " ", "-")))
		if slug == "" {
			slug = "check"
		}
		stdoutPath := filepath.Join(checkDir, slug+".stdout.txt")
		stderrPath := filepath.Join(checkDir, slug+".stderr.txt")
		artifacts["check_"+slug+"_stdout"] = stdoutPath
		artifacts["check_"+slug+"_stderr"] = stderrPath

		cwd := workspace
		if check.Workdir != "" {
			rel, err := safeRelativePath(check.Workdir)
			if err != nil {
				result.Checks = append(result.Checks, CheckResult{
					Name:    "functional_check",
					Passed:  false,
					Message: fmt.Sprintf("%s: invalid workdir: %v", check.Name, err),
					Weight:  10,
				})
				continue
			}
			cwd = filepath.Join(workspace, rel)
		}

		commandEnv := append([]string{}, env...)
		commandEnv = append(commandEnv, "COMPOSE_PROJECT_NAME="+composeProjectName(result.ID))
		commandEnv = commandCheckEnv(commandEnv, check.Env)
		checkCtx := ctx
		cancel := func() {}
		if check.TimeoutSeconds > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
		}
		record := runLoggedCommand(checkCtx, "sh", []string{"-c", check.Command}, cwd, commandEnv, stdoutPath, stderrPath)
		checkErr := checkCtx.Err()
		cancel()
		redactSensitiveArtifacts(stdoutPath, stderrPath)
		records = append(records, record)

		passed := record.ExitCode == 0
		message := fmt.Sprintf("%s exit=%d", check.Name, record.ExitCode)
		if checkErr != nil {
			message = fmt.Sprintf("%s: %v", check.Name, checkErr)
		}
		result.Checks = append(result.Checks, CheckResult{
			Name:    "functional_check",
			Passed:  passed,
			Message: message,
			Weight:  10,
		})
	}
	return records, artifacts
}

func commandCheckEnv(base []string, extra map[string]string) []string {
	env := append([]string{}, base...)
	if len(extra) == 0 {
		return env
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+extra[key])
	}
	return env
}

func composeProjectName(id string) string {
	id = strings.ToLower(sanitizeFileName(id))
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" {
		name = "case"
	}
	return "coop-eval-" + name
}
