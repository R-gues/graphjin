//nolint:errcheck
package psql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dosco/graphjin/core/internal/qcode"
	"github.com/dosco/graphjin/core/internal/sdata"
	"github.com/dosco/graphjin/core/internal/util"
)

const (
	closeBlock = 500
)

type Param struct {
	Name    string
	Type    string
	IsArray bool
}

type Metadata struct {
	ct     string
	poll   bool
	params []Param
	pindex map[string]int
}

type compilerContext struct {
	md *Metadata
	w  *bytes.Buffer
	qc *qcode.QCode
	*Compiler
}

type Variables map[string]json.RawMessage

type Config struct {
	Vars      map[string]string
	DBType    string
	DBVersion int
}

type Compiler struct {
	svars map[string]string
	ct    string // db type
	cv    int    // db version
}

func NewCompiler(conf Config) *Compiler {
	return &Compiler{svars: conf.Vars, ct: conf.DBType, cv: conf.DBVersion}
}

func (co *Compiler) CompileEx(qc *qcode.QCode) (Metadata, []byte, error) {
	var w bytes.Buffer

	if metad, err := co.Compile(&w, qc); err != nil {
		return metad, nil, err
	} else {
		return metad, w.Bytes(), nil
	}
}

func (co *Compiler) Compile(w *bytes.Buffer, qc *qcode.QCode) (Metadata, error) {
	var err error
	var md Metadata

	if qc == nil {
		return md, fmt.Errorf("qcode is nil")
	}

	switch qc.Type {
	case qcode.QTQuery:
		co.CompileQuery(w, qc, &md)

	case qcode.QTSubscription:
		co.CompileQuery(w, qc, &md)

	case qcode.QTMutation:
		co.compileMutation(w, qc, &md)

	default:
		err = fmt.Errorf("Unknown operation type %d", qc.Type)
	}

	return md, err
}

func (co *Compiler) CompileQuery(
	w *bytes.Buffer,
	qc *qcode.QCode,
	md *Metadata) {

	if qc.Type == qcode.QTSubscription {
		md.poll = true
	}

	md.ct = qc.Schema.Type()

	st := NewIntStack()
	c := &compilerContext{
		md:       md,
		w:        w,
		qc:       qc,
		Compiler: co,
	}

	i := 0
	switch c.ct {
	case "mysql":
		c.w.WriteString(`SELECT json_object(`)
	default:
		c.w.WriteString(`SELECT jsonb_build_object(`)
	}
	for _, id := range qc.Roots {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		sel := &qc.Selects[id]

		if sel.SkipRender == qcode.SkipTypeUserNeeded {
			c.w.WriteString(`'`)
			c.w.WriteString(sel.FieldName)
			c.w.WriteString(`', NULL`)

			if sel.Paging.Cursor {
				c.w.WriteString(`, '`)
				c.w.WriteString(sel.FieldName)
				c.w.WriteString(`_cursor', NULL`)
			}

		} else {
			c.w.WriteString(`'`)
			c.w.WriteString(sel.FieldName)
			c.w.WriteString(`', __sj_`)
			int32String(c.w, sel.ID)
			c.w.WriteString(`.json`)

			// return the cursor for the this child selector as part of the parents json
			if sel.Paging.Cursor {
				c.w.WriteString(`, '`)
				c.w.WriteString(sel.FieldName)
				c.w.WriteString(`_cursor', `)

				c.w.WriteString(`__sj_`)
				int32String(c.w, sel.ID)
				c.w.WriteString(`.__cursor`)
			}

			st.Push(sel.ID + closeBlock)
			st.Push(sel.ID)
		}
		i++
	}

	// This helps multi-root work as well as return a null json value when
	// there are no rows found.

	c.w.WriteString(`) AS __root FROM ((SELECT true)) AS __root_x`)
	c.renderQuery(st, true)
}

