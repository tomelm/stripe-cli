package main

// fix.go — dry-run removal. Proves the byte-span claim end to end: for each
// finding, compute the span that removes the whole parameter entry (pair +
// separator, or builder link, or whole statement), apply the edits in memory,
// REPARSE the result, and report whether the file is still syntactically
// valid. Nothing is written to disk.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

type span struct {
	start, end uint32
	label      string
}

// removalSpan computes the byte range that deletes a matched parameter
// entirely, per language shape.
func removalSpan(key *ts.Node, spec langSpec, lang *ts.Language, src []byte) (span, bool) {
	if spec.pairKinds == nil {
		// Java builder: the key is the method name of an invocation link.
		var inv *ts.Node
		for cur := key.Parent(); cur != nil; cur = cur.Parent() {
			if containsStr(spec.anchorKinds, cur.Type(lang)) {
				inv = cur
				break
			}
		}
		if inv == nil {
			return span{}, false
		}
		// Standalone statement (paramsBuilder.addX("card");) → remove the
		// whole statement; chain link (.addX("card")) → remove from the end
		// of the receiver through the end of this link.
		if p := inv.Parent(); p != nil && p.Type(lang) == "expression_statement" {
			return withLine(span{p.StartByte(), p.EndByte(), "statement"}, src), true
		}
		recv := inv.NamedChild(0)
		if recv == nil {
			return span{}, false
		}
		return span{recv.EndByte(), inv.EndByte(), "chain-link"}, true
	}

	// Pair-shaped languages: the enclosing pair node plus one separator.
	var pair *ts.Node
	for cur := key.Parent(); cur != nil; cur = cur.Parent() {
		if containsStr(spec.pairKinds, cur.Type(lang)) {
			pair = cur
			break
		}
	}
	if pair == nil {
		return span{}, false
	}
	s, e := pair.StartByte(), pair.EndByte()
	// Prefer swallowing the trailing comma; else the leading one.
	if i := skipWS(src, int(e), +1); i < len(src) && src[i] == ',' {
		return span{s, uint32(i + 1), "pair+trailing-comma"}, true
	}
	if i := skipWS(src, int(s)-1, -1); i >= 0 && src[i] == ',' {
		return span{uint32(i), e, "pair+leading-comma"}, true
	}
	return span{s, e, "pair"}, true
}

// withLine expands a statement span to swallow its trailing newline so no
// blank line is left behind.
func withLine(sp span, src []byte) span {
	if i := skipWS(src, int(sp.end), +1); i < len(src) && src[i] == '\n' {
		sp.end = uint32(i + 1)
	}
	return sp
}

func skipWS(src []byte, i, dir int) int {
	for i >= 0 && i < len(src) && (src[i] == ' ' || src[i] == '\t' || (dir > 0 && src[i] == '\n')) {
		i += dir
	}
	return i
}

// fixRun computes removal spans per file, applies them in memory, reparses,
// and — only when apply is true AND the reparse is clean — writes the file.
// A file that fails reparse is never written.
func fixRun(root string, rule Rule, apply, includeAll bool) (*FixReport, error) {
	if rule.Action != "remove" {
		return nil, fmt.Errorf("rule %s is action=%q: it detects and advises but has no automatic fix — run `doctor` and follow %s", rule.ID, rule.Action, rule.Docs)
	}
	type fileEdit struct {
		spec  langSpec
		spans []span
	}
	edits := map[string]*fileEdit{}

	findings, _, _, err := scan(root, rule)
	if err != nil {
		return nil, err
	}
	report := &FixReport{Command: "fix", Applied: apply, AllClean: true}
	for _, f := range findings {
		// Gate: dynamic values and deliberate single-method restrictions are
		// never auto-removed unless --all — the doctor's own taxonomy says
		// they need human judgment.
		if !includeAll {
			switch intent := classifyIntent(f.Value); intent {
			case "dynamic":
				report.Skipped = append(report.Skipped, SkippedFinding{File: f.File, Line: f.Line, Intent: intent,
					Reason: "value is computed at runtime — review the routing logic (use --all to override)"})
				continue
			case "deliberate":
				report.Skipped = append(report.Skipped, SkippedFinding{File: f.File, Line: f.Line, Intent: intent,
					Reason: "single-method restriction looks intentional — consider excluded_payment_method_types (use --all to override)"})
				continue
			}
		}
		spec := specs[strings.ToLower(ext(f.File))]
		src, err := os.ReadFile(f.File)
		if err != nil {
			continue
		}
		lang := spec.lang()
		tree, err := ts.NewParser(lang).Parse(src)
		if err != nil {
			continue
		}
		// Re-locate the key node at the finding's position.
		key := tree.RootNode().NamedNodeAtByte(byteAt(src, f.Line, f.Col))
		if key == nil {
			continue
		}
		sp, ok := removalSpan(key, spec, lang, src)
		if !ok {
			continue
		}
		if edits[f.File] == nil {
			edits[f.File] = &fileEdit{spec: spec}
		}
		edits[f.File].spans = append(edits[f.File].spans, sp)
	}

	var files []string
	for f := range edits {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		fe := edits[file]
		src, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		// Apply spans back-to-front so offsets stay valid.
		sort.Slice(fe.spans, func(i, j int) bool { return fe.spans[i].start > fe.spans[j].start })
		out := src
		ff := FixFile{Path: file}
		for _, sp := range fe.spans {
			if int(sp.end) > len(out) || sp.start >= sp.end {
				continue
			}
			out = append(append([]byte{}, out[:sp.start]...), out[sp.end:]...)
			ff.Edits = append(ff.Edits, FixEdit{Start: sp.start, End: sp.end, Label: sp.label})
		}
		ff.BytesRemoved = len(src) - len(out)
		lang := fe.spec.lang()
		tree, err := ts.NewParser(lang).Parse(out)
		if err != nil || tree.RootNode().HasError() {
			ff.Reparse = "error"
			report.AllClean = false
		} else {
			ff.Reparse = "clean"
			if apply {
				info, statErr := os.Stat(file)
				mode := os.FileMode(0o644)
				if statErr == nil {
					mode = info.Mode()
				}
				if werr := os.WriteFile(file, out, mode); werr == nil {
					ff.Written = true
				} else {
					// Record and continue: a half-applied tree must still
					// produce a complete report of what happened.
					ff.Error = werr.Error()
					report.AllClean = false
				}
			}
		}
		report.Files = append(report.Files, ff)
	}
	return report, nil
}

func ext(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i:]
	}
	return ""
}

// byteAt converts a 1-based line:col back to a byte offset.
func byteAt(src []byte, line, col int) uint32 {
	l := 1
	for i := 0; i < len(src); i++ {
		if l == line {
			return uint32(i + col - 1)
		}
		if src[i] == '\n' {
			l++
		}
	}
	return 0
}
