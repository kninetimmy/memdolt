package codeindex

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	ts "github.com/tree-sitter/go-tree-sitter"
)

const (
	windowLines   = 50
	maxChunkBytes = 4000
)

// Chunk retains the tagged AST line ranges and LF-normalized embedding input.
// Bodies are internal index data; locator responses contain only breadcrumbs.
type Chunk struct {
	StartLine, EndLine int
	Symbol             *string
	Kind, EmbedText    string
	Body               string
}

// ChunkFile ports the tagged top-level walker. A successful parse with no
// recognized items falls back to windows; native failures are visible errors.
func ChunkFile(ctx context.Context, file, content string) ([]Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(content) {
		return nil, errors.New("code chunker requires UTF-8")
	}
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	g := grammarFor(file)
	if g.language == nil {
		return lineWindows(file, content), nil
	}
	parser := ts.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(g.language); err != nil {
		return nil, fmt.Errorf("load %s tree-sitter grammar: %w", languageFor(file), err)
	}
	// Parse normalized bytes too: Go comment nodes can end between CR and LF,
	// where normalizing only the sliced node would leave a stray CR.
	src := []byte(strings.ReplaceAll(content, "\r\n", "\n"))
	// This binding releases its input callback registration, but leaks a
	// non-nil ParseOptions registration. Keep options nil. Cancellation is
	// checked at input boundaries, not periodically inside native parsing.
	tree := parser.ParseWithOptions(func(offset int, _ ts.Point) []byte {
		if ctx.Err() != nil || offset >= len(src) {
			return nil
		}
		return src[offset:]
	}, nil, nil)
	if tree == nil {
		return nil, errors.Join(errors.New("tree-sitter did not produce a tree"), ctx.Err())
	}
	defer tree.Close()
	if err := ctx.Err(); err != nil {
		return nil, err // A canceled input callback can still produce a tree.
	}
	w := chunkWalker{file: file, src: src, grammar: g}
	w.moduleDoc(tree.RootNode())
	w.collect(tree.RootNode(), "")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(w.chunks) == 0 {
		return lineWindows(file, content), nil
	}
	return w.chunks, nil
}

type chunkWalker struct {
	file string
	src  []byte
	grammar
	chunks []Chunk
}

func namedChildren(n *ts.Node) []ts.Node {
	cursor := n.Walk()
	defer cursor.Close()
	return n.NamedChildren(cursor)
}

func (w *chunkWalker) collect(parent *ts.Node, prefix string) {
	for _, n := range namedChildren(parent) {
		kind := n.Kind()
		switch {
		case slices.Contains(w.functions, kind):
			label := "function"
			p := prefix
			if w.goReceiver {
				p = w.receiver(&n)
			}
			if p != "" {
				label = "method"
			}
			w.push(&n, qualify(p, w.field(&n, "name")), label, false)
		case kind == "impl_item" && w.moduleDocKind() == "rust":
			if body := n.ChildByFieldName("body"); body != nil {
				w.collect(body, w.field(&n, "type"))
			}
		case slices.Contains(w.types, kind):
			symbol := qualify(prefix, w.field(&n, "name"))
			w.push(&n, symbol, kindLabel(kind), true)
			if body := n.ChildByFieldName("body"); body != nil {
				w.collect(body, symbol)
			}
		case slices.Contains(w.namespaces, kind):
			if body := n.ChildByFieldName("body"); body != nil {
				w.collect(body, prefix)
			}
		case slices.Contains(w.transparent, kind):
			w.collect(&n, prefix)
		case slices.Contains(w.items, kind), prefix != "" && slices.Contains(w.members, kind):
			w.push(&n, qualify(prefix, w.field(&n, "name")), kindLabel(kind), false)
		case w.js && (kind == "lexical_declaration" || kind == "variable_declaration"):
			declarators := []ts.Node{}
			for _, d := range namedChildren(&n) {
				if d.Kind() == "variable_declarator" {
					declarators = append(declarators, d)
				}
			}
			for _, d := range declarators {
				if !functionValued(&d) {
					continue
				}
				item := &d
				if len(declarators) == 1 {
					item = &n
				}
				label := "function"
				if prefix != "" {
					label = "method"
				}
				w.push(item, qualify(prefix, w.field(&d, "name")), label, false)
			}
		case w.js && prefix != "" && (kind == "public_field_definition" || kind == "field_definition") && functionValued(&n):
			name := w.field(&n, "name")
			if name == "" {
				name = w.field(&n, "property")
			}
			w.push(&n, qualify(prefix, name), "method", false)
		}
	}
}