func (c *compilerContext) renderQuery(st *IntStack, multi bool) {
	for {
		var sel *qcode.Select
		var open bool

		if st.Len() == 0 {
			break
		}

		id := st.Pop()
		if id < closeBlock {
			sel = &c.qc.Selects[id]
			open = true
		} else {
			sel = &c.qc.Selects[(id - closeBlock)]
		}

		if open {
			if sel.Type != qcode.SelTypeUnion {
				if sel.Rel.Type != sdata.RelNone || multi {
					c.renderLateralJoin()
				}
				if sel.Rel.Type == sdata.RelRecursive {
					c.renderRecursiveCTE(sel)
				}
				c.renderPluralSelect(sel)
				c.renderSelect(sel)
			}

			for _, cid := range sel.Children {
				child := &c.qc.Selects[cid]

				if child.SkipRender != qcode.SkipTypeNone {
					continue
				}

				st.Push(child.ID + closeBlock)
				st.Push(child.ID)
			}

		} else {
			if sel.Type != qcode.SelTypeUnion {
				c.renderSelectClose(sel)
				if sel.Rel.Type != sdata.RelNone || multi {
					c.renderLateralJoinClose(sel)
				}
			}
		}
	}
}

func (c *compilerContext) renderPluralSelect(sel *qcode.Select) {
	if sel.Singular {
		return
	}
	switch c.ct {
	case "mysql":
		c.w.WriteString(`SELECT CAST(COALESCE(json_arrayagg(__sj_`)
		int32String(c.w, sel.ID)
		c.w.WriteString(`.json), '[]') AS JSON) AS json`)
	default:
		c.w.WriteString(`SELECT COALESCE(jsonb_agg(__sj_`)
		int32String(c.w, sel.ID)
		c.w.WriteString(`.json), '[]') AS json`)
	}

	// Build the cursor value string
	if sel.Paging.Cursor {
		c.w.WriteString(`, CONCAT_WS(','`)
		for i := 0; i < len(sel.OrderBy); i++ {
			c.w.WriteString(`, max(__cur_`)
			int32String(c.w, int32(i))
			c.w.WriteString(`)`)
		}
		c.w.WriteString(`) as __cursor`)
	}

	c.w.WriteString(` FROM (`)
}

func (c *compilerContext) renderSelect(sel *qcode.Select) {
	switch c.ct {
	case "mysql":
		c.w.WriteString(`SELECT json_object(`)
		c.renderJSONFields(sel)
		c.w.WriteString(`) `)
	default:
		c.w.WriteString(`SELECT to_jsonb(__sr_`)
		int32String(c.w, sel.ID)
		c.w.WriteString(`.*) `)

		// Exclude the cusor values from the the generated json object since
		// we manually use these values to build the cursor string
		// Notice the `- '__cur_` its' what excludes fields in `to_jsonb`
		if sel.Paging.Cursor {
			for i := range sel.OrderBy {
				c.w.WriteString(`- '__cur_`)
				int32String(c.w, int32(i))
				c.w.WriteString(`' `)
			}
		}
	}

	c.w.WriteString(`AS json `)

	// We manually insert the cursor values into row we're building outside
	// of the generated json object so they can be used higher up in the sql.
	if sel.Paging.Cursor {
		for i := range sel.OrderBy {
			c.w.WriteString(`, __cur_`)
			int32String(c.w, int32(i))
			c.w.WriteString(` `)
		}
	}

	c.w.WriteString(`FROM (SELECT `)
	c.renderColumns(sel)

	// This is how we get the values to use to build the cursor.
	if sel.Paging.Cursor {
		for i, ob := range sel.OrderBy {
			c.w.WriteString(`, LAST_VALUE(`)
			colWithTableID(c.w, sel.Table, sel.ID, ob.Col.Name)
			c.w.WriteString(`) OVER() AS __cur_`)
			int32String(c.w, int32(i))
		}
	}

	c.w.WriteString(` FROM (`)
	c.renderBaseSelect(sel)
	c.w.WriteString(`)`)
	aliasWithID(c.w, sel.Table, sel.ID)
}

func (c *compilerContext) renderSelectClose(sel *qcode.Select) {
	c.w.WriteString(`)`)
	aliasWithID(c.w, "__sr", sel.ID)

	if !sel.Singular {
		c.w.WriteString(`)`)
		aliasWithID(c.w, "__sj", sel.ID)
	}
}

