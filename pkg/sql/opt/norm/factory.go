// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package norm

import (
	"fmt"
	"math"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/util"
)

// MatchedRuleFunc defines the callback function for the NotifyOnMatchedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been matched. The name of the
// matched rule is passed as a parameter. If the function returns false, then
// the rule is not applied (i.e. skipped).
type MatchedRuleFunc func(ruleName opt.RuleName) bool

// AppliedRuleFunc defines the callback function for the NotifyOnAppliedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been applied. The function is
// called with the name of the rule and the memo group it affected. If the rule
// was an exploration rule, then the added parameter gives the number of
// expressions added to the group by the rule.
type AppliedRuleFunc func(ruleName opt.RuleName, group memo.GroupID, added int)

// Factory constructs a normalized expression tree within the memo. As each
// kind of expression is constructed by the factory, it transitively runs
// normalization transformations defined for that expression type. This may
// result in the construction of a different type of expression than what was
// requested. If, after normalization, the expression is already part of the
// memo, then construction is a no-op. Otherwise, a new memo group is created,
// with the normalized expression as its first and only expression.
//
// The result of calling each Factory Construct method is the id of the group
// that was constructed. Callers can access the normalized expression tree that
// the factory constructs by creating a memo.ExprView, like this:
//
//   ev := memo.MakeNormExprView(f.Memo(), group)
//
// Factory is largely auto-generated by optgen. The generated code can be found
// in factory.og.go. The factory.go file contains helper functions that are
// invoked by normalization patterns. While most patterns are specified in the
// optgen DSL, the factory always calls the `onConstruct` method as its last
// step, in order to allow any custom manual code to execute.
type Factory struct {
	mem     *memo.Memo
	evalCtx *tree.EvalContext
	props   rulePropsBuilder

	// scratchItems is a slice that is reused by listBuilder to store temporary
	// results that are accumulated before passing them to InternList.
	scratchItems []memo.GroupID

	// ruleCycles is used to detect cyclical rule invocations. Each rule with
	// the "DetectCycles" tag adds its expression fingerprint into this map
	// before constructing its replacement. If the replacement pattern recursively
	// invokes the same rule (or another rule with the DetectCycles tag) with that
	// same fingerprint, then the rule sees that the fingerprint is already in the
	// map, and will skip application of the rule.
	ruleCycles map[memo.Fingerprint]bool

	// matchedRule is the callback function that is invoked each time a normalize
	// rule has been matched by the factory. It can be set via a call to the
	// NotifyOnMatchedRule method.
	matchedRule MatchedRuleFunc

	// appliedRule is the callback function which is invoked each time a normalize
	// rule has been applied by the factory. It can be set via a call to the
	// NotifyOnAppliedRule method.
	appliedRule AppliedRuleFunc
}

// NewFactory returns a new Factory structure with a new, blank memo structure
// inside.
func NewFactory(evalCtx *tree.EvalContext) *Factory {
	mem := memo.New()
	return &Factory{
		mem:        mem,
		evalCtx:    evalCtx,
		props:      rulePropsBuilder{mem: mem},
		ruleCycles: make(map[memo.Fingerprint]bool),
	}
}

// DisableOptimizations disables all transformation rules. The unaltered input
// expression tree becomes the output expression tree (because no transforms
// are applied).
func (f *Factory) DisableOptimizations() {
	f.NotifyOnMatchedRule(func(opt.RuleName) bool { return false })
}

// NotifyOnMatchedRule sets a callback function which is invoked each time a
// normalize rule has been matched by the factory. If matchedRule is nil, then
// no further notifications are sent, and all rules are applied by default. In
// addition, callers can invoke the DisableOptimizations convenience method to
// disable all rules.
func (f *Factory) NotifyOnMatchedRule(matchedRule MatchedRuleFunc) {
	f.matchedRule = matchedRule
}

// NotifyOnAppliedRule sets a callback function which is invoked each time a
// normalize rule has been applied by the factory. If appliedRule is nil, then
// no further notifications are sent.
func (f *Factory) NotifyOnAppliedRule(appliedRule AppliedRuleFunc) {
	f.appliedRule = appliedRule
}

// Memo returns the memo structure that the factory is operating upon.
func (f *Factory) Memo() *memo.Memo {
	return f.mem
}

// Metadata returns the query-specific metadata, which includes information
// about the columns and tables used in this particular query.
func (f *Factory) Metadata() *opt.Metadata {
	return f.mem.Metadata()
}

// ConstructSimpleProject is a convenience wrapper for calling
// ConstructProject when there are no synthesized columns.
func (f *Factory) ConstructSimpleProject(
	input memo.GroupID, passthroughCols opt.ColSet,
) memo.GroupID {
	def := memo.ProjectionsOpDef{PassthroughCols: passthroughCols}
	return f.ConstructProject(
		input,
		f.ConstructProjections(memo.EmptyList, f.InternProjectionsOpDef(&def)),
	)
}

