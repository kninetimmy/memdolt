package codeindex

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAllSevenASTsPreserveContainersReceiversDocumentationAndLF(t *testing.T) {
	for _, tt := range []struct {
		file, source            string
		symbols                 []string
		header, present, absent string
	}{
		{"src/a.rs", "//! Module purpose.\n\n/// A point.\n#[derive(Debug)]\nstruct Point { x: i32 }\ntrait Greet { fn greet(&self); }\nenum E { A }\nunion U { a: i32 }\ntype Alias = i32;\nconst MAX: usize = 1;\nstatic VALUE: i32 = 2;\nmacro_rules! m { () => {} }\nimpl Point {\n /// method docs\n fn magnitude(&self) -> i32 { self.x }\n}\nmod nested { fn helper() {} }\n",
			[]string{"module-doc:", "struct:Point", "trait:Greet", "enum:E", "union:U", "type:Alias", "const:MAX", "static:VALUE", "macro:m", "method:Point::magnitude", "function:helper"}, "Point", "/// A point.\n#[derive(Debug)]", ""},
		{"src/A.cs", "/// File purpose.\nusing System;\nnamespace X {\n /// Class docs\n public class A {\n  public A() { int constructorBody = 1; }\n  /// Member docs\n  public void Run() { int methodBody = 2; }\n  public int Value { get; set; }\n  public class Inner { public void Nested() {} }\n }\n record R(int Value);\n enum E { One }\n delegate void D();\n}\n",
			[]string{"module-doc:", "class:A", "constructor:A::A", "method:A::Run", "property:A::Value", "class:A::Inner", "method:A::Inner::Nested", "record:R", "enum:E", "delegate:D"}, "A", "public int Value { get; set; }", "methodBody"},
		{"src/A.java", "/** File purpose. */\npackage app;\n/** Class docs. */\nclass A {\n A() { int constructorBody = 1; }\n /** Method docs. */\n @Deprecated public void run() { int methodBody = 2; }\n class Inner { void nested() {} }\n}\ninterface I { void empty(); }\nrecord R(int value) {}\nenum E { ONE }\n",
			[]string{"module-doc:", "class:A", "constructor:A::A", "method:A::run", "class:A::Inner", "method:A::Inner::nested", "interface:I", "method:I::empty", "record:R", "enum:E"}, "A", "/** Class docs. */", "methodBody"},
		{"src/a.ts", "/** File purpose. */\n\n/** Arrow docs. */\nexport const arrow = () => 1;\nexport function free() {}\nexport interface I { run(): void; }\nexport class A {\n field = () => 2;\n run() { const methodBody = 1; }\n}\nnamespace X { export function inner() {} }\ntype Alias = string;\nenum E { One }\nlet data = 1, fn = function() {};\n",
			[]string{"module-doc:", "function:arrow", "function:free", "interface:I", "method:I::run", "class:A", "method:A::field", "method:A::run", "function:inner", "type_alias:Alias", "enum:E", "function:fn"}, "A", "field = () => 2;", "methodBody"},
		{"src/a.jsx", "/** File purpose. */\n\n/** Arrow docs. */\nexport const arrow = () => <div />;\nexport function free() {}\nexport class A {\n field = () => 2;\n run() { const methodBody = 1; }\n}\nlet data = 1, fn = function() {};\n",
			[]string{"module-doc:", "function:arrow", "function:free", "class:A", "method:A::field", "method:A::run", "function:fn"}, "A", "field = () => 2;", "methodBody"},
		{"src/a.py", "\"\"\"File purpose.\"\"\"\n\n# Not documentation.\n@decorate\nclass A:\n    \"\"\"Class documentation.\"\"\"\n    @decorate\n    def run(self):\n        \"\"\"Method docs.\"\"\"\n        methodBody = 2\n    class Inner:\n        def nested(self):\n            pass\n\n@decorate\nasync def free():\n    def local():\n        pass\n",
			[]string{"module-doc:", "class:A", "method:A::run", "class:A::Inner", "method:A::Inner::nested", "function:free"}, "A", "@decorate\nclass A:\n    \"\"\"Class documentation.\"\"\"", "methodBody"},
		{"geo/a.go", "// License far away.\n\n// Package geo provides geometry.\npackage geo\n\n// Point docs.\ntype Point struct { X int }\nfunc (p *Point) Distance() int { return p.X }\nfunc (s *Stack[T]) Pop() T { return s.value }\nfunc Free() {}\nconst ( A = 1; B = 2 )\nvar ( C = 3; D = 4 )\n",
			[]string{"module-doc:", "type:Point", "method:Point::Distance", "method:Stack::Pop", "function:Free", "const:A", "const:B", "var:C", "var:D"}, "Point", "// Point docs.\ntype Point", ""},
	} {
		t.Run(tt.file, func(t *testing.T) {
			chunks, err := ChunkFile(context.Background(), tt.file, tt.source)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range chunks {
				symbol := ""
				if c.Symbol != nil {
					symbol = *c.Symbol
				}
				got = append(got, c.Kind+":"+symbol)
				if c.StartLine < 1 || c.EndLine < c.StartLine || !strings.HasPrefix(c.EmbedText, tt.file+"\n") {
					t.Fatalf("bad breadcrumb: %+v", c)
				}
				if symbol == tt.header {
					if !strings.Contains(c.Body, tt.present) || tt.absent != "" && strings.Contains(c.Body, tt.absent) {
						t.Fatalf("incorrect header/doc folding: %s", c.Body)
					}
				}
			}
			if !reflect.DeepEqual(got, tt.symbols) {
				t.Fatalf("AST chunks=%q, want %q", got, tt.symbols)
			}
			crlf, err := ChunkFile(context.Background(), tt.file, strings.ReplaceAll(tt.source, "\n", "\r\n"))
			if err != nil || !reflect.DeepEqual(crlf, chunks) {
				t.Fatalf("CRLF changed normalized chunks: err=%v", err)
			}
		})
	}
	// TSX is its actual grammar, not a TypeScript parse-error fallback.
	chunks, err := ChunkFile(context.Background(), "View.tsx", "export const View = () => <div title=\"x\">hello</div>;\n")
	if err != nil || len(chunks) != 1 || *chunks[0].Symbol != "View" || chunks[0].Kind != "function" {
		t.Fatalf("TSX: %+v %v", chunks, err)
	}
}

