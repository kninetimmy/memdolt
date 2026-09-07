package localdolt

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ported from memhub v0.2.0 src/commands/doc.rs. These are its observable
// rules, including family-only fence closing and an unbounded single paragraph
// or fence. This is deliberately not a CommonMark parser.
const maxDocumentChunkChars = 2000

type documentSection struct {
	heading string
	body    string
}

func chunkMarkdown(content string) []documentSection {
	type heading struct {
		level int
		text  string
	}
	var stack []heading
	var sections []documentSection
	var body []string
	path := ""
	var fence byte
	flush := func() {
		joined := strings.Join(body, "\n")
		if strings.TrimSpace(joined) != "" {
			sections = append(sections, documentSection{path, joined})
		}
	}
	for _, line := range markdownLines(content) {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		if fence != 0 {
			body = append(body, line)
			if fenceToken(trimmed) == fence {
				fence = 0
			}
			continue
		}
		if marker := fenceToken(trimmed); marker != 0 {
			fence = marker
			body = append(body, line)
			continue
		}
		level, text := markdownHeading(trimmed)
		if level == 0 {
			body = append(body, line)
			continue
		}
		flush()
		body = []string{line}
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, heading{level, text})
		parts := make([]string, len(stack))
		for i, h := range stack {
			parts[i] = h.text
		}
		path = strings.Join(parts, " > ")
	}
	flush()
	chunks := []documentSection{}
	for _, section := range sections {
		if utf8.RuneCountInString(section.body) <= maxDocumentChunkChars {
			chunks = append(chunks, documentSection{section.heading, strings.TrimSpace(section.body)})
			continue
		}
		for _, piece := range splitDocumentSection(section.body) {
			chunks = append(chunks, documentSection{section.heading, piece})
		}
	}
	return chunks
}

// Rust str::lines removes LF or CRLF, but retains a bare CR and does not
// produce a trailing empty line. Do not normalize the original hash input.
func markdownLines(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		if strings.HasSuffix(line, "\n") {
			lines[i] = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		}
	}
	return lines
}

func splitDocumentSection(body string) []string {
	var blocks, current []string
	var fence byte
	for _, line := range markdownLines(body) {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		if fence != 0 {
			current = append(current, line)
			if fenceToken(trimmed) == fence {
				fence = 0
			}
		} else if marker := fenceToken(trimmed); marker != 0 {
			fence = marker
			current = append(current, line)
		} else if strings.TrimSpace(line) == "" {
			if len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
			}
		} else {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	var pieces []string
	acc := ""
	for _, block := range blocks {
		if acc != "" && utf8.RuneCountInString(acc)+utf8.RuneCountInString(block)+2 > maxDocumentChunkChars {
			pieces = append(pieces, acc)
			acc = ""
		}
		if acc != "" {
			acc += "\n\n"
		}
		acc += block
	}
	if strings.TrimSpace(acc) != "" {
		pieces = append(pieces, acc)
	}
	return pieces
}

func markdownHeading(line string) (int, string) {
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 || level == len(line) {
		return 0, ""
	}
	r, _ := utf8.DecodeRuneInString(line[level:])
	if !unicode.IsSpace(r) {
		return 0, ""
	}
	text := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[level:]), "#"))
	if text == "" {
		return 0, ""
	}
	return level, text
}

func fenceToken(line string) byte {
	if strings.HasPrefix(line, "```") {
		return '`'
	}
	if strings.HasPrefix(line, "~~~") {
		return '~'
	}
	return 0
}

func documentTitle(chunks []documentSection, path string) string {
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.heading) != "" {
			parts := strings.Split(chunk.heading, " > ")
			return parts[len(parts)-1]
		}
	}
	return filepath.Base(path)
}