// InternList adds the given list of group IDs to memo storage and returns an
// ID that can be used for later lookup. If the same list was added previously,
// this method is a no-op and returns the ID of the previous value.
func (f *Factory) InternList(items []memo.GroupID) memo.ListID {
	return f.mem.InternList(items)
}

// onConstruct is called as a final step by each factory construction method,
// so that any custom manual pattern matching/replacement code can be run.
func (f *Factory) onConstruct(e memo.Expr) memo.GroupID {
	// RaceEnabled ensures that checks are run on every change (as part of make
	// testrace) while keeping the check code out of non-test builds.
	// TODO(radu): replace this with a flag that is true for all tests.
	if util.RaceEnabled {
		f.checkExpr(e)
	}
	group := f.mem.MemoizeNormExpr(f.evalCtx, e)
	f.props.buildProps(memo.MakeNormExprView(f.mem, group))
	return group
}

// ----------------------------------------------------------------------
//
// Private extraction functions
//   Helper functions that make extracting common private types easier.
//
// ----------------------------------------------------------------------

func (f *Factory) extractColID(private memo.PrivateID) opt.ColumnID {
	return f.mem.LookupPrivate(private).(opt.ColumnID)
}

func (f *Factory) extractColList(private memo.PrivateID) opt.ColList {
	return f.mem.LookupPrivate(private).(opt.ColList)
}

func (f *Factory) extractOrdering(private memo.PrivateID) props.Ordering {
	return f.mem.LookupPrivate(private).(props.Ordering)
}

// ----------------------------------------------------------------------
//
// List functions
//   General custom match and replace functions used to test and construct
//   lists.
//
// ----------------------------------------------------------------------

// listOnlyHasNulls if every item in the given list is a Null op. If the list
// is empty, listOnlyHasNulls returns false.
func (f *Factory) listOnlyHasNulls(list memo.ListID) bool {
	if list.Length == 0 {
		return false
	}

	for _, item := range f.mem.LookupList(list) {
		if f.mem.NormExpr(item).Operator() != opt.NullOp {
			return false
		}
	}
	return true
}

// removeListItem returns a new list that is a copy of the given list, except
// that it does not contain the given search item. If the list contains the item
// multiple times, then only the first instance is removed. If the list does not
// contain the item, then the method is a no-op.
func (f *Factory) removeListItem(list memo.ListID, search memo.GroupID) memo.ListID {
	existingList := f.mem.LookupList(list)
	b := listBuilder{f: f}
	for i, item := range existingList {
		if item == search {
			b.addItems(existingList[i+1:])
			break
		}
		b.addItem(item)
	}
	return b.buildList()
}

// replaceListItem returns a new list that is a copy of the given list, except
// that the given search item has been replaced by the given replace item. If
// the list contains the search item multiple times, then only the first
// instance is replaced. If the list does not contain the item, then the method
// is a no-op.
func (f *Factory) replaceListItem(list memo.ListID, search, replace memo.GroupID) memo.ListID {
	existingList := f.mem.LookupList(list)
	b := listBuilder{f: f}
	for i, item := range existingList {
		if item == search {
			// Replace item and copy remainder of list.
			b.addItem(replace)
			b.addItems(existingList[i+1:])
			break
		}
		b.addItem(item)
	}
	return b.buildList()
}

// internSingletonList interns a list containing the single given item and
// returns its id.
func (f *Factory) internSingletonList(item memo.GroupID) memo.ListID {
	b := listBuilder{f: f}
	b.addItem(item)
	return b.buildList()
}

// isSortedUniqueList returns true if the list is in sorted order, with no
// duplicates. See the comment for listSorter.compare for comparison rule
// details.
func (f *Factory) isSortedUniqueList(list memo.ListID) bool {
	ls := listSorter{f: f, list: f.mem.LookupList(list)}
	for i := 0; i < int(list.Length-1); i++ {
		if !ls.less(i, i+1) {
			return false
		}
	}
	return true
}

// constructSortedUniqueList sorts the given list and removes duplicates, and
// returns the resulting list. See the comment for listSorter.compare for
// comparison rule details.
func (f *Factory) constructSortedUniqueList(list memo.ListID) memo.ListID {
	// Make a copy of the list, since it needs to stay immutable.
	lb := listBuilder{f: f}
	lb.addItems(f.mem.LookupList(list))
	ls := listSorter{f: f, list: lb.items}

	// Sort the list.
	sort.Slice(ls.list, ls.less)

	// Remove duplicates from the list.
	n := 0
	for i := 0; i < int(list.Length); i++ {
		if i == 0 || ls.compare(i-1, i) < 0 {
			lb.items[n] = lb.items[i]
			n++
		}
	}
	lb.setLength(n)
	return lb.buildList()
}

