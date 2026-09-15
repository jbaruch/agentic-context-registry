package codecensus

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
)

// Target identifies a field by its declaring type, not a local/import name.
// Registered is the allowed vocabulary at this output boundary. AllowEmpty is
// for boundaries that normalize an empty code before rendering it.
type Target struct {
	Package, Type, Field, Namespace string
	Registered                      []string
	AllowEmpty                      bool
}

// Diagnostic locates an unregistered literal or an unproven source expression.
type Diagnostic struct {
	Position           token.Position
	Namespace, Message string
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", d.Position.Filename, d.Position.Line, d.Namespace, d.Message)
}

// Result contains sorted code sets and deterministic, deduplicated diagnostics.
type Result struct {
	Codes       map[string][]string
	Diagnostics []Diagnostic
}

type value struct {
	text    string
	pos     token.Pos
	unknown bool
}
type node struct {
	functions []*types.Signature
	values    []value
	edges     []*node
}
type environment map[types.Object]*node

type invocation struct {
	function *node
	args     []*node
}

type census struct {
	calls         []invocation
	loopContinues []*[]environment
	breakExits    []*[]environment
	jump          token.Pos
	*sourceImporter
	captures map[types.Object]*node
	fields   map[types.Object]*node
	params   map[types.Object]*node
	funcs    map[*types.Func]bool
	globals  environment
}

