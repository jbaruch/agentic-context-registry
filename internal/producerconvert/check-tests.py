# Parse only. Never import or execute proposed test code.
import ast
import json
import sys
def definitions(tree: ast.AST):
    result: "dict[tuple[tuple[str, str, int], ...], ast.ClassDef | ast.FunctionDef | ast.AsyncFunctionDef]" = {}
    def visit(node: ast.AST, owner: tuple[tuple[str, str, int], ...], occurrences: dict[tuple[str, str], int]) -> None:
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
        if isinstance(item, ast.Assert):
            count += 1
        if isinstance(item, ast.Call):
            callee = item.func
            if (isinstance(callee, ast.Name) and callee.id == 'fail') or (
                isinstance(callee, ast.Attribute) and (callee.attr.startswith('assert') or callee.attr == 'fail')):
                count += 1
    return count
def check(before: str, after: str) -> None:
    old = ast.parse(before)
    new = ast.parse(after)
    old_definitions, new_definitions = definitions(old), definitions(new)
    tests = {identity: node for identity, node in old_definitions.items()
             if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith('test')}
    for identity, node in old_definitions.items():
        label = '.'.join(part + ('[' + str(n) + ']' if n > 1 else '')
                         for _, part, n in identity)
        if identity in tests and identity not in new_definitions:
            raise ValueError('original test function removed: ' + label)
        # Preserve the original test footprint, partitioned by lexical owner.
        # Empty ordinary helpers and unrelated definitions are not frozen.
        if any(identity[:depth] in tests for depth in range(1, len(identity) + 1)):
            count = assertions(node)
            if count and (identity not in new_definitions or assertions(new_definitions[identity]) < count):
                raise ValueError('original assertion/failure checks removed from ' + label)
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == 'fail':
            if identity not in new_definitions or ast.dump(node) != ast.dump(new_definitions[identity]):
                raise ValueError('test failure collector must retain its behavior')
    # Retain invocation/registration of original tests, beyond definitions/comments.
    for identity in tests:
        name = tests[identity].name
        old_calls = sum(isinstance(n, ast.Name) and n.id == name for n in ast.walk(old))
        new_calls = sum(isinstance(n, ast.Name) and n.id == name for n in ast.walk(new))
        if new_calls < old_calls:
            raise ValueError('test invocation/registration removed: ' + name)
# Request reading and validation run only as a program; importing the module
# for tests defines the helpers without touching stdin.
def main() -> None:
    request = json.load(sys.stdin)
    check(request['before'], request['after'])
if __name__ == '__main__':
    main()
