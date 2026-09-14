# Parse only. Never import or execute proposed test code.
from __future__ import annotations
import ast
import json
import sys
from typing import TYPE_CHECKING, Callable, Optional, Union
# Annotations stay lazy and the aliases stay behind TYPE_CHECKING, so nothing
# here is evaluated at definition time. The operator supplies the interpreter
# (validate.go runs `python3 -I -S -c`), so a subscripted builtin evaluated at
# import would raise TypeError on any interpreter older than PEP 585.
if TYPE_CHECKING:
    Identity = tuple[tuple[str, str, int], ...]
    Definition = Union[ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef]
    Owner = Union[ast.Module, ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef]
    Bindings = dict[str, Optional[str]]
    DefinitionConstraint = Callable[['Comparison', Identity, str], None]
    ModuleConstraint = Callable[['Comparison'], None]
# Decorators whose identity survives while their arguments adapt: patch targets
# name the migrated source. Every other test decorator must remain unchanged.
ADAPTABLE_DECORATORS = frozenset({
    'unittest.mock.patch', 'unittest.mock.patch.object', 'unittest.mock.patch.dict',
    'mock.patch', 'mock.patch.object', 'mock.patch.dict',
})
# Named only to explain a refusal; an unlisted decorator change is refused too.
BYPASS_DECORATORS = frozenset({
    'unittest.skip', 'unittest.skipIf', 'unittest.skipUnless', 'unittest.expectedFailure',
    'unittest.case.skip', 'unittest.case.skipIf', 'unittest.case.skipUnless', 'unittest.case.expectedFailure',
    'pytest.mark.skip', 'pytest.mark.skipif', 'pytest.mark.xfail', 'pytest.fixture', 'pytest.yield_fixture',
})
SKIP_CALLS = frozenset({'pytest.skip', 'pytest.xfail', 'pytest.importorskip', 'unittest.case.SkipTest', 'unittest.SkipTest'})
SKIP_RAISES = frozenset({'unittest.SkipTest', 'unittest.case.SkipTest', 'pytest.skip.Exception', '_pytest.outcomes.Skipped'})
def definitions(tree: ast.AST) -> dict[Identity, Definition]:
    result: dict[Identity, Definition] = {}
    def visit(node: ast.AST, owner: Identity, occurrences: dict[tuple[str, str], int]) -> None:
        if isinstance(node, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            component = (type(node).__name__, node.name)
            occurrence = occurrences.get(component, 0) + 1
            occurrences[component] = occurrence
            owner = owner + ((component[0], component[1], occurrence),)
            result[owner] = node
            occurrences = {}
        # Statements such as if/try do not introduce a lexical owner.
        for child in ast.iter_child_nodes(node):
            visit(child, owner, occurrences)
    visit(tree, (), {})
    return result
def assertions(node: ast.AST) -> int:
    count = 0
    pending = [node]
    while pending:
        item = pending.pop()
        # Child definitions own their checks; compare them separately below.
        if item is not node and isinstance(item, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        pending.extend(ast.iter_child_nodes(item))
        if isinstance(item, (ast.Assert, ast.Raise)):
            count += 1
        if isinstance(item, ast.Call):
            callee = item.func
            if (isinstance(callee, ast.Name) and callee.id == 'fail') or (
                isinstance(callee, ast.Attribute) and (callee.attr.startswith('assert') or callee.attr == 'fail')):
                count += 1
    return count
def body(owner: Owner) -> list[ast.AST]:
    # The owner's own statements, stopping at child definitions and excluding
    # the owner's decorators, which are compared as decorators.
    result: list[ast.AST] = []
    pending: list[ast.AST] = list(owner.body)
    while pending:
        item = pending.pop()
        if isinstance(item, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        pending.extend(ast.iter_child_nodes(item))
        result.append(item)
    return result
def bindings(tree: ast.AST) -> Bindings:
    # Names an import supplies, resolved to the dotted name they import. A name
    # bound twice, or rebound by an assignment or definition anywhere in the
    # module, cannot be resolved without executing the module.
    result: Bindings = {}
    rebound: set[str] = set()
    def bind(local: str, target: str) -> None:
        if local in result and result[local] != target:
            result[local] = None
        else:
            result[local] = target
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                if alias.asname is None:
                    bind(alias.name.split('.')[0], alias.name.split('.')[0])
                else:
                    bind(alias.asname, alias.name)
        elif isinstance(node, ast.ImportFrom):
            package = '.' * node.level + (node.module + '.' if node.module else '')
            for alias in node.names:
                if alias.name != '*':
                    bind(alias.asname or alias.name, package + alias.name)
        elif isinstance(node, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            rebound.add(node.name)
        elif isinstance(node, ast.Name) and isinstance(node.ctx, ast.Store):
            rebound.add(node.id)
    for name in rebound:
        if name in result:
            result[name] = None
    return result
def resolve(expression: ast.expr, names: Bindings) -> Optional[str]:
    # The dotted name an expression refers to through import bindings. Calls
    # resolve to their callee. Anything rooted elsewhere is unresolvable.
    if isinstance(expression, ast.Call):
        return resolve(expression.func, names)
    if isinstance(expression, ast.Attribute):
        owner = resolve(expression.value, names)
        return None if owner is None else owner + '.' + expression.attr
    if isinstance(expression, ast.Name):
        return names.get(expression.id)
    return None
def decorators(owner: Owner) -> list[ast.expr]:
    if not isinstance(owner, ast.Module):
        return list(owner.decorator_list)
    # A module-level pytestmark applies to every test the module defines.
    result: list[ast.expr] = []
    for item in body(owner):
        targets: list[ast.expr] = []
        if isinstance(item, ast.Assign):
            targets = item.targets
        elif isinstance(item, (ast.AnnAssign, ast.AugAssign)):
            targets = [item.target]
        value = item.value if isinstance(item, (ast.Assign, ast.AnnAssign, ast.AugAssign)) else None
        if value is None or not any(isinstance(target, ast.Name) and target.id == 'pytestmark' for target in targets):
            continue
        result.extend(value.elts if isinstance(value, (ast.List, ast.Tuple)) else [value])
    return result
def decorator_identity(decorator: ast.expr, names: Bindings) -> str:
    name = resolve(decorator, names)
    if name in ADAPTABLE_DECORATORS:
        return name
    return ast.dump(decorator)
def skips(owner: Owner, names: Bindings) -> int:
    count = 0
    for item in body(owner):
        if isinstance(item, ast.Raise) and item.exc is not None and resolve(item.exc, names) in SKIP_RAISES:
            count += 1
        if isinstance(item, ast.Call):
            callee = item.func
            if resolve(callee, names) in SKIP_CALLS or isinstance(callee, ast.Attribute) and callee.attr == 'skipTest':
                count += 1
    return count
def describe(identity: Identity) -> str:
    if not identity:
        return 'module'
    return '.'.join(part + ('[' + str(n) + ']' if n > 1 else '') for _, part, n in identity)
class Comparison:
    # Both programs, parsed, with the original tests and their lexical owners.
    def __init__(self, before: str, after: str) -> None:
        self.old = ast.parse(before)
        self.new = ast.parse(after)
        self.old_definitions = definitions(self.old)
        self.new_definitions = definitions(self.new)
        self.tests: dict[Identity, Definition] = {
            identity: node for identity, node in self.old_definitions.items()
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith('test')}
        self.old_bindings = bindings(self.old)
        self.new_bindings = bindings(self.new)
    def owner(self, identity: Identity, new: bool) -> Optional[Owner]:
        if not identity:
            return self.new if new else self.old
        return (self.new_definitions if new else self.old_definitions).get(identity)
    def footprint(self, identity: Identity) -> bool:
        return any(identity[:depth] in self.tests for depth in range(1, len(identity) + 1))
    def owners(self) -> list[Identity]:
        # The module, every original test and every definition enclosing one.
        result: list[Identity] = [()]
        for identity in self.tests:
            for depth in range(1, len(identity) + 1):
                if identity[:depth] not in result:
                    result.append(identity[:depth])
        return result
def original_test_remains(comparison: Comparison, identity: Identity, label: str) -> None:
    if identity in comparison.tests and identity not in comparison.new_definitions:
        raise ValueError('original test function removed: ' + label)
def owner_checks_remain(comparison: Comparison, identity: Identity, label: str) -> None:
    # Preserve the original test footprint, partitioned by lexical owner.
    # Empty ordinary helpers and unrelated definitions are not frozen.
    if not comparison.footprint(identity):
        return
    count = assertions(comparison.old_definitions[identity])
    replacement = comparison.new_definitions.get(identity)
    if count and (replacement is None or assertions(replacement) < count):
        raise ValueError('original assertion/failure checks removed from ' + label)
def failure_collector_unchanged(comparison: Comparison, identity: Identity, label: str) -> None:
    node = comparison.old_definitions[identity]
    if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) or node.name != 'fail':
        return
    replacement = comparison.new_definitions.get(identity)
    if replacement is None or ast.dump(node) != ast.dump(replacement):
        raise ValueError('test failure collector must retain its behavior')
def test_registration_remains(comparison: Comparison) -> None:
    # Retain invocation/registration of original tests, beyond definitions/comments.
    for identity in comparison.tests:
        name = comparison.tests[identity].name
        old_calls = sum(isinstance(n, ast.Name) and n.id == name for n in ast.walk(comparison.old))
        new_calls = sum(isinstance(n, ast.Name) and n.id == name for n in ast.walk(comparison.new))
        if new_calls < old_calls:
            raise ValueError('test invocation/registration removed: ' + name)
def test_decorators_remain(comparison: Comparison) -> None:
    # A decorator on a test, on a class or function enclosing one, or in the
    # module's pytestmark can disable the test while every check survives.
    # Compare what each decorator names, not how it is spelled: an imported
    # alias of unittest.skip is unittest.skip.
    for identity in comparison.owners():
        old, new = comparison.owner(identity, False), comparison.owner(identity, True)
        if old is None or new is None:
            continue
        before = [decorator_identity(decorator, comparison.old_bindings) for decorator in decorators(old)]
        after = decorators(new)
        for position, decorator in enumerate(after):
            if position < len(before) and before[position] == decorator_identity(decorator, comparison.new_bindings):
                continue
            name = resolve(decorator, comparison.new_bindings)
            if name in BYPASS_DECORATORS:
                raise ValueError('test bypass decorator ' + name + ' on ' + describe(identity))
            raise ValueError('original test decorators must remain on ' + describe(identity))
        if len(after) < len(before):
            raise ValueError('original test decorators must remain on ' + describe(identity))
def skips_not_added(comparison: Comparison) -> None:
    # A skip raised or requested inside any body, including a fixture such as
    # setUp or the module itself, disables tests without touching their checks.
    for identity, owner in [((), comparison.new)] + list(comparison.new_definitions.items()):
        original = comparison.owner(identity, False)
        before = 0 if original is None else skips(original, comparison.old_bindings)
        if skips(owner, comparison.new_bindings) > before:
            raise ValueError('test skip added to ' + describe(identity))
# The preservation policy, applied in this order. Definition constraints run
# for every original definition in source order; module constraints follow.
# A new finding adds a named row and a pinned test, never a branch above.
DEFINITION_CONSTRAINTS: tuple[tuple[str, DefinitionConstraint], ...] = (
    ('original-test-remains', original_test_remains),
    ('owner-checks-remain', owner_checks_remain),
    ('failure-collector-unchanged', failure_collector_unchanged),
)
MODULE_CONSTRAINTS: tuple[tuple[str, ModuleConstraint], ...] = (
    ('test-registration-remains', test_registration_remains),
    ('test-decorators-remain', test_decorators_remain),
    ('skips-not-added', skips_not_added),
)
def check(before: str, after: str) -> None:
    comparison = Comparison(before, after)
    for identity in comparison.old_definitions:
        label = describe(identity)
        for _, constraint in DEFINITION_CONSTRAINTS:
            constraint(comparison, identity, label)
    for _, constraint in MODULE_CONSTRAINTS:
        constraint(comparison)
# Request reading and validation run only as a program; importing the module
# for tests defines the helpers without touching stdin.
def main() -> None:
    request = json.load(sys.stdin)
    check(request['before'], request['after'])
if __name__ == '__main__':
    main()