func (c *compilerContext) renderLateralJoin() {
	c.w.WriteString(` LEFT OUTER JOIN LATERAL (`)
}

func (c *compilerContext) renderLateralJoinClose(sel *qcode.Select) {
	c.w.WriteString(`)`)
	aliasWithID(c.w, `__sj`, sel.ID)
	c.w.WriteString(` ON true`)
}

func (c *compilerContext) renderJoinTables(sel *qcode.Select) {
	var pid int32
	for i, rel := range sel.Joins {
		if i == 0 {
			pid = sel.ParentID
		} else {
			pid = -1
		}

		c.renderJoin(rel, pid)
	}
}

func (c *compilerContext) renderJoin(rel sdata.DBRel, pid int32) {
	c.w.WriteString(` LEFT OUTER JOIN `)
	c.w.WriteString(rel.Left.Ti.Name)
	c.w.WriteString(` ON ((`)
	c.renderRel(rel.Left.Ti, rel, pid, nil)
	c.w.WriteString(`))`)
}

func (c *compilerContext) renderBaseSelect(sel *qcode.Select) {
	c.renderCursorCTE(sel)
	c.w.WriteString(`SELECT `)
	c.renderDistinctOn(sel)
	c.renderBaseColumns(sel)
	c.renderFrom(sel)
	c.renderJoinTables(sel)

	// Recursive base selects have no where clauses
	if sel.Rel.Type != sdata.RelRecursive {
		c.renderWhere(sel)
	}

	c.renderGroupBy(sel)
	c.renderOrderBy(sel)
	c.renderLimit(sel)
}

func (c *compilerContext) renderLimit(sel *qcode.Select) {
	switch {
	case sel.Paging.NoLimit:
		break

	case sel.Singular:
		c.w.WriteString(` LIMIT 1`)

	case sel.Paging.LimitVar != "":
		c.w.WriteString(` LIMIT LEAST(`)
		c.renderParam(Param{Name: sel.Paging.LimitVar, Type: "integer"})
		c.w.WriteString(`, `)
		int32String(c.w, sel.Paging.Limit)
		c.w.WriteString(`)`)

	default:
		c.w.WriteString(` LIMIT `)
		int32String(c.w, sel.Paging.Limit)
	}

	switch {
	case sel.Paging.OffsetVar != "":
		c.w.WriteString(` OFFSET `)
		c.renderParam(Param{Name: sel.Paging.OffsetVar, Type: "integer"})

	case sel.Paging.Offset != 0:
		c.w.WriteString(` OFFSET `)
		int32String(c.w, sel.Paging.Offset)
	}
}

func (c *compilerContext) renderRecursiveCTE(sel *qcode.Select) {
	c.w.WriteString(`WITH RECURSIVE `)
	c.quoted("_rcte_" + sel.Rel.Right.Ti.Name)
	c.w.WriteString(` AS (`)
	c.renderRecursiveBaseSelect(sel)
	c.w.WriteString(`) `)
}

func (c *compilerContext) renderRecursiveBaseSelect(sel *qcode.Select) {
	psel := &c.qc.Selects[sel.ParentID]

	c.w.WriteString(`(SELECT `)
	c.renderBaseColumns(sel)
	c.renderFrom(psel)
	c.w.WriteString(` WHERE (`)
	colWithTable(c.w, sel.Table, sel.Ti.PrimaryCol.Name)
	c.w.WriteString(`) = (`)
	colWithTableID(c.w, psel.Table, psel.ID, sel.Ti.PrimaryCol.Name)
	c.w.WriteString(`) LIMIT 1) UNION ALL `)

	c.w.WriteString(`SELECT `)
	c.renderBaseColumns(sel)
	c.renderFrom(psel)
	c.w.WriteString(`, `)
	c.quoted("_rcte_" + sel.Rel.Right.Ti.Name)
	c.renderWhere(sel)
}

