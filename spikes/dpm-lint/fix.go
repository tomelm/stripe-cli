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

// fixDry re-runs the match per file, applies all removal spans in memory,
// reparses, and reports. Returns true if every touched file reparses clean.
func fixDry(root string, rule Rule) bool {
	type fileEdit struct {
		spec  langSpec
		spans []span
	}
	edits := map[string]*fileEdit{}

	findings, _, _ := scan(root, rule)
	for _, f := range findings {
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
			fmt.Printf("  %s:%d  NO-SPAN\n", f.File, f.Line)
			continue
		}
		if edits[f.File] == nil {
			edits[f.File] = &fileEdit{spec: spec}
		}
		edits[f.File].spans = append(edits[f.File].spans, sp)
	}

	allClean := true
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
		for _, sp := range fe.spans {
			if int(sp.end) > len(out) || sp.start >= sp.end {
				continue
			}
			out = append(append([]byte{}, out[:sp.start]...), out[sp.end:]...)
		}
		lang := fe.spec.lang()
		tree, err := ts.NewParser(lang).Parse(out)
		verdict := "REPARSE-CLEAN"
		if err != nil || tree.RootNode().HasError() {
			verdict = "REPARSE-ERROR (would revert)"
			allClean = false
		}
		if os.Getenv("FIX_DRY_SHOW") != "" {
			fmt.Printf("--- %s (edited, in memory) ---\n%s\n", file, out)
		}
		labels := map[string]int{}
		for _, sp := range fe.spans {
			labels[sp.label]++
		}
		var ls []string
		for l, n := range labels {
			ls = append(ls, fmt.Sprintf("%s×%d", l, n))
		}
		sort.Strings(ls)
		fmt.Printf("  %-38s -%d bytes  %-24s %s\n",
			file, len(src)-len(out), strings.Join(ls, ","), verdict)
	}
	return allClean
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
