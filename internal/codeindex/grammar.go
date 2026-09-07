// Package codeindex indexes only local tracked source in disposable SQLite.
// It never opens a Dolt store or participates in recall, export or transfer.
package codeindex

import (
	"path"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	csharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	golang "github.com/tree-sitter/tree-sitter-go/bindings/go"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// The role sets and hooks port memhub v0.2.0 src/code_index/grammar.rs.
// See tests/golden/testdata/locate/NOTICE.md for the pinned source/license.
type grammar struct {
	language                     *ts.Language
	functions, types, namespaces []string
	transparent, items, members  []string
	comments, attributes         []string
	moduleDoc                    string
	js, py, goReceiver           bool
}

func languageFor(file string) string {
	extension := file
	if i := strings.LastIndexByte(file, '.'); i >= 0 {
		extension = file[i+1:]
	}
	switch strings.ToLower(extension) {
	case "rs":
		return "rust"
	case "cs":
		return "csharp"
	case "java":
		return "java"
	case "ts", "tsx", "mts", "cts":
		return "typescript"
	case "js", "jsx", "mjs", "cjs":
		return "javascript"
	case "py":
		return "python"
	case "go":
		return "go"
	}
	return ""
}

func indexable(file string) bool {
	return languageFor(file) != "" && !strings.Contains(path.Base(file), ".min.")
}

func grammarFor(file string) grammar {
	switch languageFor(file) {
	case "rust":
		return grammar{
			language: ts.NewLanguage(rust.Language()), functions: []string{"function_item"},
			namespaces: []string{"mod_item"},
			items:      []string{"struct_item", "enum_item", "union_item", "trait_item", "type_item", "const_item", "static_item", "macro_definition"},
			comments:   []string{"line_comment", "block_comment"}, attributes: []string{"attribute_item"}, moduleDoc: "rust",
		}
	case "csharp":
		return grammar{
			language:   ts.NewLanguage(csharp.Language()),
			types:      []string{"class_declaration", "struct_declaration", "interface_declaration", "record_declaration"},
			namespaces: []string{"namespace_declaration"}, items: []string{"enum_declaration", "delegate_declaration"},
			members:  []string{"method_declaration", "constructor_declaration", "property_declaration"},
			comments: []string{"comment"}, attributes: []string{"attribute_list"}, moduleDoc: "///",
		}
	case "java":
		return grammar{
			language: ts.NewLanguage(java.Language()), types: []string{"class_declaration", "interface_declaration", "record_declaration"},
			items: []string{"enum_declaration", "annotation_type_declaration"}, members: []string{"method_declaration", "constructor_declaration"},
			comments: []string{"line_comment", "block_comment"}, moduleDoc: "/**",
		}
	case "typescript", "javascript":
		g := grammar{
			language: ts.NewLanguage(javascript.Language()), functions: []string{"function_declaration", "generator_function_declaration"},
			types: []string{"class_declaration"}, transparent: []string{"export_statement"}, members: []string{"method_definition"},
			comments: []string{"comment"}, attributes: []string{"decorator"}, moduleDoc: "/**", js: true,
		}
		if languageFor(file) == "typescript" {
			g.language = ts.NewLanguage(typescript.LanguageTypescript())
			if strings.EqualFold(path.Ext(file), ".tsx") {
				g.language = ts.NewLanguage(typescript.LanguageTSX())
			}
			g.types = append(g.types, "abstract_class_declaration", "interface_declaration")
			g.namespaces = []string{"internal_module"}
			g.transparent = append(g.transparent, "expression_statement")
			g.items = []string{"type_alias_declaration", "enum_declaration"}
			g.members = append(g.members, "method_signature")
		}
		return g
	case "python":
		return grammar{
			language: ts.NewLanguage(python.Language()), functions: []string{"function_definition"}, types: []string{"class_definition"},
			transparent: []string{"decorated_definition"}, comments: []string{"comment"}, moduleDoc: "python", py: true,
		}
	case "go":
		return grammar{
			language: ts.NewLanguage(golang.Language()), functions: []string{"function_declaration", "method_declaration"},
			transparent: []string{"type_declaration", "const_declaration", "var_declaration", "var_spec_list"},
			items:       []string{"type_spec", "const_spec", "var_spec"}, comments: []string{"comment"}, moduleDoc: "go", goReceiver: true,
		}
	}
	return grammar{}
}