// listSorter is a helper struct that implements the sort.Slice "less"
// comparison function.
type listSorter struct {
	f    *Factory
	list []memo.GroupID
}

// less returns true if item i in the list compares less than item j.
// sort.Slice uses this method to sort the list.
func (s listSorter) less(i, j int) bool {
	return s.compare(i, j) < 0
}

// compare returns -1 if item i compares less than item j, 0 if they are equal,
// and 1 if item i compares greater. Constants sort before non-constants, and
// are sorted and uniquified according to Datum comparison rules. Non-constants
// are sorted and uniquified by GroupID (arbitrary, but stable).
func (s listSorter) compare(i, j int) int {
	// If both are constant values, then use datum comparison.
	isLeftConst := s.f.mem.NormExpr(s.list[i]).IsConstValue()
	isRightConst := s.f.mem.NormExpr(s.list[j]).IsConstValue()
	if isLeftConst {
		if !isRightConst {
			// Constant always sorts before non-constant
			return -1
		}

		leftD := memo.ExtractConstDatum(memo.MakeNormExprView(s.f.mem, s.list[i]))
		rightD := memo.ExtractConstDatum(memo.MakeNormExprView(s.f.mem, s.list[j]))
		return leftD.Compare(s.f.evalCtx, rightD)
	} else if isRightConst {
		// Non-constant always sorts after constant.
		return 1
	}

	// Arbitrarily order by GroupID.
	if s.list[i] < s.list[j] {
		return -1
	} else if s.list[i] > s.list[j] {
		return 1
	}
	return 0
}

// ----------------------------------------------------------------------
//
// Typing functions
//   General custom match and replace functions used to test and construct
//   expression data types.
//
// ----------------------------------------------------------------------

// hasType returns true if the given expression has a static type that's
// equivalent to the requested type.
func (f *Factory) hasType(group memo.GroupID, typ memo.PrivateID) bool {
	groupType := f.lookupScalar(group).Type
	requestedType := f.mem.LookupPrivate(typ).(types.T)
	return groupType.Equivalent(requestedType)
}

// boolType returns the private ID of the boolean SQL type.
func (f *Factory) boolType() memo.PrivateID {
	return f.InternType(types.Bool)
}

// canConstructBinary returns true if (op left right) has a valid binary op
// overload and is therefore legal to construct. For example, while
// (Minus <date> <int>) is valid, (Minus <int> <date>) is not.
func (f *Factory) canConstructBinary(op opt.Operator, left, right memo.GroupID) bool {
	leftType := f.lookupScalar(left).Type
	rightType := f.lookupScalar(right).Type
	return memo.BinaryOverloadExists(opt.MinusOp, rightType, leftType)
}

// ----------------------------------------------------------------------
//
// Property functions
//   General custom match and replace functions used to test expression
//   logical properties.
//
// ----------------------------------------------------------------------

// operator returns the type of the given group's normalized expression.
func (f *Factory) operator(group memo.GroupID) opt.Operator {
	return f.mem.NormExpr(group).Operator()
}

// lookupLogical returns the given group's logical properties.
func (f *Factory) lookupLogical(group memo.GroupID) *props.Logical {
	return f.mem.GroupProperties(group)
}

// lookupRelational returns the given group's logical relational properties.
func (f *Factory) lookupRelational(group memo.GroupID) *props.Relational {
	return f.lookupLogical(group).Relational
}

// lookupScalar returns the given group's logical scalar properties.
func (f *Factory) lookupScalar(group memo.GroupID) *props.Scalar {
	return f.lookupLogical(group).Scalar
}

// outputCols is a helper function that extracts the set of columns projected
// by the given operator. In addition to extracting columns from any relational
// operator, outputCols can also extract columns from the Projections and
// Aggregations scalar operators, which are used with Project and GroupBy.
func (f *Factory) outputCols(group memo.GroupID) opt.ColSet {
	// Handle columns projected by relational operators.
	logical := f.lookupLogical(group)
	if logical.Relational != nil {
		return f.lookupRelational(group).OutputCols
	}

	expr := f.mem.NormExpr(group)
	switch expr.Operator() {
	case opt.AggregationsOp:
		return opt.ColListToSet(f.extractColList(expr.AsAggregations().Cols()))

	case opt.ProjectionsOp:
		return f.mem.LookupPrivate(expr.AsProjections().Def()).(*memo.ProjectionsOpDef).AllCols()

	default:
		panic(fmt.Sprintf("outputCols doesn't support op %s", expr.Operator()))
	}
}

// outerCols returns the set of outer columns associated with the given group,
// whether it be a relational or scalar operator.
func (f *Factory) outerCols(group memo.GroupID) opt.ColSet {
	return f.lookupLogical(group).OuterCols()
}

