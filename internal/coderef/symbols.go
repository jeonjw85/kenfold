package coderef

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	maxParseBytes = 1 << 20
	parseTimeout  = 2 * time.Second
)

// definitionDeny lists node-type fragments whose "name" field is a use of a
// name rather than a definition (calls, JSX tags, keyword arguments, ...).
var definitionDeny = []string{"call", "invocation", "expression", "access", "argument", "reference", "import",
	"parameter", "annotation", "decorator", "attribute", "jsx", "element", "pattern", "label", "use_", "tag"}

var (
	// errUnsupported reports a file whose language this build cannot parse
	// (or that is too large to parse).
	errUnsupported = errors.New("language not supported")
	// errIncomplete reports a symbol that was not found in a file that did
	// not parse cleanly: it may exist in the part tree-sitter could not read.
	errIncomplete = errors.New("file did not parse cleanly")
)

// definitions returns the source text of the definitions of symbol in src, in
// file order. symbol may be qualified ("Server.Dedupe", "Model::fit",
// "Api#fetch"): the last part is the defined name, and the qualifier must
// name an enclosing definition (a class) or appear before the name in the
// definition (a Go method receiver). errUnsupported means the file's
// language is not available.
func definitions(path string, src []byte, symbol string) (defs []string, err error) {
	if len(src) > maxParseBytes {
		return nil, errUnsupported
	}
	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return nil, errUnsupported
	}
	// The parser is a young pure-Go runtime; a panic on odd input must not
	// take the hook or the CLI down.
	defer func() {
		if r := recover(); r != nil {
			defs, err = nil, fmt.Errorf("parse %s: %v", path, r)
		}
	}()
	lang := entry.Language()
	if lang == nil {
		return nil, errUnsupported // grammar not embedded in this build
	}
	p := ts.NewParser(lang)
	p.SetTimeoutMicros(uint64(parseTimeout / time.Microsecond))
	tree, err := p.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if tree == nil || tree.RootNode() == nil {
		return nil, fmt.Errorf("parse %s: no tree", path)
	}
	defer tree.Release()

	qualifier, name := splitSymbol(symbol)
	var found []*ts.Node
	stack := []*ts.Node{tree.RootNode()}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		children := n.Children()
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
		for i, c := range children {
			def := definitionOf(n, c, n.FieldNameForChild(i, lang), lang, src, name)
			if def != nil && (qualifier == "" || qualified(def, c, qualifier, lang, src)) && !slices.Contains(found, def) {
				found = append(found, def)
			}
		}
	}
	if len(found) == 0 && tree.RootNode().HasError() {
		return nil, errIncomplete
	}
	slices.SortFunc(found, func(a, b *ts.Node) int { return int(a.StartByte()) - int(b.StartByte()) })
	for _, d := range found {
		defs = append(defs, d.Text(src))
	}
	return defs, nil
}

// definitionOf returns the definition node when child c (in field of parent
// n) is the defined name, or nil.
func definitionOf(n, c *ts.Node, field string, lang *ts.Language, src []byte, name string) *ts.Node {
	if c.ChildCount() > 0 || c.Text(src) != name {
		return nil
	}
	typ := n.Type(lang)
	switch field {
	case "name":
		if denied(typ) {
			return nil
		}
		return n
	case "left": // NAME = value (Python, Ruby constants)
		if strings.Contains(typ, "assignment") {
			return n
		}
	case "declarator": // C/C++: int name(...) { } — climb to the definition
		if !strings.Contains(typ, "declarator") && !strings.HasSuffix(typ, "declaration") && !strings.HasSuffix(typ, "definition") {
			return nil
		}
		for d := n; d != nil; d = d.Parent() {
			if t := d.Type(lang); strings.HasSuffix(t, "definition") || strings.HasSuffix(t, "declaration") {
				return d
			}
		}
	}
	return nil
}

func denied(typ string) bool {
	return slices.ContainsFunc(definitionDeny, func(d string) bool { return strings.Contains(typ, d) })
}

// qualified reports whether def (defining nameNode) belongs to qualifier: an
// enclosing definition is named qualifier (class Api, impl Config), or
// qualifier appears as a word in the definition before the name (func (s
// *Server) Dedupe).
func qualified(def, nameNode *ts.Node, qualifier string, lang *ts.Language, src []byte) bool {
	for a := def.Parent(); a != nil; a = a.Parent() {
		for _, f := range []string{"name", "type"} {
			if nn := a.ChildByFieldName(f, lang); nn != nil && nn.Text(src) == qualifier {
				return true
			}
		}
	}
	prefix := string(src[def.StartByte():nameNode.StartByte()])
	return slices.Contains(strings.FieldsFunc(prefix, func(r rune) bool { return !isIdentRune(r) }), qualifier)
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '$' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || r > 127
}

// splitSymbol splits "A.B", "A::B", or "A#B" into qualifier and name.
func splitSymbol(s string) (qualifier, name string) {
	s = strings.TrimSuffix(s, "()")
	for _, sep := range []string{"::", "#", "."} {
		if i := strings.LastIndex(s, sep); i >= 0 {
			q := s[:i]
			if j := strings.LastIndexAny(q, ".:#"); j >= 0 {
				q = q[j+1:]
			}
			return q, s[i+len(sep):]
		}
	}
	return "", s
}

// parsable reports whether this build can parse path's language.
func parsable(path string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	e := grammars.DetectLanguage(path)
	return e != nil && e.Language() != nil
}