func (c *compilerContext) renderFrom(sel *qcode.Select) {
	c.w.WriteString(` FROM `)

	switch sel.Rel.Type {
	case sdata.RelEmbedded:
		c.w.WriteString(sel.Rel.Left.Col.Table)
		c.w.WriteString(`, `)

		switch c.ct {
		case "mysql":
			c.renderJSONTable(sel)
		default:
			c.renderRecordSet(sel)
		}

	case sdata.RelRecursive:
		c.w.WriteString(`(SELECT * FROM `)
		c.quoted("_rcte_" + sel.Rel.Right.Ti.Name)
		switch c.ct {
		case "mysql":
			c.w.WriteString(` LIMIT 1, 18446744073709551610) `)
		default:
			c.w.WriteString(` OFFSET 1) `)
		}
		c.quoted(sel.Table)

	default:
		c.quoted(sel.Table)
	}

	if sel.Paging.Cursor {
		c.w.WriteString(`, __cur`)
	}
}

func (c *compilerContext) renderJSONTable(sel *qcode.Select) {
	c.w.WriteString(`JSON_TABLE(`)
	colWithTable(c.w, sel.Rel.Left.Col.Table, sel.Rel.Left.Col.Name)
	c.w.WriteString(`, "$[*]" COLUMNS(`)

	for i, col := range sel.Ti.Columns {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		c.w.WriteString(col.Name)
		c.w.WriteString(` `)
		c.w.WriteString(col.Type)
		c.w.WriteString(` PATH "$.`)
		c.w.WriteString(col.Name)
		c.w.WriteString(`" ERROR ON ERROR`)
	}
	c.w.WriteString(`)) AS`)
	c.quoted(sel.Table)
}

func (c *compilerContext) renderRecordSet(sel *qcode.Select) {
	// jsonb_to_recordset('[{"a":1,"b":[1,2,3],"c":"bar"}, {"a":2,"b":[1,2,3],"c":"bar"}]') as x(a int, b text, d text);
	c.w.WriteString(sel.Ti.Type)
	c.w.WriteString(`_to_recordset(`)
	colWithTable(c.w, sel.Rel.Left.Col.Table, sel.Rel.Left.Col.Name)
	c.w.WriteString(`) AS `)
	c.quoted(sel.Table)

	c.w.WriteString(`(`)
	for i, col := range sel.Ti.Columns {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		c.w.WriteString(col.Name)
		c.w.WriteString(` `)
		c.w.WriteString(col.Type)
	}
	c.w.WriteString(`)`)
}

func (c *compilerContext) renderCursorCTE(sel *qcode.Select) {
	if !sel.Paging.Cursor {
		return
	}
	c.w.WriteString(`WITH __cur AS (SELECT `)
	switch c.ct {
	case "mysql":
		for i, ob := range sel.OrderBy {
			if i != 0 {
				c.w.WriteString(`, `)
			}
			c.w.WriteString(`SUBSTRING_INDEX(SUBSTRING_INDEX(a.i, ',', `)
			int32String(c.w, int32(i+1))
			c.w.WriteString(`), ',', -1) AS `)
			c.quoted(ob.Col.Name)
		}
		c.w.WriteString(` FROM ((SELECT `)
		c.renderParam(Param{Name: "cursor", Type: "text"})
		c.w.WriteString(` AS i)) as a) `)

	default:
		for i, ob := range sel.OrderBy {
			if i != 0 {
				c.w.WriteString(`, `)
			}
			c.w.WriteString(`a[`)
			int32String(c.w, int32(i+1))
			c.w.WriteString(`] :: `)
			c.w.WriteString(ob.Col.Type)
			c.w.WriteString(` as `)
			c.quoted(ob.Col.Name)
		}
		c.w.WriteString(` FROM string_to_array(`)
		c.renderParam(Param{Name: "cursor", Type: "text"})
		c.w.WriteString(`, ',') as a) `)
	}
}