// hasOuterCols returns true if the given group has at least one outer column,
// or in other words, a reference to a variable that is not bound within its
// own scope. For example:
//
//   SELECT * FROM a WHERE EXISTS(SELECT * FROM b WHERE b.x = a.x)
//
// The a.x variable in the EXISTS subquery references a column outside the scope
// of the subquery. It is an "outer column" for the subquery (see the comment on
// RelationalProps.OuterCols for more details).
func (f *Factory) hasOuterCols(group memo.GroupID) bool {
	return !f.outerCols(group).Empty()
}

// onlyConstants returns true if the scalar expression is a "constant
// expression tree", meaning that it will always evaluate to the same result.
// See the CommuteConst pattern comment for more details.
func (f *Factory) onlyConstants(group memo.GroupID) bool {
	// TODO(andyk): Consider impact of "impure" functions with side effects.
	return f.lookupScalar(group).OuterCols.Empty()
}

// hasNoCols returns true if the group has zero output columns.
func (f *Factory) hasNoCols(group memo.GroupID) bool {
	return f.outputCols(group).Empty()
}

// hasSameCols returns true if the two groups have an identical set of output
// columns.
func (f *Factory) hasSameCols(left, right memo.GroupID) bool {
	return f.outputCols(left).Equals(f.outputCols(right))
}

// hasSubsetCols returns true if the left group's output columns are a subset of
// the right group's output columns.
func (f *Factory) hasSubsetCols(left, right memo.GroupID) bool {
	return f.outputCols(left).SubsetOf(f.outputCols(right))
}

// HasZeroRows returns true if the given group never returns any rows.
func (f *Factory) hasZeroRows(group memo.GroupID) bool {
	return f.mem.GroupProperties(group).Relational.Cardinality.IsZero()
}

// hasOneRow returns true if the given group always returns exactly one row.
func (f *Factory) hasOneRow(group memo.GroupID) bool {
	return f.mem.GroupProperties(group).Relational.Cardinality.IsOne()
}

// hasZeroOrOneRow returns true if the given group returns at most one row.
func (f *Factory) hasZeroOrOneRow(group memo.GroupID) bool {
	return f.mem.GroupProperties(group).Relational.Cardinality.IsZeroOrOne()
}

// hasOneOrMoreRows returns true if the given group will always return at least
// one row.
func (f *Factory) hasOneOrMoreRows(group memo.GroupID) bool {
	return !f.mem.GroupProperties(group).Relational.Cardinality.CanBeZero()
}

// hasCorrelatedSubquery returns true if the given scalar group contains a
// subquery within its subtree that has at least one outer column.
func (f *Factory) hasCorrelatedSubquery(group memo.GroupID) bool {
	return f.lookupScalar(group).HasCorrelatedSubquery
}

// shortestKey returns the strong key in the given memo group that is composed
// of the fewest columns. If there are multiple keys with the same number of
// columns, any one of them may be returned. If there are no strong keys in the
// group, then shortestKey returns ok=false.
func (f *Factory) shortestKey(group memo.GroupID) (key opt.ColSet, ok bool) {
	var shortest opt.ColSet
	var shortestLen int
	props := f.lookupLogical(group).Relational
	for _, wk := range props.WeakKeys {
		// A strong key requires all columns to be non-nullable.
		if wk.SubsetOf(props.NotNullCols) {
			l := wk.Len()
			if !ok || l < shortestLen {
				shortestLen = l
				shortest = wk
				ok = true
			}
		}
	}
	return shortest, ok
}

// ensureKey finds the shortest strong key for the input memo group. If no
// strong key exists, then ensureKey wraps the input in a RowNumber operator,
// which provides a key column by uniquely numbering the rows. ensureKey returns
// the input group (perhaps wrapped by RowNumber) and the set of columns that
// form the shortest available key.
func (f *Factory) ensureKey(in memo.GroupID) (out memo.GroupID, key opt.ColSet) {
	key, ok := f.shortestKey(in)
	if ok {
		return in, key
	}

	colID := f.Metadata().AddColumn("rownum", types.Int)
	def := &memo.RowNumberDef{ColID: colID}
	out = f.ConstructRowNumber(in, f.InternRowNumberDef(def))
	key.Add(int(colID))
	return out, key
}

// ----------------------------------------------------------------------
//
// Projection construction functions
//   General helper functions to construct Projections.
//
// ----------------------------------------------------------------------

// projectColsFrom returns a Projections operator that projects the output
// columns from the given group as passthrough columns. If the group is already
// a Projections operator, then projectColsFrom is a no-op.
func (f *Factory) projectColsFrom(group memo.GroupID) memo.GroupID {
	// If group is already a ProjectionsOp, then no more to do.
	if f.mem.NormExpr(group).Operator() == opt.ProjectionsOp {
		return group
	}

	def := memo.ProjectionsOpDef{PassthroughCols: f.lookupLogical(group).Relational.OutputCols}
	return f.ConstructProjections(memo.EmptyList, f.InternProjectionsOpDef(&def))
}