// Analyze checks supplied source bodies. Constants, lexical local assignments,
// branch joins, direct helper arguments and typed field forwarding are followed.
// Other string-producing expressions fail closed when they reach a target.
// Field vocabularies and helper parameters are joined across the supplied
// program; this is a closed-source census, not a proof about external embedders.
func Analyze(sources []Source, fallback types.Importer, targets []Target) (Result, error) {
	parsed, err := parse(sources, fallback)
	if err != nil {
		return Result{}, err
	}
	c := &census{sourceImporter: parsed, captures: map[types.Object]*node{}, fields: map[types.Object]*node{}, params: map[types.Object]*node{}, funcs: map[*types.Func]bool{}, globals: environment{}}
	for _, files := range c.files {
		for _, file := range files {
			for _, decl := range file.Decls {
				if f, ok := decl.(*ast.FuncDecl); ok {
					fn := c.info.Defs[f.Name].(*types.Func)
					c.funcs[fn] = true
					sig := fn.Type().(*types.Signature)
					for i := 0; i < sig.Params().Len(); i++ {
						p := sig.Params().At(i)
						c.params[p] = &node{}
					}
				}
			}
		}
	}
	// Global initializers precede function bodies. A global string that is changed
	// later is joined, never assumed constant merely because of its name.
	for _, files := range c.files {
		for _, file := range files {
			for _, decl := range file.Decls {
				if d, ok := decl.(*ast.GenDecl); ok {
					c.declaration(d, c.globals)
				}
			}
		}
	}
	for _, files := range c.files {
		for _, file := range files {
			for _, decl := range file.Decls {
				if f, ok := decl.(*ast.FuncDecl); ok && f.Body != nil {
					c.functionBody(f.Body, c.info.Defs[f.Name].Type().(*types.Signature), c.globals)
				}
			}
		}
	}
	// Connect calls after all bodies are read: a callback may be passed through
	// several helpers before its code-producing closure is invoked.
	connected := map[int]map[*types.Signature]bool{}
	for changed := true; changed; {
		changed = false
		for i, call := range c.calls {
			if connected[i] == nil {
				connected[i] = map[*types.Signature]bool{}
			}
			for _, sig := range functionValues(call.function, map[*node]bool{}) {
				if connected[i][sig] {
					continue
				}
				connected[i][sig] = true
				changed = true
				for j, arg := range call.args {
					if j < sig.Params().Len() {
						p := sig.Params().At(j)
						if n := c.params[p]; n != nil {
							n.edges = append(n.edges, arg)
						}
					}
				}
			}
		}
	}
	for obj, n := range c.params {
		if len(n.edges) == 0 {
			n.values = append(n.values, value{pos: obj.Pos(), unknown: true})
		}
	}
	for obj, n := range c.fields {
		if len(n.edges) == 0 {
			n.values = append(n.values, value{pos: obj.Pos(), unknown: true})
		}
	}
	result := Result{Codes: map[string][]string{}}
	seen := map[string]bool{}
	for _, target := range targets {
		p := c.packages[target.Package]
		if p == nil {
			return result, fmt.Errorf("census target package %s is absent", target.Package)
		}
		obj := p.Scope().Lookup(target.Type)
		if obj == nil {
			return result, fmt.Errorf("census target type %s.%s is absent", target.Package, target.Type)
		}
		field, _, _ := types.LookupFieldOrMethod(obj.Type(), true, p, target.Field)
		if field == nil {
			return result, fmt.Errorf("census target field %s.%s.%s is absent", target.Package, target.Type, target.Field)
		}
		root := c.fields[field]
		if root == nil {
			return result, fmt.Errorf("census target %s.%s.%s has no source writes", target.Package, target.Type, target.Field)
		}
		registered := map[string]bool{}
		for _, code := range target.Registered {
			registered[code] = true
		}
		codes := map[string]bool{}
		values := c.resolve(root, map[*node]bool{})
		if len(values) == 0 {
			values = []value{{pos: field.Pos(), unknown: true}}
		}
		for _, v := range values {
			if !v.unknown && v.text == "" && target.AllowEmpty {
				continue
			}
			message := ""
			if v.unknown {
				message = "cannot prove code expression; use a constant or a supported assignment flow"
			} else {
				codes[v.text] = true
				if !registered[v.text] {
					message = fmt.Sprintf("unregistered code %q", v.text)
				}
			}
			if message != "" {
				d := Diagnostic{c.fset.Position(v.pos), target.Namespace, message}
				key := d.String()
				if !seen[key] {
					seen[key] = true
					result.Diagnostics = append(result.Diagnostics, d)
				}
			}
		}
		for code := range codes {
			result.Codes[target.Namespace] = append(result.Codes[target.Namespace], code)
		}
		sort.Strings(result.Codes[target.Namespace])
	}
	sort.Slice(result.Diagnostics, func(i, j int) bool {
		a, b := result.Diagnostics[i], result.Diagnostics[j]
		if a.Position.Filename != b.Position.Filename {
			return a.Position.Filename < b.Position.Filename
		}
		if a.Position.Line != b.Position.Line {
			return a.Position.Line < b.Position.Line
		}
		return a.String() < b.String()
	})
	return result, nil
}
func (c *census) resolve(n *node, seen map[*node]bool) []value {
	if n == nil || seen[n] {
		return nil
	}
	seen[n] = true
	result := append([]value(nil), n.values...)
	for _, edge := range n.edges {
		result = append(result, c.resolve(edge, seen)...)
	}
	return result
}
func unknown(pos token.Pos) *node              { return &node{values: []value{{pos: pos, unknown: true}}} }
func literal(text string, pos token.Pos) *node { return &node{values: []value{{text: text, pos: pos}}} }
func join(nodes ...*node) *node                { return &node{edges: nodes} }
func copyEnv(env environment) environment {
	copy := environment{}
	for k, v := range env {
		copy[k] = v
	}
	return copy
}
func mergeEnv(dst environment, branches ...environment) {
	for _, branch := range branches {
		for key, v := range branch {
			if old := dst[key]; old != v {
				dst[key] = join(old, v)
			}
		}
	}
}
func (c *census) field(obj types.Object) *node {
	if c.fields[obj] == nil {
		c.fields[obj] = &node{}
	}
	return c.fields[obj]
}
func (c *census) object(id *ast.Ident) types.Object {
	if o := c.info.Defs[id]; o != nil {
		return o
	}
	return c.info.Uses[id]
}
func isString(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

func (c *census) expr(e ast.Expr, env environment) *node {
	if e == nil {
		return nil
	}
	if tv := c.info.Types[e]; tv.Value != nil && tv.Value.Kind() == constant.String {
		return literal(constant.StringVal(tv.Value), e.Pos())
	}
	switch e := e.(type) {
	case *ast.Ident:
		obj := c.object(e)
		if n := env[obj]; n != nil {
			return n
		}
		if p := c.params[obj]; p != nil {
			return p
		}
		if fn, ok := obj.(*types.Func); ok && c.funcs[fn] {
			return &node{functions: []*types.Signature{fn.Type().(*types.Signature)}}
		}
		return unknown(e.Pos())
	case *ast.ParenExpr:
		return c.expr(e.X, env)
	case *ast.SelectorExpr:
		c.expr(e.X, env)
		if sel := c.info.Selections[e]; sel != nil && sel.Kind() == types.FieldVal {
			return c.field(sel.Obj())
		}
		if fn, ok := c.info.Uses[e.Sel].(*types.Func); ok && c.funcs[fn] {
			return &node{functions: []*types.Signature{fn.Type().(*types.Signature)}}
		}
		return unknown(e.Pos())
	case *ast.CallExpr:
		args := make([]*node, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expr(arg, env)
		}
		if c.info.Types[e.Fun].IsType() && len(args) == 1 {
			if isString(c.info.TypeOf(e)) && isString(c.info.TypeOf(e.Args[0])) {
				return args[0]
			}
			// Legal aggregate conversions connect type fields, even when the
			// converted value is subsequently forwarded through another helper.
			c.convertFields(c.info.TypeOf(e), c.info.TypeOf(e.Args[0]), false, map[[2]types.Type]bool{})
			return unknown(e.Pos())
		}
		fun := c.expr(e.Fun, env)
		c.calls = append(c.calls, invocation{fun, args})
		return unknown(e.Pos())
	case *ast.CompositeLit:
		st, ok := c.info.TypeOf(e).Underlying().(*types.Struct)
		for i, element := range e.Elts {
			rhs := element
			var field *types.Var
			if kv, keyed := element.(*ast.KeyValueExpr); keyed {
				rhs = kv.Value
				if id, ok := kv.Key.(*ast.Ident); ok {
					field, _ = c.info.Uses[id].(*types.Var)
				}
			} else if ok && i < st.NumFields() {
				field = st.Field(i)
			}
			v := c.expr(rhs, env)
			if field != nil && isString(field.Type()) {
				c.field(field).edges = append(c.field(field).edges, v)
			}
		}
		return unknown(e.Pos())
	case *ast.UnaryExpr:
		v := c.expr(e.X, env)
		if e.Op == token.AND {
			// A string address can be mutated by a callee. Record that uncertainty at
			// its source, even when an earlier assignment was a registered constant.
			if isString(c.info.TypeOf(e.X)) {
				c.assign(e.X, unknown(e.Pos()), env, false)
			}
		}
		return v
	case *ast.BinaryExpr:
		c.expr(e.X, env)
		c.expr(e.Y, env)
	case *ast.IndexExpr:
		c.expr(e.X, env)
		c.expr(e.Index, env)
	case *ast.SliceExpr:
		c.expr(e.X, env)
		c.expr(e.Low, env)
		c.expr(e.High, env)
		c.expr(e.Max, env)
	case *ast.StarExpr:
		return c.expr(e.X, env)
	case *ast.TypeAssertExpr:
		c.expr(e.X, env)
	case *ast.FuncLit:
		// Closures capture variable cells, not the value at their declaration.
		ast.Inspect(e.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := c.info.Uses[id]
			if current := env[obj]; current != nil {
				if c.captures[obj] == nil {
					c.captures[obj] = join(current)
				}
				env[obj] = c.captures[obj]
			}
			return true
		})
		sig := c.info.TypeOf(e).(*types.Signature)
		for i := 0; i < sig.Params().Len(); i++ {
			p := sig.Params().At(i)
			if c.params[p] == nil {
				c.params[p] = &node{}
			}
		}
		c.functionBody(e.Body, sig, env)
		return &node{functions: []*types.Signature{sig}}
	case *ast.KeyValueExpr:
		c.expr(e.Key, env)
		c.expr(e.Value, env)
	}
	return unknown(e.Pos())
}

