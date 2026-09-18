package api

import (
	"strings"
)

// BlockSpec is one block to create from Markdown source.
type BlockSpec struct {
	Markdown string
	Props    map[string]string
	Children []*BlockSpec
	SrcLine  int
}

// Splitter turns Markdown into block specs following the page format rules
// (specification section 9). The Markdown engine provides the full
// implementation; NaiveSplitter covers the structural rules without parsing
// inline syntax.
type Splitter interface {
	// Split splits src; outliner selects the list-item rule.
	Split(src string, outliner bool) []*BlockSpec
	// Join is the inverse used by Markdown reads and exports.
	Join(blocks []*BlockSpec, outliner bool) string
}

// NaiveSplitter implements the page-format rules on lines alone: in outliner
// format every `- ` list item (by indentation) is a block; in markdown format
// paragraphs separated by blank lines are blocks and headings nest what
// follows them by level.
type NaiveSplitter struct{}

// Split implements Splitter.
func (NaiveSplitter) Split(src string, outliner bool) []*BlockSpec {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if strings.TrimSpace(src) == "" {
		return nil
	}
	if outliner {
		return splitOutliner(src)
	}
	return splitMarkdown(src)
}

type stackItem struct {
	indent int
	spec   *BlockSpec
}

func splitOutliner(src string) []*BlockSpec {
	var roots []*BlockSpec
	var stack []stackItem
	var current *BlockSpec
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		isItem := strings.HasPrefix(trimmed, "- ") || trimmed == "-" || strings.HasPrefix(trimmed, "* ")
		if !isItem {
			if current != nil && strings.TrimSpace(line) != "" {
				// Continuation content belongs to the current item.
				current.Markdown += "\n" + strings.TrimSpace(line)
			} else if current == nil && strings.TrimSpace(line) != "" {
				spec := &BlockSpec{Markdown: strings.TrimSpace(line), SrcLine: i + 1}
				roots = append(roots, spec)
			}
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "* "))
		if trimmed == "-" {
			text = ""
		}
		spec := &BlockSpec{Markdown: text, SrcLine: i + 1}
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			roots = append(roots, spec)
		} else {
			parent := stack[len(stack)-1].spec
			parent.Children = append(parent.Children, spec)
		}
		stack = append(stack, stackItem{indent: indent, spec: spec})
		current = spec
	}
	return roots
}

func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

func splitMarkdown(src string) []*BlockSpec {
	// Paragraph blocks: consecutive non-blank lines; fenced code blocks stay whole.
	var chunks []*BlockSpec
	lines := strings.Split(src, "\n")
	var buf []string
	start := 0
	inFence := false
	flush := func() {
		if len(buf) == 0 {
			return
		}
		chunks = append(chunks, &BlockSpec{Markdown: strings.Join(buf, "\n"), SrcLine: start + 1})
		buf = nil
	}
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inFence {
				flush()
				start = i
			}
			inFence = !inFence
			buf = append(buf, line)
			if !inFence {
				flush()
			}
			continue
		}
		if inFence {
			buf = append(buf, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if len(buf) == 0 {
			start = i
		}
		if headingLevel(strings.TrimSpace(line)) > 0 {
			flush()
			start = i
			chunks = append(chunks, &BlockSpec{Markdown: strings.TrimSpace(line), SrcLine: i + 1})
			continue
		}
		buf = append(buf, line)
	}
	flush()
	// Nest by heading level.
	var roots []*BlockSpec
	type frame struct {
		level int
		spec  *BlockSpec
	}
	var stack []frame
	for _, ch := range chunks {
		lvl := headingLevel(ch.Markdown)
		if lvl > 0 {
			for len(stack) > 0 && stack[len(stack)-1].level >= lvl {
				stack = stack[:len(stack)-1]
			}
		}
		if len(stack) == 0 {
			roots = append(roots, ch)
		} else {
			p := stack[len(stack)-1].spec
			p.Children = append(p.Children, ch)
		}
		if lvl > 0 {
			stack = append(stack, frame{level: lvl, spec: ch})
		}
	}
	return roots
}

// Join implements Splitter.
func (NaiveSplitter) Join(blocks []*BlockSpec, outliner bool) string {
	var b strings.Builder
	if outliner {
		var walk func(specs []*BlockSpec, depth int)
		walk = func(specs []*BlockSpec, depth int) {
			for _, s := range specs {
				indent := strings.Repeat("  ", depth)
				lines := strings.Split(s.Markdown, "\n")
				b.WriteString(indent + "- " + lines[0] + "\n")
				for _, l := range lines[1:] {
					b.WriteString(indent + "  " + l + "\n")
				}
				walk(s.Children, depth+1)
			}
		}
		walk(blocks, 0)
		return b.String()
	}
	var walk func(specs []*BlockSpec)
	walk = func(specs []*BlockSpec) {
		for _, s := range specs {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s.Markdown + "\n")
			walk(s.Children)
		}
	}
	walk(blocks)
	return b.String()
}