// projectColsFromBoth returns a Projections operator that combines distinct
// columns from both the provided left and right groups. If the group is a
// Projections operator, then the projected expressions will be directly added
// to the new Projections operator. Otherwise, the group's output columns will
// be added as passthrough columns to the new Projections operator.
func (f *Factory) projectColsFromBoth(left, right memo.GroupID) memo.GroupID {
	leftCols := f.outputCols(left)
	rightCols := f.outputCols(right)
	if leftCols.SubsetOf(rightCols) {
		return f.projectColsFrom(right)
	}
	if rightCols.SubsetOf(leftCols) {
		return f.projectColsFrom(left)
	}

	synthLen := 0
	if f.operator(left) == opt.ProjectionsOp {
		synthLen += leftCols.Len()
	}
	if f.operator(right) == opt.ProjectionsOp {
		synthLen += rightCols.Len()
	}

	remaining := leftCols.Union(rightCols)
	elems := make([]memo.GroupID, 0, synthLen)
	def := memo.ProjectionsOpDef{SynthesizedCols: make(opt.ColList, 0, synthLen)}

	// Define function that can append either existing Projections columns or
	// synthesize new columns from other operators' output columns.
	appendCols := func(group memo.GroupID, groupCols opt.ColSet) {
		projectionsExpr := f.mem.NormExpr(group).AsProjections()
		if projectionsExpr != nil {
			// Projections case.
			projectionsElems := f.mem.LookupList(projectionsExpr.Elems())
			projectionsDef := f.mem.LookupPrivate(projectionsExpr.Def()).(*memo.ProjectionsOpDef)

			// Add synthesized columns.
			for i := 0; i < len(projectionsDef.SynthesizedCols); i++ {
				colID := projectionsDef.SynthesizedCols[i]
				if remaining.Contains(int(colID)) {
					elems = append(elems, projectionsElems[i])
					def.SynthesizedCols = append(def.SynthesizedCols, colID)
				}
			}

			// Add pass-through columns.
			def.PassthroughCols.UnionWith(projectionsDef.PassthroughCols)
		} else {
			// Non-projections case.
			def.PassthroughCols.UnionWith(groupCols)
		}

		// Remove all appended columns from the remaining column set.
		remaining.DifferenceWith(groupCols)
	}

	appendCols(left, leftCols)
	appendCols(right, rightCols)
	return f.ConstructProjections(f.mem.InternList(elems), f.mem.InternProjectionsOpDef(&def))
}

// projectExtraCol constructs a new Project operator that passes through all
// columns in the given "in" expression, and then adds the given "extra"
// expression as an additional column.
func (f *Factory) projectExtraCol(in, extra memo.GroupID, extraID opt.ColumnID) memo.GroupID {
	return f.ConstructProject(
		in,
		f.ConstructProjections(
			f.InternList([]memo.GroupID{extra}),
			f.InternProjectionsOpDef(&memo.ProjectionsOpDef{
				PassthroughCols: f.outputCols(in),
				SynthesizedCols: opt.ColList{extraID},
			}),
		),
	)
}

// ----------------------------------------------------------------------
//
// Select Rules
//   Custom match and replace functions used with select.opt rules.
//
// ----------------------------------------------------------------------

// isCorrelated returns true if any variable in the source expression references
// a column from the destination expression. For example:
//   (InnerJoin
//     (Scan a)
//     (Scan b)
//     (Eq (Variable a.x) (Const 1))
//   )
//
// The (Eq) expression is correlated with the (Scan a) expression because it
// references one of its columns. But the (Eq) expression is not correlated
// with the (Scan b) expression.
func (f *Factory) isCorrelated(src, dst memo.GroupID) bool {
	return f.outerCols(src).Intersects(f.outputCols(dst))
}

// isBoundBy returns true if all outer references in the source expression are
// bound by the destination expression. For example:
//
//   (InnerJoin
//     (Scan a)
//     (Scan b)
//     (Eq (Variable a.x) (Const 1))
//   )
//
// The (Eq) expression is fully bound by the (Scan a) expression because all of
// its outer references are satisfied by the columns produced by the Scan.
func (f *Factory) isBoundBy(src, dst memo.GroupID) bool {
	return f.outerCols(src).SubsetOf(f.outputCols(dst))
}

// extractBoundConditions returns a new list containing only those expressions
// from the given list that are fully bound by the given expression (i.e. all
// outer references are satisfied by it). For example:
//
//   (InnerJoin
//     (Scan a)
//     (Scan b)
//     (Filters [
//       (Eq (Variable a.x) (Variable b.x))
//       (Gt (Variable a.x) (Const 1))
//     ])
//   )
//
// Calling extractBoundConditions with the filter conditions list and the output
// columns of (Scan a) would extract the (Gt) expression, since its outer
// references only reference columns from a.
func (f *Factory) extractBoundConditions(list memo.ListID, group memo.GroupID) memo.ListID {
	lb := listBuilder{f: f}
	for _, item := range f.mem.LookupList(list) {
		if f.isBoundBy(item, group) {
			lb.addItem(item)
		}
	}
	return lb.buildList()
}