func (c *compilerContext) renderWhere(sel *qcode.Select) {
	if sel.Rel.Type == sdata.RelNone && sel.Where.Exp == nil {
		return
	}

	c.w.WriteString(` WHERE (`)

	var pid int32

	if sel.Type == qcode.SelTypeMember {
		pid = c.qc.Selects[sel.ParentID].ParentID
	} else {
		pid = sel.ParentID
	}

	if len(sel.Joins) != 0 {
		pid = -1
	}

	c.renderRel(sel.Ti, sel.Rel, pid, sel.ArgMap)

	if sel.Where.Exp != nil {
		if sel.Rel.Type != sdata.RelNone {
			c.w.WriteString(` AND `)
		}
		c.renderExp(c.qc.Schema, sel.Ti, sel.Where.Exp, false)
	}

	c.w.WriteString(`)`)
}

func (c *compilerContext) renderExp(schema *sdata.DBSchema, ti sdata.TInfo, ex *qcode.Exp, skipNested bool) {
	st := util.NewStackInf()
	st.Push(ex)

	for {
		if st.Len() == 0 {
			break
		}

		intf := st.Pop()
		switch val := intf.(type) {
		case int32:
			switch val {
			case '(':
				c.w.WriteString(`(`)
			case ')':
				c.w.WriteString(`)`)
			}

		case qcode.ExpOp:
			switch val {
			case qcode.OpAnd:
				c.w.WriteString(` AND `)
			case qcode.OpOr:
				c.w.WriteString(` OR `)
			case qcode.OpNot:
				c.w.WriteString(`NOT `)
			case qcode.OpFalse:
				c.w.WriteString(`false`)
			}

		case *qcode.Exp:
			switch val.Op {
			case qcode.OpFalse:
				st.Push(val.Op)

			case qcode.OpAnd, qcode.OpOr:
				st.Push(')')
				for i := len(val.Children) - 1; i >= 0; i-- {
					st.Push(val.Children[i])
					if i > 0 {
						st.Push(val.Op)
					}
				}
				st.Push('(')

			case qcode.OpNot:
				st.Push(val.Children[0])
				st.Push(qcode.OpNot)

			default:
				if !skipNested && len(val.Rels) != 0 {
					c.renderNestedWhere(schema, ti, val)
				} else {
					c.renderOp(schema, ti, val)
				}
			}
		}
	}
}

func (c *compilerContext) renderNestedWhere(
	schema *sdata.DBSchema, ti sdata.TInfo, ex *qcode.Exp) {
	firstRel := ex.Rels[0]
	c.w.WriteString(`EXISTS (SELECT 1 FROM `)
	c.w.WriteString(firstRel.Left.Col.Table)

	if len(ex.Rels) > 1 {
		for _, rel := range ex.Rels[1:(len(ex.Rels) - 1)] {
			c.renderJoin(rel, -1)
		}
	}

	c.w.WriteString(` WHERE `)
	lastRel := ex.Rels[(len(ex.Rels) - 1)]
	c.renderExp(schema, lastRel.Left.Ti, ex, true)

	c.w.WriteString(` AND (`)
	c.renderRel(ti, firstRel, -1, nil)
	c.w.WriteString(`))`)
}

