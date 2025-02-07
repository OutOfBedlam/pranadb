package parplan

import (
	"sort"

	pc_parser "github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/charset"
	"github.com/squareup/pranadb/errors"
	driver "github.com/squareup/pranadb/tidb/types/parser_driver"
)

func NewParser() *Parser {
	p := pc_parser.New()
	p.SetStrictDoubleTypeCheck(false)
	p.EnableWindowFunc(false)
	return &Parser{p}
}

type Parser struct {
	parser *pc_parser.Parser
}

func (p *Parser) Parse(sql string) (stmt AstHandle, err error) {
	stmtNodes, warns, err := p.parser.Parse(sql, charset.CharsetUTF8, "")
	if err != nil {
		return AstHandle{}, errors.WithStack(err)
	}
	if warns != nil {
		for _, warn := range warns {
			println(warn)
		}
	}
	if len(stmtNodes) != 1 {
		return AstHandle{}, errors.Errorf("expected 1 statement got %d", len(stmtNodes))
	}

	// We gather the param marker expressions then sort them in order of where they appear in the original sql
	// as they may be visited in a different order.
	// We then set the order property on them
	stmtNode := stmtNodes[0]
	vis := &pmVisitor{}
	stmtNode.Accept(vis)
	pms := vis.pms
	sorter := &pmSorter{pms: pms}
	sort.Sort(sorter)
	for i, pme := range pms {
		pme.SetOrder(i)
	}

	return AstHandle{stmt: stmtNode}, nil
}

// AstHandle wraps the underlying TiDB ast, to avoid leaking the TiDB too much into the rest of the code
type AstHandle struct {
	stmt ast.StmtNode
}

type pmVisitor struct {
	pms []ast.ParamMarkerExpr
}

func (p *pmVisitor) Enter(in ast.Node) (ast.Node, bool) {
	return in, false
}

func (p *pmVisitor) Leave(in ast.Node) (ast.Node, bool) {
	pm, ok := in.(*driver.ParamMarkerExpr)
	if ok {
		p.pms = append(p.pms, pm)
	}
	return in, true
}

type pmSorter struct {
	pms []ast.ParamMarkerExpr
}

func (ps *pmSorter) Len() int {
	return len(ps.pms)
}

func (ps *pmSorter) Less(i, j int) bool {
	return ps.pms[i].(*driver.ParamMarkerExpr).Offset < ps.pms[j].(*driver.ParamMarkerExpr).Offset
}

func (ps *pmSorter) Swap(i, j int) {
	ps.pms[i], ps.pms[j] = ps.pms[j], ps.pms[i]
}
