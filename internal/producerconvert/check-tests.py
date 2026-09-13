# Parse only. Never import or execute proposed test code.
import ast
import json
import sys
def functions(tree: ast.AST):
    result: "dict[tuple[tuple[str, str, int], ...], ast.FunctionDef | ast.AsyncFunctionDef]" = {}
    def visit(node: ast.AST, owner: tuple[tuple[str, str, int], ...], occurrences: dict[tuple[str, str], int]) -> None:
        if isinstance(node, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            component = (type(node).__name__, node.name)
            occurrence = occurrences.get(component, 0) + 1
            occurrences[component] = occurrence
            owner = owner + ((component[0], component[1], occurrence),)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                result[owner] = node
            occurrences = {}
        # Statements such as if/try do not introduce a lexical owner.
        for child in ast.iter_child_nodes(node):
            visit(child, owner, occurrences)
    visit(tree, (), {})
    return result
def assertions(node: ast.AST) -> int:
    count = 0
    for item in ast.walk(node):
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
    old_functions, new_functions = functions(old), functions(new)
    for identity, function in old_functions.items():
        name = function.name
        label = '.'.join(part + ('[' + str(n) + ']' if n > 1 else '')
                         for _, part, n in identity)
        if name.startswith('test'):
            if identity not in new_functions:
                raise ValueError('original test function removed: ' + label)
            if assertions(new_functions[identity]) < assertions(function):
                raise ValueError('original assertion/failure checks removed from ' + label)
        if name == 'fail':
            if identity not in new_functions or ast.dump(function) != ast.dump(new_functions[identity]):
                raise ValueError('test failure collector must retain its behavior')
    # Retain invocation/registration of original tests, beyond definitions/comments.
    for function in old_functions.values():
        name = function.name
        if not name.startswith('test'):
            continue
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
