package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}

func sanitizeFileName(s string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-", ":", "-")
	return replacer.Replace(s)
}

func workspacePathContainsPattern(workspace, pathSpec, pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	paths, err := matchingWorkspaceFiles(workspace, pathSpec)
	if err != nil {
		return false
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if re.Match(data) {
			return true
		}
	}
	return false
}

func matchingWorkspaceFiles(workspace, pathSpec string) ([]string, error) {
	pathSpec = filepath.ToSlash(strings.TrimSpace(pathSpec))
	if pathSpec == "" {
		return nil, fmt.Errorf("path spec must not be empty")
	}
	if !containsGlobMeta(pathSpec) {
		return []string{filepath.Join(workspace, filepath.FromSlash(pathSpec))}, nil
	}
	re, err := pathSpecRegexp(pathSpec)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if shouldSkipEvalScanDir(entry.Name()) && path != workspace {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func containsGlobMeta(pathSpec string) bool {
	return strings.ContainsAny(pathSpec, "*?[")
}

func shouldSkipEvalScanDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "coverage":
		return true
	default:
		return false
	}
}

func pathSpecRegexp(pathSpec string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pathSpec); i++ {
		if strings.HasPrefix(pathSpec[i:], "**/") {
			b.WriteString(`(?:.*/)?`)
			i += len("**/") - 1
			continue
		}
		switch pathSpec[i] {
		case '*':
			if i+1 < len(pathSpec) && pathSpec[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			end := strings.IndexByte(pathSpec[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
			} else {
				class := pathSpec[i : i+end+2]
				b.WriteString(class)
				i += end + 1
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(pathSpec[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