func functionValued(n *ts.Node) bool {
	v := n.ChildByFieldName("value")
	return v != nil && slices.Contains([]string{"arrow_function", "function", "function_expression", "generator_function"}, v.Kind())
}

func qualify(prefix, name string) string {
	if name != "" && prefix != "" {
		return prefix + "::" + name
	}
	return name
}

func (w *chunkWalker) field(n *ts.Node, field string) string {
	if child := n.ChildByFieldName(field); child != nil {
		return child.Utf8Text(w.src)
	}
	return ""
}

func (w *chunkWalker) receiver(n *ts.Node) string {
	receiver := n.ChildByFieldName("receiver")
	if receiver == nil {
		return ""
	}
	for _, param := range namedChildren(receiver) {
		if param.Kind() != "parameter_declaration" {
			continue
		}
		ty := param.ChildByFieldName("type")
		if ty != nil && ty.Kind() == "pointer_type" {
			ty = ty.NamedChild(0)
		}
		if ty != nil && ty.Kind() == "generic_type" {
			ty = ty.ChildByFieldName("type")
		}
		if ty != nil {
			return ty.Utf8Text(w.src)
		}
		break
	}
	return ""
}

func (w *chunkWalker) leading(n *ts.Node) *ts.Node {
	for parent := n.Parent(); parent != nil && slices.Contains(w.transparent, parent.Kind()); parent = n.Parent() {
		n = parent
	}
	if w.py {
		return n // Python docs are body strings; # comments are not folded.
	}
	for prev := n.PrevSibling(); prev != nil; prev = n.PrevSibling() {
		if !slices.Contains(w.comments, prev.Kind()) && !slices.Contains(w.attributes, prev.Kind()) {
			break
		}
		if int(n.StartPosition().Row)-int(prev.EndPosition().Row) > 1 {
			break
		}
		n = prev
	}
	return n
}

