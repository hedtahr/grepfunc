package grepfunc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
)

// goSymbols extracts Go symbols with the standard parser, which is exact about
// boundaries where the brace scanner is heuristic (generics, composite literals,
// raw strings, Allman style). Unparseable files fall back to the scanner.
type goSymbols struct {
	fset     *token.FileSet
	lines    [][]byte
	pattern  *regexp.Regexp
	sigFn    func([]byte) bool
	limit    int
	needBody bool
	seen     map[int]bool
	results  []FuncMatch
}

// parseGoSymbols returns Go symbols for data. The bool reports whether the file
// parsed, so the caller can fall back to the brace scanner when it did not.
func parseGoSymbols(filePath string, data []byte, pattern *regexp.Regexp, limit int,
	sigFn func([]byte) bool, needBody bool) ([]FuncMatch, bool) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, filePath, data, parser.SkipObjectResolution)
	if err != nil || file == nil {
		return nil, false
	}

	collector := &goSymbols{
		fset:     fset,
		lines:    toLines(data),
		pattern:  pattern,
		sigFn:    sigFn,
		limit:    limit,
		needBody: needBody,
		seen:     make(map[int]bool, 8),
	}

	collector.walkFile(file)

	return collector.results, true
}

func (g *goSymbols) walkFile(file *ast.File) {
	for _, decl := range file.Decls {
		if g.full() {
			return
		}

		switch typed := decl.(type) {
		case *ast.FuncDecl:
			g.add(typed, typed.Name.Name, typed.Pos(), typed.End())
			g.walkLiterals(typed.Body)
		case *ast.GenDecl:
			g.walkGenDecl(typed)
		}
	}
}

func (g *goSymbols) walkGenDecl(decl *ast.GenDecl) {
	if decl.Tok != token.TYPE {
		return
	}

	for _, spec := range decl.Specs {
		typeSpec, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}

		g.add(typeSpec, typeSpec.Name.Name, typeSpec.Pos(), typeSpec.End())
	}
}

// walkLiterals records assigned closures, which the brace scanner also reports.
func (g *goSymbols) walkLiterals(body *ast.BlockStmt) {
	if body == nil {
		return
	}

	ast.Inspect(body, func(node ast.Node) bool {
		if g.full() {
			return false
		}

		lit, ok := node.(*ast.FuncLit)
		if !ok {
			return true
		}

		startLine := g.fset.Position(lit.Pos()).Line
		if name := g.lineName(startLine); name != "" {
			g.add(lit, name, lit.Pos(), lit.End())
		}

		return true
	})
}

// lineName names a closure the way the brace scanner does, from its own line.
func (g *goSymbols) lineName(line int) string {
	if line < 1 || line > len(g.lines) {
		return ""
	}

	return extractFuncName(string(g.lines[line-1]))
}

func (g *goSymbols) add(node ast.Node, name string, pos, end token.Pos) {
	startLine := g.fset.Position(pos).Line
	endLine := g.fset.Position(end).Line

	if startLine < 1 || startLine > len(g.lines) || g.seen[startLine] {
		return
	}

	// Same filter the brace scanner applies, so func/type selection is identical.
	if !g.sigFn(g.lines[startLine-1]) {
		return
	}

	if !g.matches(startLine, endLine) {
		return
	}

	g.seen[startLine] = true

	body := ""
	if g.needBody {
		body = joinBlock(g.lines, startLine-1, g.blockEnd(endLine)-1)
	}

	g.results = append(g.results, FuncMatch{
		Line:    startLine,
		EndLine: endLine,
		Name:    name,
		Lines:   endLine - startLine + 1,
		Body:    body,
	})
}

// matches scans the symbol's lines, falling back to whole-symbol matching for
// patterns that can span a line break.
func (g *goSymbols) matches(startLine, endLine int) bool {
	end := g.blockEnd(endLine)

	for lineIdx := startLine - 1; lineIdx < end; lineIdx++ {
		if g.pattern.Match(g.lines[lineIdx]) {
			return true
		}
	}

	if canMatchNewline(g.pattern) {
		return g.pattern.MatchString(joinBlock(g.lines, startLine-1, end-1))
	}

	return false
}

// blockEnd clamps a 1-based end line to the lines that exist.
func (g *goSymbols) blockEnd(endLine int) int {
	return min(max(endLine, 1), len(g.lines))
}

func (g *goSymbols) full() bool {
	return len(g.results) >= g.limit
}