// convertFields follows corresponding fields of a type-checked conversion.
// Pointers and slices share storage, so writes through either type reach both
// vocabularies. Value conversions only forward from source to destination.
func (c *census) convertFields(dst, src types.Type, shared bool, seen map[[2]types.Type]bool) {
	pair := [2]types.Type{dst, src}
	if seen[pair] {
		return
	}
	seen[pair] = true
	switch d := dst.Underlying().(type) {
	case *types.Pointer:
		if s, ok := src.Underlying().(*types.Pointer); ok {
			c.convertFields(d.Elem(), s.Elem(), true, seen)
		} else if _, ok := src.Underlying().(*types.Slice); ok {
			// A slice-to-array-pointer conversion aliases the slice's elements.
			c.convertFields(d.Elem(), src, true, seen)
		}
	case *types.Slice:
		if s, ok := src.Underlying().(*types.Slice); ok {
			c.convertFields(d.Elem(), s.Elem(), true, seen)
		}
	case *types.Array:
		switch s := src.Underlying().(type) {
		case *types.Array:
			c.convertFields(d.Elem(), s.Elem(), shared, seen)
		case *types.Slice:
			c.convertFields(d.Elem(), s.Elem(), shared, seen)
		}
	case *types.Struct:
		if s, ok := src.Underlying().(*types.Struct); ok {
			for i := 0; i < d.NumFields(); i++ {
				df, sf := d.Field(i), s.Field(i)
				if isString(df.Type()) {
					dn, sn := c.field(df), c.field(sf)
					if dn != sn {
						dn.edges = append(dn.edges, sn)
						if shared {
							sn.edges = append(sn.edges, dn)
						}
					}
				} else {
					c.convertFields(df.Type(), sf.Type(), shared, seen)
				}
			}
		}
	}
}