func (w *chunkWalker) push(n *ts.Node, symbol, kind string, header bool) {
	start := w.leading(n)
	body := string(w.src[start.StartByte():n.EndByte()])
	if header {
		var excise [][2]uint
		if b := n.ChildByFieldName("body"); b != nil {
			for _, member := range namedChildren(b) {
				w.excisions(&member, &excise)
			}
		}
		var out strings.Builder
		pos := start.StartByte()
		for _, span := range excise {
			if span[0] < pos || span[1] > n.EndByte() {
				continue
			}
			out.Write(w.src[pos:span[0]])
			out.WriteString("{ ... }")
			pos = span[1]
		}
		out.Write(w.src[pos:n.EndByte()])
		body = out.String()
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	name := symbol
	var sym *string
	if name == "" {
		name = kind
	} else {
		sym = &symbol
	}
	w.chunks = append(w.chunks, Chunk{
		StartLine: int(start.StartPosition().Row) + 1, EndLine: int(n.EndPosition().Row) + 1,
		Symbol: sym, Kind: kind, Body: body, EmbedText: w.file + "\n" + kind + " " + name + "\n\n" + body,
	})
}

func (w *chunkWalker) excisions(n *ts.Node, spans *[][2]uint) {
	if slices.Contains(w.transparent, n.Kind()) {
		for _, inner := range namedChildren(n) {
			w.excisions(&inner, spans)
		}
	} else if body := n.ChildByFieldName("body"); body != nil {
		*spans = append(*spans, [2]uint{body.StartByte(), body.EndByte()})
	}
}

func kindLabel(kind string) string {
	for _, suffix := range []string{"_item", "_definition", "_declaration", "_signature", "_spec"} {
		if strings.HasSuffix(kind, suffix) {
			return strings.TrimSuffix(kind, suffix)
		}
	}
	return kind
}

func (w *chunkWalker) moduleDocKind() string { return w.grammar.moduleDoc }

func (w *chunkWalker) moduleDoc(root *ts.Node) {
	children := namedChildren(root)
	if len(children) == 0 {
		return
	}
	var first, last *ts.Node
	switch w.moduleDocKind() {
	case "python":
		n := &children[0]
		if inner := n.NamedChild(0); n.Kind() == "expression_statement" && inner != nil && inner.Kind() == "string" {
			first, last = n, n
		}
	case "go":
		for i, n := range children {
			if n.Kind() != "package_clause" {
				continue
			}
			next := &n
			for j := i - 1; j >= 0; j-- {
				prev := &children[j]
				if !slices.Contains(w.comments, prev.Kind()) || int(next.StartPosition().Row)-int(prev.EndPosition().Row) > 1 {
					break
				}
				if last == nil {
					last = prev
				}
				first, next = prev, prev
			}
			break
		}
	default:
		for _, n := range children {
			text := n.Utf8Text(w.src)
			marker := strings.HasPrefix(text, w.moduleDocKind())
			if w.moduleDocKind() == "rust" {
				marker = strings.HasPrefix(text, "//!") || strings.HasPrefix(text, "/*!")
			}
			if !slices.Contains(w.comments, n.Kind()) || !marker || last != nil && int(n.StartPosition().Row)-int(last.EndPosition().Row) > 1 {
				if last != nil && w.moduleDocKind() != "rust" && int(n.StartPosition().Row)-int(last.EndPosition().Row) <= 1 && w.declaration(n.Kind()) {
					return // An adjacent declaration owns its leading documentation.
				}
				break
			}
			if first == nil {
				first = &n
			}
			last = &n
		}
	}
	if first != nil {
		body := strings.ReplaceAll(string(w.src[first.StartByte():last.EndByte()]), "\r\n", "\n")
		w.chunks = append(w.chunks, Chunk{StartLine: int(first.StartPosition().Row) + 1, EndLine: int(last.EndPosition().Row) + 1,
			Kind: "module-doc", Body: body, EmbedText: w.file + "\nmodule-doc\n\n" + body})
	}
}

func (w *chunkWalker) declaration(kind string) bool {
	return slices.Contains(w.functions, kind) || slices.Contains(w.types, kind) || slices.Contains(w.items, kind) || slices.Contains(w.members, kind) || slices.Contains(w.transparent, kind)
}

// Rust str::lines strips CR only when followed by LF, and omits the trailing
// empty line. Share that exact rule with windows and disk snippets.
func sourceLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

func lineWindows(file, content string) []Chunk {
	lines := sourceLines(content)
	var out []Chunk
	push := func(start, end int, body string) {
		out = append(out, Chunk{StartLine: start, EndLine: end, Kind: "line-window", Body: body, EmbedText: file + "\n\n" + body})
	}
	for start := 0; start < len(lines); {
		end, size := start, 0
		for end < len(lines) && end-start < windowLines {
			n := len(lines[end]) + 1
			if end > start && size+n > maxChunkBytes {
				break
			}
			size += n
			end++
		}
		body := strings.Join(lines[start:end], "\n")
		if end == start+1 && len(body) > maxChunkBytes {
			for len(body) > 0 {
				n := min(len(body), maxChunkBytes)
				for n < len(body) && !utf8.RuneStart(body[n]) {
					n--
				}
				push(start+1, start+1, body[:n])
				body = body[n:]
			}
		} else {
			push(start+1, end, body)
		}
		start = end
	}
	return out
}