// extractUnboundConditions is the inverse of extractBoundConditions. Instead of
// extracting expressions that are bound by the given expression, it extracts
// list expressions that have at least one outer reference that is *not* bound
// by the given expression (i.e. it has a "free" variable).
func (f *Factory) extractUnboundConditions(list memo.ListID, group memo.GroupID) memo.ListID {
	lb := listBuilder{f: f}
	for _, item := range f.mem.LookupList(list) {
		if !f.isBoundBy(item, group) {
			lb.addItem(item)
		}
	}
	return lb.buildList()
}

// concatFilters creates a new Filters operator that contains conditions from
// both the left and right boolean filter expressions. If the left or right
// expression is itself a Filters operator, then it is "flattened" by merging
// its conditions into the new Filters operator.
func (f *Factory) concatFilters(left, right memo.GroupID) memo.GroupID {
	leftExpr := f.mem.NormExpr(left)
	rightExpr := f.mem.NormExpr(right)

	// Handle cases where left/right filters are constant boolean values.
	if leftExpr.Operator() == opt.TrueOp || rightExpr.Operator() == opt.FalseOp {
		return right
	}
	if rightExpr.Operator() == opt.TrueOp || leftExpr.Operator() == opt.FalseOp {
		return left
	}

	// Determine how large to make the conditions slice (at least 2 slots).
	cnt := 2
	leftFiltersExpr := leftExpr.AsFilters()
	if leftFiltersExpr != nil {
		cnt += int(leftFiltersExpr.Conditions().Length) - 1
	}
	rightFiltersExpr := rightExpr.AsFilters()
	if rightFiltersExpr != nil {
		cnt += int(rightFiltersExpr.Conditions().Length) - 1
	}

	// Create the conditions slice and populate it.
	lb := listBuilder{f: f}
	if leftFiltersExpr != nil {
		lb.addItems(f.mem.LookupList(leftFiltersExpr.Conditions()))
	} else {
		lb.addItem(left)
	}
	if rightFiltersExpr != nil {
		lb.addItems(f.mem.LookupList(rightFiltersExpr.Conditions()))
	} else {
		lb.addItem(right)
	}
	return f.ConstructFilters(lb.buildList())
}

// ----------------------------------------------------------------------
//
// Join Rules
//   Custom match and replace functions used with join.opt rules.
//
// ----------------------------------------------------------------------

// constructNonLeftJoin maps a left join to an inner join and a full join to a
// right join when it can be proved that the right side of the join always
// produces at least one row for every row on the left.
func (f *Factory) constructNonLeftJoin(
	joinOp opt.Operator, left, right, on memo.GroupID,
) memo.GroupID {
	switch joinOp {
	case opt.LeftJoinOp:
		return f.ConstructInnerJoin(left, right, on)
	case opt.LeftJoinApplyOp:
		return f.ConstructInnerJoinApply(left, right, on)
	case opt.FullJoinOp:
		return f.ConstructRightJoin(left, right, on)
	case opt.FullJoinApplyOp:
		return f.ConstructRightJoinApply(left, right, on)
	}
	panic(fmt.Sprintf("unexpected join operator: %v", joinOp))
}

// constructNonRightJoin maps a right join to an inner join and a full join to a
// left join when it can be proved that the left side of the join always
// produces at least one row for every row on the right.
func (f *Factory) constructNonRightJoin(
	joinOp opt.Operator, left, right, on memo.GroupID,
) memo.GroupID {
	switch joinOp {
	case opt.RightJoinOp:
		return f.ConstructInnerJoin(left, right, on)
	case opt.RightJoinApplyOp:
		return f.ConstructInnerJoinApply(left, right, on)
	case opt.FullJoinOp:
		return f.ConstructLeftJoin(left, right, on)
	case opt.FullJoinApplyOp:
		return f.ConstructLeftJoinApply(left, right, on)
	}
	panic(fmt.Sprintf("unexpected join operator: %v", joinOp))
}

// ----------------------------------------------------------------------
//
// GroupBy Rules
//   Custom match and replace functions used with groupby.opt rules.
//
// ----------------------------------------------------------------------

// colsAreKey returns true if the given columns form a strong key for the output
// rows of the given group. A strong key means that the set of given column
// values are unique and not null.
func (f *Factory) colsAreKey(cols memo.PrivateID, group memo.GroupID) bool {
	colSet := f.mem.LookupPrivate(cols).(opt.ColSet)
	props := f.lookupLogical(group).Relational
	for _, weakKey := range props.WeakKeys {
		if weakKey.SubsetOf(colSet) && weakKey.SubsetOf(props.NotNullCols) {
			return true
		}
	}
	return false
}