func (c *census) assign(lhs ast.Expr, v *node, env environment, compound bool) {
	lhs = ast.Unparen(lhs)
	if !isString(c.info.TypeOf(lhs)) && !isFunction(c.info.TypeOf(lhs)) {
		return
	}
	if compound {
		v = unknown(lhs.Pos())
	}
	if c.jump.IsValid() {
		v = join(v, unknown(c.jump))
	}
	switch lhs := lhs.(type) {
	case *ast.Ident:
		obj := c.object(lhs)
		if obj == nil {
			return
		}
		if cell := c.captures[obj]; cell != nil {
			cell.edges = append(cell.edges, v)
			env[obj] = cell
			return
		}
		if obj.Parent() == obj.Pkg().Scope() {
			n := c.globals[obj]
			if n == nil {
				n = &node{}
				c.globals[obj] = n
			}
			n.edges = append(n.edges, v)
			env[obj] = n
		} else {
			env[obj] = v
		}
	case *ast.SelectorExpr:
		c.expr(lhs.X, env)
		if sel := c.info.Selections[lhs]; sel != nil {
			n := c.field(sel.Obj())
			n.edges = append(n.edges, v)
		}
	default:
		// An unsupported write through a pointer/index must not silently retain
		// the value read from that location.
		n := c.expr(lhs, env)
		if n != nil {
			n.edges = append(n.edges, unknown(lhs.Pos()))
		}
	}
}
func (c *census) declaration(d *ast.GenDecl, env environment) {
	if d.Tok != token.VAR {
		return
	}
	for _, spec := range d.Specs {
		v := spec.(*ast.ValueSpec)
		values := make([]*node, len(v.Values))
		for i, e := range v.Values {
			values[i] = c.expr(e, env)
		}
		for i, name := range v.Names {
			n := literal("", name.Pos())
			if i < len(values) {
				n = values[i]
			} else if len(values) > 0 {
				n = unknown(name.Pos())
			}
			c.assign(name, n, env, false)
		}
	}
}
func (c *census) block(b *ast.BlockStmt, env environment) {
	if b != nil {
		for _, s := range b.List {
			c.stmt(s, env)
			switch branch := s.(type) {
			case *ast.BranchStmt:
				if branch.Tok != token.GOTO {
					return
				}
			case *ast.ReturnStmt:
				if !c.jump.IsValid() {
					return
				}
			}
		}
	}
}
func (c *census) stmt(s ast.Stmt, env environment) {
	switch s := s.(type) {
	case *ast.BlockStmt:
		c.block(s, env)
	case *ast.DeclStmt:
		if d, ok := s.Decl.(*ast.GenDecl); ok {
			c.declaration(d, env)
		}
	case *ast.AssignStmt:
		values := make([]*node, len(s.Rhs))
		for i, rhs := range s.Rhs {
			values[i] = c.expr(rhs, env)
		}
		for i, lhs := range s.Lhs {
			v := unknown(lhs.Pos())
			if i < len(values) {
				v = values[i]
			}
			c.assign(lhs, v, env, s.Tok != token.ASSIGN && s.Tok != token.DEFINE)
		}
	case *ast.ExprStmt:
		c.expr(s.X, env)
	case *ast.ReturnStmt:
		for _, e := range s.Results {
			c.expr(e, env)
		}
	case *ast.IfStmt:
		c.stmt(s.Init, env)
		c.expr(s.Cond, env)
		yes, no := copyEnv(env), copyEnv(env)
		c.block(s.Body, yes)
		c.stmt(s.Else, no)
		// Each lexical object is distinct, so a shadow cannot replace an outer
		// variable. Join only the two outcomes, not the pre-branch value.
		for key := range env {
			env[key] = join(yes[key], no[key])
		}
	case *ast.SwitchStmt:
		c.stmt(s.Init, env)
		c.expr(s.Tag, env)
		var branches []environment
		c.breakExits = append(c.breakExits, &branches)
		hasDefault := false
		var fall environment
		for _, item := range s.Body.List {
			cl := item.(*ast.CaseClause)
			branch := copyEnv(env)
			if fall != nil {
				mergeEnv(branch, fall)
			}
			fall = nil
			if len(cl.List) == 0 {
				hasDefault = true
			}
			for _, e := range cl.List {
				c.expr(e, branch)
			}
			c.block(&ast.BlockStmt{List: cl.Body}, branch)
			if len(cl.Body) > 0 {
				if last, ok := cl.Body[len(cl.Body)-1].(*ast.BranchStmt); ok && last.Tok == token.FALLTHROUGH {
					fall = branch
				}
			}
			branches = append(branches, branch)
		}
		c.breakExits = c.breakExits[:len(c.breakExits)-1]
		if !hasDefault {
			branches = append(branches, copyEnv(env))
		}
		for key := range env {
			var values []*node
			for _, branch := range branches {
				values = append(values, branch[key])
			}
			env[key] = join(values...)
		}
	case *ast.BranchStmt:
		if s.Tok == token.BREAK && len(c.breakExits) > 0 {
			exits := c.breakExits[len(c.breakExits)-1]
			*exits = append(*exits, copyEnv(env))
		}
		if s.Tok == token.CONTINUE && len(c.loopContinues) > 0 {
			continues := c.loopContinues[len(c.loopContinues)-1]
			*continues = append(*continues, copyEnv(env))
		}
	case *ast.RangeStmt:
		c.expr(s.X, env)
		branch, headers := loopEnv(env)
		var continues []environment
		c.loopContinues = append(c.loopContinues, &continues)
		var exits []environment
		c.breakExits = append(c.breakExits, &exits)
		if s.Key != nil {
			c.assign(s.Key, unknown(s.Key.Pos()), branch, false)
		}
		if s.Value != nil {
			c.assign(s.Value, unknown(s.Value.Pos()), branch, false)
		}
		c.block(s.Body, branch)
		mergeEnv(branch, continues...)
		closeLoop(env, branch, headers)
		mergeEnv(env, exits...)
		c.breakExits = c.breakExits[:len(c.breakExits)-1]
		c.loopContinues = c.loopContinues[:len(c.loopContinues)-1]
	case *ast.ForStmt:
		c.stmt(s.Init, env)
		branch, headers := loopEnv(env)
		c.expr(s.Cond, branch)
		var continues []environment
		c.loopContinues = append(c.loopContinues, &continues)
		var exits []environment
		c.breakExits = append(c.breakExits, &exits)
		c.block(s.Body, branch)
		// A continue executes this loop's post statement before its back edge.
		// Restore saved values even if the continuing path overwrote them.
		mergeEnv(branch, continues...)
		c.stmt(s.Post, branch)
		closeLoop(env, branch, headers)
		mergeEnv(env, exits...)
		c.breakExits = c.breakExits[:len(c.breakExits)-1]
		c.loopContinues = c.loopContinues[:len(c.loopContinues)-1]
	case *ast.TypeSwitchStmt:
		var exits []environment
		c.breakExits = append(c.breakExits, &exits)
		c.stmt(s.Init, env)
		c.stmt(s.Assign, env)
		for _, item := range s.Body.List {
			branch := copyEnv(env)
			for _, stmt := range item.(*ast.CaseClause).Body {
				c.stmt(stmt, branch)
			}
			mergeEnv(env, branch)
		}
		mergeEnv(env, exits...)
		c.breakExits = c.breakExits[:len(c.breakExits)-1]
	case *ast.SelectStmt:
		var exits []environment
		c.breakExits = append(c.breakExits, &exits)
		for _, item := range s.Body.List {
			cl := item.(*ast.CommClause)
			branch := copyEnv(env)
			c.stmt(cl.Comm, branch)
			for _, stmt := range cl.Body {
				c.stmt(stmt, branch)
			}
			mergeEnv(env, branch)
		}
		mergeEnv(env, exits...)
		c.breakExits = c.breakExits[:len(c.breakExits)-1]
	case *ast.GoStmt:
		c.expr(s.Call, env)
	case *ast.DeferStmt:
		c.expr(s.Call, env)
	case *ast.SendStmt:
		c.expr(s.Chan, env)
		c.expr(s.Value, env)
	case *ast.IncDecStmt:
		c.assign(s.X, unknown(s.Pos()), env, true)
	case *ast.LabeledStmt:
		c.stmt(s.Stmt, env)
	}
}

