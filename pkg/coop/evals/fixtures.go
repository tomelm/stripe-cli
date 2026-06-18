package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (r *Runner) prepareFixture(ctx context.Context, c Case, resultDir, workspace string) (*ExternalFixture, error) {
	localFixture := filepath.Join(r.opts.FixturesDir, c.Fixture)
	if info, err := os.Stat(localFixture); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("fixture %s is not a directory", localFixture)
		}
		return nil, copyDir(localFixture, workspace)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	fixture, err := r.loadExternalFixture(c.Fixture)
	if err != nil {
		return nil, err
	}
	if err := r.materializeExternalFixture(ctx, fixture, resultDir, workspace); err != nil {
		return nil, err
	}
	return &fixture, nil
}

func (r *Runner) loadExternalFixture(id string) (ExternalFixture, error) {
	path := filepath.Join(r.opts.ExternalFixturesDir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ExternalFixture{}, fmt.Errorf("fixture %q not found as local directory or external manifest", id)
		}
		return ExternalFixture{}, err
	}
	var fixture ExternalFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return ExternalFixture{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if fixture.ID == "" {
		fixture.ID = id
	}
	if fixture.ID != id {
		return ExternalFixture{}, fmt.Errorf("external fixture %s has mismatched id %q", path, fixture.ID)
	}
	if fixture.Source.Type == "" {
		fixture.Source.Type = "git"
	}
	if fixture.Source.Type != "git" {
		return ExternalFixture{}, fmt.Errorf("external fixture %q has unsupported source type %q", id, fixture.Source.Type)
	}
	if fixture.Source.URL == "" {
		return ExternalFixture{}, fmt.Errorf("external fixture %q source.url is required", id)
	}
	if fixture.Source.Ref == "" {
		return ExternalFixture{}, fmt.Errorf("external fixture %q source.ref is required", id)
	}
	return fixture, nil
}

func (r *Runner) materializeExternalFixture(ctx context.Context, fixture ExternalFixture, resultDir, workspace string) error {
	sourceDir := filepath.Join(resultDir, "fixture-source")
	cloneDir := filepath.Join(sourceDir, "repo")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return err
	}
	if err := runGitCommand(ctx, sourceDir, "clone", "--no-checkout", fixture.Source.URL, cloneDir); err != nil {
		return err
	}
	if len(fixture.Source.SparseCheckout) > 0 {
		if err := runGitCommand(ctx, cloneDir, "sparse-checkout", "init", "--cone"); err != nil {
			return err
		}
		args := append([]string{"sparse-checkout", "set"}, fixture.Source.SparseCheckout...)
		if err := runGitCommand(ctx, cloneDir, args...); err != nil {
			return err
		}
	}
	if err := runGitCommand(ctx, cloneDir, "fetch", "--depth=1", "origin", fixture.Source.Ref); err != nil {
		return err
	}
	if err := runGitCommand(ctx, cloneDir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}

	src := cloneDir
	if fixture.CopyPath != "" && fixture.CopyPath != "." {
		copyPath, err := safeRelativePath(fixture.CopyPath)
		if err != nil {
			return fmt.Errorf("external fixture %q copy_path: %w", fixture.ID, err)
		}
		src = filepath.Join(cloneDir, copyPath)
	}
	if err := copyDir(src, workspace); err != nil {
		return err
	}
	if fixture.Overlay != "" {
		overlay := fixture.Overlay
		if !filepath.IsAbs(overlay) {
			overlay = filepath.Join(r.opts.ExternalFixturesDir, filepath.FromSlash(overlay))
		}
		if err := copyDir(overlay, workspace); err != nil {
			return fmt.Errorf("applying overlay for fixture %q: %w", fixture.ID, err)
		}
	}
	return nil
}

func runGitCommand(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(data))
	}
	return nil
}

func initFixtureGit(ctx context.Context, workspace string) error {
	commands := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "coop-eval@example.com"},
		{"git", "config", "user.name", "Co-op Eval"},
		{"git", "add", "."},
		{"git", "commit", "-m", "fixture baseline"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = workspace
		if data, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, string(data))
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("fixture %s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" && rel != "." {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func safeRelativePath(path string) (string, error) {
	path = filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	if path == "." {
		return "", nil
	}
	if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must stay within fixture root")
	}
	return path, nil
}