// isScalarGroupBy returns true if the given grouping columns come from a
// "scalar" GroupBy operator. A scalar GroupBy always returns exactly one row,
// with any aggregate functions operating over the entire input expression.
func (f *Factory) isScalarGroupBy(groupingCols memo.PrivateID) bool {
	return f.mem.LookupPrivate(groupingCols).(opt.ColSet).Empty()
}

// ----------------------------------------------------------------------
//
// Limit Rules
//   Custom match and replace functions used with limit.opt rules.
//
// ----------------------------------------------------------------------

// limitGeMaxRows returns true if the given constant limit value is greater than
// or equal to the max number of rows returned by the input group.
func (f *Factory) limitGeMaxRows(limit memo.PrivateID, input memo.GroupID) bool {
	limitVal := int64(*f.mem.LookupPrivate(limit).(*tree.DInt))
	maxRows := f.mem.GroupProperties(input).Relational.Cardinality.Max
	return limitVal >= 0 && maxRows < math.MaxUint32 && limitVal >= int64(maxRows)
}

// ----------------------------------------------------------------------
//
// Boolean Rules
//   Custom match and replace functions used with bool.opt rules.
//
// ----------------------------------------------------------------------

// simplifyAnd removes True operands from an And operator, and eliminates the
// And operator altogether if any operand is False. It also "flattens" any And
// operator child by merging its conditions into the top-level list. Only one
// level of flattening is necessary, since this pattern would have already
// matched any And operator children. If, after simplification, no operands
// remain, then simplifyAnd returns True.
func (f *Factory) simplifyAnd(conditions memo.ListID) memo.GroupID {
	lb := listBuilder{f: f}
	for _, item := range f.mem.LookupList(conditions) {
		itemExpr := f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.AndOp:
			// Flatten nested And operands.
			lb.addItems(f.mem.LookupList(itemExpr.AsAnd().Conditions()))

		case opt.TrueOp:
			// And operator skips True operands.

		case opt.FalseOp:
			// Entire And evaluates to False if any operand is False.
			return item

		default:
			lb.addItem(item)
		}
	}

	if len(lb.items) == 0 {
		return f.ConstructTrue()
	}
	return f.ConstructAnd(lb.buildList())
}

// simplifyOr removes False operands from an Or operator, and eliminates the Or
// operator altogether if any operand is True. It also "flattens" any Or
// operator child by merging its conditions into the top-level list. Only one
// level of flattening is necessary, since this pattern would have already
// matched any Or operator children. If, after simplification, no operands
// remain, then simplifyOr returns False.
func (f *Factory) simplifyOr(conditions memo.ListID) memo.GroupID {
	lb := listBuilder{f: f}
	for _, item := range f.mem.LookupList(conditions) {
		itemExpr := f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.OrOp:
			// Flatten nested Or operands.
			lb.addItems(f.mem.LookupList(itemExpr.AsOr().Conditions()))

		case opt.FalseOp:
			// Or operator skips False operands.

		case opt.TrueOp:
			// Entire Or evaluates to True if any operand is True.
			return item

		default:
			lb.addItem(item)
		}
	}

	if len(lb.items) == 0 {
		return f.ConstructFalse()
	}
	return f.ConstructOr(lb.buildList())
}

// simplifyFilters behaves the same way as simplifyAnd, with one addition: if
// the conditions include a Null value in any position, then the entire
// expression is False. This works because the Filters expression only appears
// as a Select or Join filter condition, both of which treat a Null filter
// conjunct exactly as if it were False.
func (f *Factory) simplifyFilters(conditions memo.ListID) memo.GroupID {
	lb := listBuilder{f: f}
	for _, item := range f.mem.LookupList(conditions) {
		itemExpr := f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.AndOp:
			// Flatten nested And operands.
			lb.addItems(f.mem.LookupList(itemExpr.AsAnd().Conditions()))

		case opt.TrueOp:
			// Filters operator skips True operands.

		case opt.FalseOp:
			// Filters expression evaluates to False if any operand is False.
			return item

		case opt.NullOp:
			// Filters expression evaluates to False if any operand is False.
			return f.ConstructFalse()

		default:
			lb.addItem(item)
		}
	}

	if len(lb.items) == 0 {
		return f.ConstructTrue()
	}
	return f.ConstructFilters(lb.buildList())
}

func (f *Factory) negateConditions(conditions memo.ListID) memo.ListID {
	lb := listBuilder{f: f}
	list := f.mem.LookupList(conditions)
	for i := range list {
		lb.addItem(f.ConstructNot(list[i]))
	}
	return lb.buildList()
}

// negateComparison negates a comparison op like:
//   a.x = 5
// to:
//   a.x <> 5
func (f *Factory) negateComparison(cmp opt.Operator, left, right memo.GroupID) memo.GroupID {
	negate := opt.NegateOpMap[cmp]
	operands := memo.DynamicOperands{memo.DynamicID(left), memo.DynamicID(right)}
	return f.DynamicConstruct(negate, operands)
}