func (c *compilerContext) renderOp(schema *sdata.DBSchema, ti sdata.TInfo, ex *qcode.Exp) {
	if ex.Op == qcode.OpNop {
		return
	}

	if c.renderValPrefix(ti, ex) {
		return
	}

	if ex.Col.Name != "" {
		c.w.WriteString(`((`)
		if ex.Type == qcode.ValRef && ex.Op == qcode.OpIsNull {
			colWithTable(c.w, ex.Table, ex.Col.Name)
		} else {
			colWithTable(c.w, ti.Name, ex.Col.Name)
		}
		c.w.WriteString(`) `)
	}

	switch ex.Op {
	case qcode.OpEquals:
		c.w.WriteString(`=`)
	case qcode.OpNotEquals:
		c.w.WriteString(`!=`)
	case qcode.OpNotDistinct:
		c.w.WriteString(`IS NOT DISTINCT FROM`)
	case qcode.OpDistinct:
		c.w.WriteString(`IS DISTINCT FROM`)
	case qcode.OpGreaterOrEquals:
		c.w.WriteString(`>=`)
	case qcode.OpLesserOrEquals:
		c.w.WriteString(`<=`)
	case qcode.OpGreaterThan:
		c.w.WriteString(`>`)
	case qcode.OpLesserThan:
		c.w.WriteString(`<`)
	case qcode.OpIn:
		c.w.WriteString(`= ANY`)
	case qcode.OpNotIn:
		c.w.WriteString(`!= ALL`)
	case qcode.OpLike:
		c.w.WriteString(`LIKE`)
	case qcode.OpNotLike:
		c.w.WriteString(`NOT LIKE`)
	case qcode.OpILike:
		c.w.WriteString(`ILIKE`)
	case qcode.OpNotILike:
		c.w.WriteString(`NOT ILIKE`)
	case qcode.OpSimilar:
		c.w.WriteString(`SIMILAR TO`)
	case qcode.OpNotSimilar:
		c.w.WriteString(`NOT SIMILAR TO`)
	case qcode.OpRegex:
		c.w.WriteString(`~`)
	case qcode.OpNotRegex:
		c.w.WriteString(`!~`)
	case qcode.OpIRegex:
		c.w.WriteString(`~*`)
	case qcode.OpNotIRegex:
		c.w.WriteString(`!~*`)
	case qcode.OpContains:
		c.w.WriteString(`@>`)
	case qcode.OpContainedIn:
		c.w.WriteString(`<@`)
	case qcode.OpHasKey:
		c.w.WriteString(`?`)
	case qcode.OpHasKeyAny:
		c.w.WriteString(`?|`)
	case qcode.OpHasKeyAll:
		c.w.WriteString(`?&`)

	case qcode.OpEqualsTrue:
		c.w.WriteString(`(`)
		c.renderParam(Param{Name: ex.Val, Type: "boolean"})
		c.w.WriteString(` IS TRUE)`)
		return

	case qcode.OpNotEqualsTrue:
		c.w.WriteString(`(`)
		c.renderParam(Param{Name: ex.Val, Type: "boolean"})
		c.w.WriteString(` IS NOT TRUE)`)
		return

	case qcode.OpIsNull:
		if strings.EqualFold(ex.Val, "true") {
			c.w.WriteString(`IS NULL)`)
		} else {
			c.w.WriteString(`IS NOT NULL)`)
		}
		return

	case qcode.OpTsQuery:
		switch c.ct {
		case "mysql":
			//MATCH (name) AGAINST ('phone' IN BOOLEAN MODE);
			c.w.WriteString(`(MATCH(`)
			for i, col := range ti.FullText {
				if i != 0 {
					c.w.WriteString(`, `)
				}
				colWithTable(c.w, ti.Name, col.Name)
			}
			c.w.WriteString(`) AGAINST (`)
			c.renderParam(Param{Name: ex.Val, Type: "text"})
			c.w.WriteString(` IN NATURAL LANGUAGE MODE))`)

		default:
			//fmt.Fprintf(w, `(("%s") @@ websearch_to_tsquery('%s'))`, c.ti.TSVCol, val.Val)
			c.w.WriteString(`((`)
			for i, col := range ti.FullText {
				if i != 0 {
					c.w.WriteString(` OR (`)
				}
				colWithTable(c.w, ti.Name, col.Name)
				if c.cv >= 110000 {
					c.w.WriteString(`) @@ websearch_to_tsquery(`)
				} else {
					c.w.WriteString(`) @@ to_tsquery(`)
				}
				c.renderParam(Param{Name: ex.Val, Type: "text"})
				c.w.WriteString(`)`)
			}
			c.w.WriteString(`)`)
		}
		return
	}
	c.w.WriteString(` `)

	switch {
	case ex.Type == qcode.ValList:
		c.renderList(ex)
	default:
		c.renderVal(ti, ex)
	}

	c.w.WriteString(`)`)
}

func (c *compilerContext) renderGroupBy(sel *qcode.Select) {
	if !sel.GroupCols {
		return
	}
	c.w.WriteString(` GROUP BY `)

	for i, col := range sel.BCols {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		colWithTable(c.w, sel.Table, col.Col.Name)
	}
}