func TestFallbackCapsAndUnsplitSymbols(t *testing.T) {
	source := strings.Repeat("use alpha;\n", 51)
	chunks, err := ChunkFile(context.Background(), "a.rs", source)
	if err != nil || len(chunks) != 2 || chunks[0].StartLine != 1 || chunks[0].EndLine != 50 || chunks[1].StartLine != 51 {
		t.Fatalf("windows: %+v %v", chunks, err)
	}
	chunks, err = ChunkFile(context.Background(), "unknown.txt", strings.Repeat("界", 3000))
	if err != nil || len(chunks) != 3 {
		t.Fatalf("byte windows: %+v %v", chunks, err)
	}
	for _, c := range chunks {
		if len(c.Body) > 4000 || !utf8.ValidString(c.Body) || c.StartLine != 1 || c.EndLine != 1 {
			t.Fatalf("bad byte window: %+v", c)
		}
	}
	chunks, err = ChunkFile(context.Background(), "a.rs", "fn giant() {\n"+strings.Repeat(" let x = 0;\n", 600)+"}\n")
	if err != nil || len(chunks) != 1 || len(chunks[0].Body) <= 4000 || chunks[0].EndLine != 602 {
		t.Fatalf("AST symbol was split: chunks=%d err=%v", len(chunks), err)
	}
	if chunks, err := ChunkFile(context.Background(), "a.rs", "\r\n \t"); err != nil || len(chunks) != 0 {
		t.Fatalf("empty: %+v %v", chunks, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ChunkFile(ctx, "a.rs", "fn f() {}"); err == nil {
		t.Fatal("canceled parse succeeded")
	}
}

func TestModuleDocsGapAndBreadcrumbClipping(t *testing.T) {
	for _, file := range []string{"a.ts", "a.js", "a.java", "a.cs"} {
		doc := "/** Attached docs. */\nclass A {}"
		if file == "a.cs" {
			doc = "/// Attached docs.\nclass A {}"
		}
		chunks, err := ChunkFile(context.Background(), file, doc)
		if err != nil || len(chunks) != 1 || chunks[0].Kind == "module-doc" || chunks[0].StartLine != 1 {
			t.Fatalf("attached docs %s: %+v %v", file, chunks, err)
		}
	}
	if text := clipSnippet(sourceLines(strings.Repeat("line\n", 9)), 1, 9); strings.Count(text, "\n") != 5 || !strings.HasSuffix(text, "…") {
		t.Fatalf("line cap: %q", text)
	}
	if text := clipSnippet([]string{strings.Repeat("界", 500)}, 1, 1); utf8.RuneCountInString(text) != 400 || !strings.HasSuffix(text, "…") {
		t.Fatalf("character cap: %q", text)
	}
	if clipSnippet([]string{"a"}, 2, 1) != "" {
		t.Fatal("invalid line range was read")
	}
}