// commuteInequality swaps the operands of an inequality comparison expression,
// changing the operator to compensate:
//   5 < x
// to:
//   x > 5
func (f *Factory) commuteInequality(op opt.Operator, left, right memo.GroupID) memo.GroupID {
	switch op {
	case opt.GeOp:
		return f.ConstructLe(right, left)
	case opt.GtOp:
		return f.ConstructLt(right, left)
	case opt.LeOp:
		return f.ConstructGe(right, left)
	case opt.LtOp:
		return f.ConstructGt(right, left)
	}
	panic(fmt.Sprintf("called commuteInequality with operator %s", op))
}

// ----------------------------------------------------------------------
//
// Comparison Rules
//   Custom match and replace functions used with comp.opt rules.
//
// ----------------------------------------------------------------------

// normalizeTupleEquality remaps the elements of two tuples compared for
// equality, like this:
//   (a, b, c) = (x, y, z)
// into this:
//   (a = x) AND (b = y) AND (c = z)
func (f *Factory) normalizeTupleEquality(left, right memo.ListID) memo.GroupID {
	if left.Length != right.Length {
		panic("tuple length mismatch")
	}

	lb := listBuilder{f: f}
	leftList := f.mem.LookupList(left)
	rightList := f.mem.LookupList(right)
	for i := 0; i < len(leftList); i++ {
		lb.addItem(f.ConstructEq(leftList[i], rightList[i]))
	}
	return f.ConstructAnd(lb.buildList())
}

// ----------------------------------------------------------------------
//
// Scalar Rules
//   Custom match and replace functions used with scalar.opt rules.
//
// ----------------------------------------------------------------------

// simplifyCoalesce discards any leading null operands, and then if the next
// operand is a constant, replaces with that constant.
func (f *Factory) simplifyCoalesce(args memo.ListID) memo.GroupID {
	argList := f.mem.LookupList(args)
	for i := 0; i < int(args.Length-1); i++ {
		// If item is not a constant value, then its value may turn out to be
		// null, so no more folding. Return operands from then on.
		item := f.mem.NormExpr(argList[i])
		if !item.IsConstValue() {
			return f.ConstructCoalesce(f.InternList(argList[i:]))
		}

		if item.Operator() != opt.NullOp {
			return argList[i]
		}
	}

	// All operands up to the last were null (or the last is the only operand),
	// so return the last operand without the wrapping COALESCE function.
	return argList[args.Length-1]
}

// allowNullArgs returns true if the binary operator with the given inputs
// allows one of those inputs to be null. If not, then the binary operator will
// simply be replaced by null.
func (f *Factory) allowNullArgs(op opt.Operator, left, right memo.GroupID) bool {
	leftType := f.lookupScalar(left).Type
	rightType := f.lookupScalar(right).Type
	return memo.BinaryAllowsNullArgs(op, leftType, rightType)
}

// foldNullUnary replaces the unary operator with a typed null value having the
// same type as the unary operator would have.
func (f *Factory) foldNullUnary(op opt.Operator, input memo.GroupID) memo.GroupID {
	typ := f.lookupScalar(input).Type
	return f.ConstructNull(f.InternType(memo.InferUnaryType(op, typ)))
}

// foldNullBinary replaces the binary operator with a typed null value having
// the same type as the binary operator would have.
func (f *Factory) foldNullBinary(op opt.Operator, left, right memo.GroupID) memo.GroupID {
	leftType := f.lookupScalar(left).Type
	rightType := f.lookupScalar(right).Type
	return f.ConstructNull(f.InternType(memo.InferBinaryType(op, leftType, rightType)))
}

// ----------------------------------------------------------------------
//
// Numeric Rules
//   Custom match and replace functions used with numeric.opt rules.
//
// ----------------------------------------------------------------------

// isZero returns true if the input expression is a numeric constant with a
// value of zero.
func (f *Factory) isZero(input memo.GroupID) bool {
	d := f.mem.LookupPrivate(f.mem.NormExpr(input).AsConst().Value()).(tree.Datum)
	switch t := d.(type) {
	case *tree.DDecimal:
		return t.Decimal.Sign() == 0
	case *tree.DFloat:
		return *t == 0
	case *tree.DInt:
		return *t == 0
	}
	return false
}

// isOne returns true if the input expression is a numeric constant with a
// value of one.
func (f *Factory) isOne(input memo.GroupID) bool {
	d := f.mem.LookupPrivate(f.mem.NormExpr(input).AsConst().Value()).(tree.Datum)
	switch t := d.(type) {
	case *tree.DDecimal:
		return t.Decimal.Cmp(&tree.DecimalOne.Decimal) == 0
	case *tree.DFloat:
		return *t == 1.0
	case *tree.DInt:
		return *t == 1
	}
	return false
}