func isFunction(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Signature)
	return ok
}
func functionValues(n *node, seen map[*node]bool) []*types.Signature {
	if n == nil || seen[n] {
		return nil
	}
	seen[n] = true
	result := append([]*types.Signature(nil), n.functions...)
	for _, edge := range n.edges {
		result = append(result, functionValues(edge, seen)...)
	}
	return result
}

func (c *census) functionBody(body *ast.BlockStmt, sig *types.Signature, outer environment) {
	previousJump, previousLoops, previousExits := c.jump, c.loopContinues, c.breakExits
	defer func() { c.jump = previousJump; c.loopContinues = previousLoops; c.breakExits = previousExits }()
	c.jump = token.NoPos
	c.loopContinues = nil
	c.breakExits = nil
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if branch, ok := n.(*ast.BranchStmt); ok && (branch.Tok == token.GOTO || branch.Label != nil) {
			c.jump = branch.Pos()
		}
		return true
	})
	env := copyEnv(outer)
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		env[p] = c.params[p]
	}
	c.block(body, env)
}

// A loop header joins entry and back-edge values. Emissions inside the body
// therefore include later iterations, not just the first lexical traversal.
func loopEnv(env environment) (environment, environment) {
	branch, headers := environment{}, environment{}
	for key, v := range env {
		headers[key] = join(v)
		branch[key] = headers[key]
	}
	return branch, headers
}
func closeLoop(env, branch, headers environment) {
	for key, header := range headers {
		header.edges = append(header.edges, branch[key])
		env[key] = join(header, branch[key])
	}
}