func (c *compilerContext) renderOrderBy(sel *qcode.Select) {
	if len(sel.OrderBy) == 0 {
		return
	}
	c.w.WriteString(` ORDER BY `)
	for i, col := range sel.OrderBy {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		colWithTable(c.w, sel.Table, col.Col.Name)

		switch col.Order {
		case qcode.OrderAsc:
			c.w.WriteString(` ASC`)
		case qcode.OrderDesc:
			c.w.WriteString(` DESC`)
		case qcode.OrderAscNullsFirst:
			c.w.WriteString(` ASC NULLS FIRST`)
		case qcode.OrderDescNullsFirst:
			c.w.WriteString(` DESC NULLLS FIRST`)
		case qcode.OrderAscNullsLast:
			c.w.WriteString(` ASC NULLS LAST`)
		case qcode.OrderDescNullsLast:
			c.w.WriteString(` DESC NULLS LAST`)
		}
	}
}

func (c *compilerContext) renderDistinctOn(sel *qcode.Select) {
	if len(sel.DistinctOn) == 0 {
		return
	}
	c.w.WriteString(`DISTINCT ON (`)
	for i, col := range sel.DistinctOn {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		colWithTable(c.w, sel.Table, col.Name)
	}
	c.w.WriteString(`) `)
}

func (c *compilerContext) renderList(ex *qcode.Exp) {
	c.w.WriteString(`(ARRAY[`)
	for i := range ex.ListVal {
		if i != 0 {
			c.w.WriteString(`, `)
		}
		switch ex.ListType {
		case qcode.ValBool, qcode.ValNum:
			c.w.WriteString(ex.ListVal[i])
		case qcode.ValStr:
			c.w.WriteString(`'`)
			c.w.WriteString(ex.ListVal[i])
			c.w.WriteString(`'`)
		}
	}
	c.w.WriteString(`])`)
}

func (c *compilerContext) renderValPrefix(ti sdata.TInfo, ex *qcode.Exp) bool {
	if ex.Type == qcode.ValVar {
		return c.renderValVarPrefix(ti, ex)
	}
	return false
}

func (c *compilerContext) renderValVarPrefix(ti sdata.TInfo, ex *qcode.Exp) bool {
	if ex.Op == qcode.OpIn || ex.Op == qcode.OpNotIn {
		if c.ct == "mysql" {
			c.w.WriteString(`JSON_CONTAINS(`)
			c.renderParam(Param{Name: ex.Val, Type: ex.Col.Type, IsArray: true})
			c.w.WriteString(`, CAST(`)
			colWithTable(c.w, ti.Name, ex.Col.Name)
			c.w.WriteString(` AS JSON), '$')`)
			return true
		}
	}
	return false
}

func (c *compilerContext) renderVal(ti sdata.TInfo, ex *qcode.Exp) {
	switch ex.Type {
	case qcode.ValVar:
		c.renderValVar(ti, ex)

	case qcode.ValRef:
		colWithTable(c.w, ex.Table, ex.Col.Name)

	default:
		c.squoted(ex.Val)
	}
}

func (c *compilerContext) renderValVar(ti sdata.TInfo, ex *qcode.Exp) {
	val, isVal := c.svars[ex.Val]

	switch {
	case isVal && strings.HasPrefix(val, "sql:"):
		c.w.WriteString(`(`)
		c.renderVar(val[4:])
		c.w.WriteString(`)`)

	case isVal:
		c.w.WriteString(`'`)
		c.renderVar(val)
		c.w.WriteString(`'`)

	case ex.Op == qcode.OpIn || ex.Op == qcode.OpNotIn:
		c.w.WriteString(`(ARRAY(SELECT json_array_elements_text(`)
		c.renderParam(Param{Name: ex.Val, Type: ex.Col.Type, IsArray: true})
		c.w.WriteString(`))`)
		c.w.WriteString(` :: `)
		c.w.WriteString(ex.Col.Type)
		c.w.WriteString(`[])`)

	default:
		c.renderParam(Param{Name: ex.Val, Type: ex.Col.Type, IsArray: false})
	}
}
