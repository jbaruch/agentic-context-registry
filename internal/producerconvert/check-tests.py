# Parse only. Never import or execute proposed test code.
import ast
import json
import sys
def functions(tree: ast.AST):
    return {node.name: node for node in ast.walk(tree)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
def assertions(node: ast.AST) -> int:
    count = 0
    for item in ast.walk(node):
        if isinstance(item, ast.Assert):
            count += 1
        if isinstance(item, ast.Call):
            callee = item.func
            if (isinstance(callee, ast.Name) and callee.id == 'fail') or (
                isinstance(callee, ast.Attribute) and callee.attr.startswith('assert')):
                count += 1
    return count
def check(before: str, after: str) -> None:
    old = ast.parse(before)
    new = ast.parse(after)
    old_functions, new_functions = functions(old), functions(new)
    for name, function in old_functions.items():
        if name.startswith('test_'):
            if name not in new_functions:
                raise ValueError('original test function removed: ' + name)
            if assertions(new_functions[name]) < assertions(function):
                raise ValueError('original assertion/failure checks removed from ' + name)
        if name == 'fail':
            if name not in new_functions or ast.dump(function) != ast.dump(new_functions[name]):
                raise ValueError('test failure collector must retain its behavior')
    # Retain invocation/registration of original tests, beyond definitions/comments.
    for name in old_functions:
        if not name.startswith('test_'):
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
