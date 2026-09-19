package codecensus

import (
	"go/ast"
	"go/token"
	"go/types"
)

// Operand reads retain their value dependency and any capture cell introduced
// while evaluating sibling operands. Go does not order ordinary variable reads
// relative to independent calls. A shallow env copy, or storing a callee node
// before an argument creates its capture, would lose that possible dependency.
func (c *census) beginOperands() func() {
	outer := c.operandReads
	reads := map[types.Object][]*node{}
	c.operandReads = reads
	return func() {
		for obj, nodes := range reads {
			if cell := c.captures[obj]; cell != nil {
				for _, n := range nodes {
					n.edges = append(n.edges, cell)
				}
			}
			// A capture may be introduced by a later operand in the enclosing
			// assignment, after this nested call has finished evaluation.
			if outer != nil {
				outer[obj] = append(outer[obj], nodes...)
			}
		}
		c.operandReads = outer
	}
}

func (c *census) readOperand(obj types.Object, current *node) *node {
	if c.operandReads == nil || (!isString(obj.Type()) && !isFunction(obj.Type())) {
		return current
	}
	read := join(current)
	c.operandReads[obj] = append(c.operandReads[obj], read)
	return read
}

// assignmentTarget is an evaluated destination, not syntax to revisit. read is
// the dependency observed while evaluating it; object/field identify supported
// writes. The graph's shared field/capture summaries keep their existing joins.
// A snapshot of the environment alone would still share those mutable nodes.
type assignmentTarget struct {
	pos     token.Pos
	object  types.Object
	field   *node
	read    *node
	tracked bool
}

func (c *census) assignmentTarget(lhs ast.Expr, env environment) assignmentTarget {
	lhs = ast.Unparen(lhs)
	target := assignmentTarget{
		pos:     lhs.Pos(),
		tracked: isString(c.info.TypeOf(lhs)) || isFunction(c.info.TypeOf(lhs)),
	}
	// Even an aggregate target can evaluate code-emitting calls or conversions
	// that connect tracked fields. Evaluate these before all assignment writes.
	target.read = c.expr(lhs, env)
	if !target.tracked {
		return target
	}
	switch lhs := lhs.(type) {
	case *ast.Ident:
		target.object = c.object(lhs)
	case *ast.SelectorExpr:
		if sel := c.info.Selections[lhs]; sel != nil && sel.Kind() == types.FieldVal {
			target.field = c.field(sel.Obj())
		} else if obj := c.info.Uses[lhs.Sel]; isGlobalVariable(obj) && c.packages[obj.Pkg().Path()] != nil {
			// A qualified package variable is a Uses entry, not a field
			// Selection. Keep the identity its owning package's readers use.
			target.object = obj
		}
	}
	return target
}

func (c *census) assignmentTargets(lhs []ast.Expr, env environment) []assignmentTarget {
	targets := make([]assignmentTarget, len(lhs))
	for i, expr := range lhs {
		targets[i] = c.assignmentTarget(expr, env)
	}
	return targets
}

func (c *census) writeAssignments(targets []assignmentTarget, values []*node, env environment, compound bool) {
	values = unpackResults(values, len(targets))
	for i, target := range targets {
		v := unknown(target.pos)
		if i < len(values) {
			v = values[i]
		}
		c.writeAssignment(target, v, env, compound)
	}
}

// No expression evaluation belongs here: writes run only after operand
// evaluation. Shared graph joins do not erase an already evaluated dependency.
func (c *census) writeAssignment(target assignmentTarget, v *node, env environment, compound bool) {
	if !target.tracked {
		return
	}
	if compound {
		v = unknown(target.pos)
	}
	if c.jump.IsValid() {
		v = join(v, unknown(c.jump))
	}
	if obj := target.object; obj != nil {
		if cell := c.captures[obj]; cell != nil {
			cell.edges = append(cell.edges, v)
			env[obj] = cell
		} else if isGlobalVariable(obj) {
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
	} else if target.field != nil {
		target.field.edges = append(target.field.edges, v)
	} else if target.read != nil {
		// Unsupported indirect string/function writes invalidate the dependency
		// resolved during evaluation, rather than traversing in the changed env.
		target.read.edges = append(target.read.edges, unknown(target.pos))
	}
}

func (c *census) assign(lhs ast.Expr, v *node, env environment, compound bool) {
	finish := c.beginOperands()
	target := c.assignmentTarget(lhs, env)
	finish()
	c.writeAssignment(target, v, env, compound)
}

// Package variables have one shared destination across all supplied bodies.
// They must never be split into a function's lexical capture cell.
func isGlobalVariable(obj types.Object) bool {
	_, ok := obj.(*types.Var)
	return ok && obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()
}

// Statement operands close dependencies before any following body or write.
// A nested call contributes its reads to this phase without re-evaluation.
func (c *census) statementOperands(env environment, expressions ...ast.Expr) {
	finish := c.beginOperands()
	for _, expression := range expressions {
		c.expr(expression, env)
	}
	finish()
}
