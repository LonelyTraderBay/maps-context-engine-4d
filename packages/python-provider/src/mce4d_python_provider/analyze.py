"""Bounded AST-only symbol, scope, import, and uncertainty discovery."""

from __future__ import annotations

import ast
import builtins
import hashlib
import io
import sys
import tokenize
from dataclasses import dataclass, field
from typing import Any

from .descriptor import language_provider_descriptor
from .profile import METADATA_NAMES, validate_provider_request

MAX_AST_NODES = 200_000
MAX_SYMBOLS = 200_000
MAX_RELATIONS = 400_000
MAX_DIAGNOSTICS = 100_000
BUILTINS = frozenset(dir(builtins)) | frozenset({"__name__", "__file__", "__package__", "__builtins__"})
STD_LIB = getattr(sys, "stdlib_module_names", frozenset())


class ProviderAnalysisError(ValueError):
    """A bounded analysis failure; target code and import hooks are never run."""


@dataclass
class Scope:
    kind: str
    name: str
    symbol_id: str
    parent: "Scope | None"
    bindings: dict[str, str] = field(default_factory=dict)
    global_names: set[str] = field(default_factory=set)
    nonlocal_names: set[str] = field(default_factory=set)
    class_cell_symbol_id: str | None = None


class SourceOffsets:
    def __init__(self, source_bytes: bytes) -> None:
        self.source_bytes = source_bytes
        self.starts = [3 if source_bytes.startswith(b"\xef\xbb\xbf") else 0]
        offset = self.starts[0]
        while offset < len(source_bytes):
            if source_bytes[offset] == 0x0D:
                offset += 2 if offset + 1 < len(source_bytes) and source_bytes[offset + 1] == 0x0A else 1
                self.starts.append(offset)
            elif source_bytes[offset] == 0x0A:
                offset += 1
                self.starts.append(offset)
            else:
                offset += 1

    def range(self, node: ast.AST | None) -> dict[str, int]:
        if node is None:
            return {"start_byte": 0, "end_byte": 0}
        start_line = max(1, int(getattr(node, "lineno", 1)))
        end_line = max(start_line, int(getattr(node, "end_lineno", start_line)))
        start_col = max(0, int(getattr(node, "col_offset", 0)))
        end_col = max(start_col if end_line == start_line else 0, int(getattr(node, "end_col_offset", start_col)))
        start = self._offset(start_line, start_col)
        end = self._offset(end_line, end_col)
        return {"start_byte": start, "end_byte": max(start, end)}

    def _offset(self, line: int, utf8_column: int) -> int:
        index = min(line - 1, len(self.starts) - 1)
        return min(len(self.source_bytes), self.starts[index] + utf8_column)


def _identity(prefix: str, *parts: object) -> str:
    payload = "\0".join(str(part) for part in parts).encode("utf-8")
    return prefix + ":" + hashlib.sha256(payload).hexdigest()


def _utf8_key(value: str) -> bytes:
    return value.encode("utf-8")


def _python_module_name(path: str, roots: list[str]) -> str | None:
    for root in sorted(roots, key=lambda item: (-len(item.encode("utf-8")), _utf8_key(item))):
        prefix = root + "/"
        if not path.startswith(prefix):
            continue
        relative = path[len(prefix):]
        if relative.endswith(".pyi"):
            relative = relative[:-4]
        elif relative.endswith(".py"):
            relative = relative[:-3]
        else:
            continue
        if relative == "__init__":
            return ""
        if relative.endswith("/__init__"):
            relative = relative[:-len("/__init__")]
        parts = relative.split("/")
        if not parts or any(not part.isidentifier() for part in parts):
            return None
        return ".".join(parts)
    return None


def _static_import_name(path: str, roots: list[str], module: str | None, level: int) -> str | None:
    if level == 0:
        return module
    current = _python_module_name(path, roots)
    if current is None:
        return None
    current_parts = current.split(".") if current else []
    package_parts = current_parts if path.endswith(("/__init__.py", "/__init__.pyi")) else current_parts[:-1]
    remove = level - 1
    if remove > len(package_parts):
        return None
    base = package_parts[:len(package_parts) - remove]
    if module:
        base.extend(module.split("."))
    return ".".join(base)


class FileAnalyzer(ast.NodeVisitor):
    def __init__(
        self,
        path: str,
        source: str,
        source_bytes: bytes,
        root_symbol_id: str,
        module_index: dict[str, list[str]],
        roots: list[str],
    ) -> None:
        self.path = path
        self.offsets = SourceOffsets(source_bytes)
        self.module_index = module_index
        self.roots = roots
        self.symbols: list[dict[str, Any]] = []
        self.relations: list[dict[str, Any]] = []
        self.diagnostics: list[dict[str, Any]] = []
        self._symbol_ids: set[str] = set()
        self._relation_ids: set[str] = set()
        self._diagnostic_keys: set[tuple[str, int, int, str]] = set()
        self.root_scope = Scope("module", path, root_symbol_id, None)
        self.scope = self.root_scope
        self.scope_stack = [self.root_scope]
        self.dynamic_import_aliases: set[str] = set()
        self.pending_nonlocal_bindings: list[tuple[Scope, str, ast.Name]] = []
        self.pending_references: list[tuple[Scope, str, ast.Name]] = []

    def add_symbol(self, kind: str, name: str, node: ast.AST | None, stable_parts: tuple[object, ...] = ()) -> str:
        source_range = self.offsets.range(node)
        symbol_id = _identity("sym", self.path, kind, *stable_parts, source_range["start_byte"], source_range["end_byte"])
        if symbol_id not in self._symbol_ids:
            if len(self.symbols) >= MAX_SYMBOLS:
                raise ProviderAnalysisError("Python provider symbol budget exceeded")
            self._symbol_ids.add(symbol_id)
            self.symbols.append({
                "id": symbol_id,
                "kind": kind,
                "name": name,
                "path": self.path,
                "range": source_range,
                "analysis_status": "syntax",
            })
        return symbol_id

    def add_relation(
        self,
        kind: str,
        source_symbol_id: str,
        target_symbol_id: str | None,
        node: ast.AST | None,
        evidence: str = "declared",
        status: str = "syntax",
        identity: str = "",
    ) -> None:
        source_range = self.offsets.range(node)
        relation_id = _identity(
            "rel", self.path, kind, source_symbol_id, target_symbol_id or "", identity,
            source_range["start_byte"], source_range["end_byte"],
        )
        if relation_id in self._relation_ids:
            return
        if len(self.relations) >= MAX_RELATIONS:
            raise ProviderAnalysisError("Python provider relation budget exceeded")
        self._relation_ids.add(relation_id)
        self.relations.append({
            "id": relation_id,
            "source_symbol_id": source_symbol_id,
            "target_symbol_id": target_symbol_id,
            "kind": kind,
            "evidence_kind": evidence,
            "path": self.path,
            "range": source_range,
            "analysis_status": status,
        })

    def add_diagnostic(self, code: str, message: str, node: ast.AST | None, severity: str = "warning") -> None:
        source_range = self.offsets.range(node)
        key = (code, source_range["start_byte"], source_range["end_byte"], message)
        if key in self._diagnostic_keys:
            return
        if len(self.diagnostics) >= MAX_DIAGNOSTICS:
            raise ProviderAnalysisError("Python provider diagnostic budget exceeded")
        self._diagnostic_keys.add(key)
        self.diagnostics.append({
            "code": code,
            "severity": severity,
            "message": message,
            "path": self.path,
            "range": source_range,
        })

    def _declare_named_scope(self, kind: str, name: str, node: ast.AST) -> tuple[str, Scope]:
        parent_scope = self.scope
        binding_scope = self._binding_scope(name)
        declaration_id = self.add_symbol(kind, name, node)
        binding_scope.bindings[name] = declaration_id
        self.add_relation("declares", binding_scope.symbol_id, declaration_id, node)
        scope_name = f"{kind} {name}"
        scope_id = self.add_symbol("scope", scope_name, node, (kind, name))
        self.add_relation("owns_scope", declaration_id, scope_id, node)
        return declaration_id, Scope(kind, name, scope_id, parent_scope)

    def _anonymous_scope(self, kind: str, name: str, node: ast.AST) -> Scope:
        parent_scope = self.scope
        scope_id = self.add_symbol("scope", name, node, (kind, name))
        self.add_relation("contains_scope", parent_scope.symbol_id, scope_id, node, identity=kind)
        return Scope(kind, name, scope_id, parent_scope)

    def _push(self, scope: Scope) -> None:
        self.scope_stack.append(scope)
        self.scope = scope

    def _pop(self) -> None:
        self.scope_stack.pop()
        self.scope = self.scope_stack[-1]

    def _binding_scope(self, name: str) -> Scope:
        if name in self.scope.global_names:
            return self.root_scope
        if name in self.scope.nonlocal_names:
            current = self.scope.parent
            while current is not None:
                if current.kind == "function":
                    return current
                current = current.parent
        return self.scope

    def _bind_variable(self, name: str, node: ast.AST, kind: str = "variable") -> str:
        target_scope = self._binding_scope(name)
        existing = target_scope.bindings.get(name)
        if existing and kind == "variable":
            symbol_id = existing
        else:
            symbol_id = self.add_symbol(kind, name, node, (target_scope.symbol_id, name))
            target_scope.bindings[name] = symbol_id
        self.add_relation("declares", target_scope.symbol_id, symbol_id, node, identity=name)
        return symbol_id

    def _resolve_name(self, name: str) -> str | None:
        if name in self.scope.global_names:
            return self.root_scope.bindings.get(name) or ("builtin" if name in BUILTINS else None)
        if name in self.scope.nonlocal_names:
            current = self.scope.parent
            while current is not None:
                if current.kind == "function" and name in current.bindings:
                    return current.bindings[name]
                current = current.parent
            return None
        if name == "__class__":
            current = self.scope
            in_function_scope = False
            while current is not None:
                if current.kind in {"function", "listcomp", "setcomp", "dictcomp", "genexpr"}:
                    in_function_scope = True
                if current.kind == "class":
                    if in_function_scope:
                        return current.class_cell_symbol_id
                    break
                current = current.parent
        if name in self.scope.bindings:
            return self.scope.bindings[name]
        current = self.scope.parent
        while current is not None:
            if current.kind == "module":
                return current.bindings.get(name) or ("builtin" if name in BUILTINS else None)
            if current.kind != "class" and name in current.bindings:
                return current.bindings[name]
            current = current.parent
        return "builtin" if name in BUILTINS else None

    def _import_target(self, imported: str | None) -> tuple[str | None, str]:
        if not imported:
            return None, "unknown"
        matches = self.module_index.get(imported, [])
        if len(matches) == 1:
            return matches[0], "syntax"
        if len(matches) > 1:
            return None, "unknown"
        top_level = imported.split(".", 1)[0]
        if top_level in STD_LIB:
            return None, "syntax"
        return None, "unknown"

    def _record_import(self, module: str | None, level: int, node: ast.AST, alias: str) -> None:
        imported = _static_import_name(self.path, self.roots, module, level)
        target, status = self._import_target(imported)
        self.add_relation("imports", self.scope.symbol_id, target, node, status=status, identity=alias or imported or "relative")
        if status == "unknown":
            code = "IMPORT_TARGET_AMBIGUOUS" if imported and len(self.module_index.get(imported, [])) > 1 else "IMPORT_TARGET_NOT_CAPTURED"
            self.add_diagnostic(code, "Static import target is not resolved in the captured source set.", node)

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        for decorator in node.decorator_list:
            self.visit(decorator)
        for default in [*node.args.defaults, *(item for item in node.args.kw_defaults if item is not None)]:
            self.visit(default)
        for argument in [*node.args.posonlyargs, *node.args.args, *node.args.kwonlyargs]:
            if argument.annotation is not None:
                self.visit(argument.annotation)
        if node.args.vararg and node.args.vararg.annotation:
            self.visit(node.args.vararg.annotation)
        if node.args.kwarg and node.args.kwarg.annotation:
            self.visit(node.args.kwarg.annotation)
        if node.returns is not None:
            self.visit(node.returns)
        declaration_id, child_scope = self._declare_named_scope("function", node.name, node)
        for decorator in node.decorator_list:
            self.add_relation("decorator", declaration_id, None, decorator, "unknown", "unknown", node.name)
            self.add_diagnostic("DECORATOR_SEMANTICS_UNKNOWN", "Decorator behavior is not evaluated by the syntax provider.", decorator)
        if getattr(node, "type_params", ()):
            self.add_diagnostic("TYPE_PARAMETER_SCOPE_UNKNOWN", "Generic type parameter semantics are not resolved by this provider.", node)
        self._push(child_scope)
        for argument in [*node.args.posonlyargs, *node.args.args, *node.args.kwonlyargs]:
            self._bind_variable(argument.arg, argument, "parameter")
        if node.args.vararg:
            self._bind_variable(node.args.vararg.arg, node.args.vararg, "parameter")
        if node.args.kwarg:
            self._bind_variable(node.args.kwarg.arg, node.args.kwarg, "parameter")
        for statement in node.body:
            self.visit(statement)
        self._pop()

    visit_AsyncFunctionDef = visit_FunctionDef

    def visit_ClassDef(self, node: ast.ClassDef) -> None:
        for decorator in node.decorator_list:
            self.visit(decorator)
        for base in node.bases:
            self.visit(base)
        for keyword in node.keywords:
            self.visit(keyword.value)
        declaration_id, child_scope = self._declare_named_scope("class", node.name, node)
        for decorator in node.decorator_list:
            self.add_relation("decorator", declaration_id, None, decorator, "unknown", "unknown", node.name)
            self.add_diagnostic("DECORATOR_SEMANTICS_UNKNOWN", "Decorator behavior is not evaluated by the syntax provider.", decorator)
        if getattr(node, "type_params", ()):
            self.add_diagnostic("TYPE_PARAMETER_SCOPE_UNKNOWN", "Generic type parameter semantics are not resolved by this provider.", node)
        self._push(child_scope)
        child_scope.class_cell_symbol_id = declaration_id
        for statement in node.body:
            self.visit(statement)
        self._pop()

    def visit_Lambda(self, node: ast.Lambda) -> None:
        for default in [*node.args.defaults, *(item for item in node.args.kw_defaults if item is not None)]:
            self.visit(default)
        scope_name = f"lambda@{self.offsets.range(node)['start_byte']}"
        function_id = self.add_symbol("function", "<lambda>", node, ("lambda",))
        self.add_relation("contains", self.scope.symbol_id, function_id, node)
        child_scope = self._anonymous_scope("function", scope_name, node)
        self.add_relation("owns_scope", function_id, child_scope.symbol_id, node)
        self._push(child_scope)
        for argument in [*node.args.posonlyargs, *node.args.args, *node.args.kwonlyargs]:
            self._bind_variable(argument.arg, argument, "parameter")
        if node.args.vararg:
            self._bind_variable(node.args.vararg.arg, node.args.vararg, "parameter")
        if node.args.kwarg:
            self._bind_variable(node.args.kwarg.arg, node.args.kwarg, "parameter")
        self.visit(node.body)
        self._pop()

    def _visit_comprehension(self, node: ast.AST, kind: str, values: list[ast.AST]) -> None:
        generators = list(getattr(node, "generators", ()))
        if generators:
            self.visit(generators[0].iter)
        start = self.offsets.range(node)["start_byte"]
        child_scope = self._anonymous_scope(kind, f"{kind}@{start}", node)
        self._push(child_scope)
        for index, generator in enumerate(generators):
            if index > 0:
                self.visit(generator.iter)
            self.visit(generator.target)
            for condition in generator.ifs:
                self.visit(condition)
            if generator.is_async:
                self.add_diagnostic("ASYNC_COMPREHENSION_UNKNOWN", "Async comprehension execution semantics are not resolved.", node)
        for value in values:
            self.visit(value)
        self._pop()

    def visit_ListComp(self, node: ast.ListComp) -> None:
        self._visit_comprehension(node, "listcomp", [node.elt])

    def visit_SetComp(self, node: ast.SetComp) -> None:
        self._visit_comprehension(node, "setcomp", [node.elt])

    def visit_GeneratorExp(self, node: ast.GeneratorExp) -> None:
        self._visit_comprehension(node, "genexpr", [node.elt])

    def visit_DictComp(self, node: ast.DictComp) -> None:
        self._visit_comprehension(node, "dictcomp", [node.key, node.value])

    def visit_Import(self, node: ast.Import) -> None:
        for alias in node.names:
            local_name = alias.asname or alias.name.split(".", 1)[0]
            self._bind_variable(local_name, node, "import")
            self._record_import(alias.name, 0, node, alias.name)
            if alias.name == "importlib":
                self.dynamic_import_aliases.add(alias.asname or "importlib")

    def visit_ImportFrom(self, node: ast.ImportFrom) -> None:
        imported = _static_import_name(self.path, self.roots, node.module, node.level)
        target, status = self._import_target(imported)
        self.add_relation("imports", self.scope.symbol_id, target, node, status=status, identity=imported or "relative")
        if status == "unknown":
            code = "IMPORT_TARGET_AMBIGUOUS" if imported and len(self.module_index.get(imported, [])) > 1 else "IMPORT_TARGET_NOT_CAPTURED"
            self.add_diagnostic(code, "Static import target is not resolved in the captured source set.", node)
        for alias in node.names:
            if alias.name == "*":
                self.add_relation("star_import", self.scope.symbol_id, target, node, "unknown", "unknown", imported or "relative")
                self.add_diagnostic("STAR_IMPORT_SCOPE_UNKNOWN", "Star import names are not expanded by the syntax provider.", node)
                continue
            local_name = alias.asname or alias.name
            self._bind_variable(local_name, node, "import")

    def visit_Global(self, node: ast.Global) -> None:
        self.scope.global_names.update(node.names)

    def visit_Nonlocal(self, node: ast.Nonlocal) -> None:
        self.scope.nonlocal_names.update(node.names)

    def visit_Name(self, node: ast.Name) -> None:
        if isinstance(node.ctx, (ast.Store, ast.Del)):
            if node.id in self.scope.nonlocal_names:
                self.pending_nonlocal_bindings.append((self.scope, node.id, node))
            else:
                self._bind_variable(node.id, node)
            return
        self.pending_references.append((self.scope, node.id, node))

    @staticmethod
    def _enclosing_function_binding(scope: Scope, name: str) -> Scope | None:
        current = scope.parent
        while current is not None:
            if current.kind == "function" and name in current.bindings:
                return current
            current = current.parent
        return None

    def resolve_pending_nonlocal_bindings(self) -> None:
        for scope, name, node in self.pending_nonlocal_bindings:
            target_scope = self._enclosing_function_binding(scope, name)
            if target_scope is None:
                self.add_relation("reference", scope.symbol_id, None, node, "unknown", "unknown", name)
                self.add_diagnostic(
                    "UNRESOLVED_BINDING",
                    "Name binding is not proven by captured declarations or builtins.",
                    node,
                )
                continue
            symbol_id = target_scope.bindings.get(name)
            if symbol_id is None:
                symbol_id = self.add_symbol("variable", name, node, (target_scope.symbol_id, name))
                target_scope.bindings[name] = symbol_id
            self.add_relation("declares", target_scope.symbol_id, symbol_id, node, identity=name)

    def resolve_pending_references(self) -> None:
        for scope, name, node in self.pending_references:
            self.scope = scope
            target = self._resolve_name(name)
            if target is None:
                self.add_relation("reference", scope.symbol_id, None, node, "unknown", "unknown", name)
                self.add_diagnostic("UNRESOLVED_BINDING", "Name binding is not proven by captured declarations or builtins.", node)
            elif target != "builtin":
                self.add_relation("reference", scope.symbol_id, target, node, "declared", "syntax", name)
        self.scope = self.scope_stack[0]

    def visit_Call(self, node: ast.Call) -> None:
        function = node.func
        dynamic = isinstance(function, ast.Name) and (
            function.id == "__import__" or function.id in self.dynamic_import_aliases
        )
        dynamic = dynamic or (
            isinstance(function, ast.Attribute)
            and function.attr == "import_module"
            and isinstance(function.value, ast.Name)
            and function.value.id in self.dynamic_import_aliases | {"importlib"}
        )
        if dynamic:
            self.add_relation("dynamic_import", self.scope.symbol_id, None, node, "unknown", "unknown")
            self.add_diagnostic("DYNAMIC_IMPORT_UNKNOWN", "Dynamic import target is not resolved or executed.", node)
        if isinstance(function, ast.Name) and function.id in {"exec", "eval"}:
            self.add_relation("dynamic_code", self.scope.symbol_id, None, node, "unknown", "unknown", function.id)
            self.add_diagnostic("DYNAMIC_CODE_UNKNOWN", "Dynamic code behavior is not evaluated by the syntax provider.", node)
        if isinstance(function, ast.Name) and function.id == "setattr":
            self.add_relation("dynamic_mutation", self.scope.symbol_id, None, node, "unknown", "unknown")
            self.add_diagnostic("MONKEY_PATCH_UNKNOWN", "Dynamic attribute mutation is not resolved by the syntax provider.", node)
        if isinstance(function, ast.Attribute) and function.attr in {"patch", "setattr"}:
            self.add_relation("dynamic_mutation", self.scope.symbol_id, None, node, "unknown", "unknown", function.attr)
            self.add_diagnostic("MONKEY_PATCH_UNKNOWN", "Potential monkey patch behavior is not resolved by the syntax provider.", node)
        if (
            isinstance(function, ast.Attribute)
            and function.attr == "object"
            and isinstance(function.value, ast.Attribute)
            and function.value.attr == "patch"
        ):
            self.add_relation("dynamic_mutation", self.scope.symbol_id, None, node, "unknown", "unknown", "patch.object")
            self.add_diagnostic("MONKEY_PATCH_UNKNOWN", "Potential monkey patch behavior is not resolved by the syntax provider.", node)
        self.generic_visit(node)

    @staticmethod
    def _has_attribute_target(node: ast.AST) -> bool:
        pending = [node]
        while pending:
            target = pending.pop()
            if isinstance(target, ast.Attribute):
                return True
            if isinstance(target, (ast.List, ast.Tuple)):
                pending.extend(target.elts)
            elif isinstance(target, ast.Starred):
                pending.append(target.value)
        return False

    def _record_attribute_mutation(self, node: ast.AST) -> None:
        self.add_relation("dynamic_mutation", self.scope.symbol_id, None, node, "unknown", "unknown")
        self.add_diagnostic("MONKEY_PATCH_UNKNOWN", "Attribute mutation may change runtime behavior and is not resolved.", node)

    def visit_Assign(self, node: ast.Assign) -> None:
        if any(self._has_attribute_target(target) for target in node.targets):
            self._record_attribute_mutation(node)
        self.generic_visit(node)

    def visit_AugAssign(self, node: ast.AugAssign) -> None:
        if self._has_attribute_target(node.target):
            self._record_attribute_mutation(node)
        self.generic_visit(node)

    def visit_AnnAssign(self, node: ast.AnnAssign) -> None:
        if self._has_attribute_target(node.target):
            self._record_attribute_mutation(node)
        self.generic_visit(node)

    def visit_Delete(self, node: ast.Delete) -> None:
        if any(self._has_attribute_target(target) for target in node.targets):
            self._record_attribute_mutation(node)
        self.generic_visit(node)


def _count_ast_nodes(tree: ast.AST) -> int:
    count = 0
    stack = [tree]
    while stack:
        node = stack.pop()
        count += 1
        if count > MAX_AST_NODES:
            raise ProviderAnalysisError("Python AST node budget exceeded")
        stack.extend(ast.iter_child_nodes(node))
    return count


def _module_path_index(paths: list[str], roots: list[str]) -> dict[str, list[str]]:
    result: dict[str, list[str]] = {}
    for path in paths:
        module_name = _python_module_name(path, roots)
        if module_name:
            result.setdefault(module_name, []).append(path)
    for matches in result.values():
        matches.sort(key=_utf8_key)
    return result


def _validate_source_text(source_bytes: bytes) -> tuple[str | None, str | None]:
    try:
        source = source_bytes.decode("utf-8", errors="strict")
        encoding, _ = tokenize.detect_encoding(io.BytesIO(source_bytes).readline)
        if encoding.replace("_", "-").lower() not in {"utf-8", "utf-8-sig"}:
            return None, "SOURCE_ENCODING_UNSUPPORTED"
        if source.startswith("\ufeff"):
            source = source[1:]
            # A UTF-8 BOM occupies three bytes, so parsing offsets must retain it.
            return source, None
        return source, None
    except (UnicodeDecodeError, SyntaxError):
        return None, "SOURCE_ENCODING_INVALID"


def analyze_provider_request(request: Any) -> dict[str, Any]:
    config, source_by_path = validate_provider_request(request)
    roots = config["module_roots"]
    python_paths = sorted(
        (path for path in source_by_path if path.endswith((".py", ".pyi"))),
        key=_utf8_key,
    )
    module_index = _module_path_index(python_paths, roots)
    path_to_module: dict[str, str] = {}
    for module_name, paths in module_index.items():
        for path in paths:
            path_to_module[path] = module_name

    modules: dict[str, str] = {}
    results_by_path: dict[str, dict[str, Any]] = {}
    aggregate_nodes = 0
    for path in sorted(source_by_path, key=_utf8_key):
        source_bytes = source_by_path[path]
        if path.endswith((".py", ".pyi")):
            module_name = path_to_module.get(path, path)
            module_id = _identity("sym", path, "module", module_name, 0, len(source_bytes))
            modules[path] = module_id
            source, encoding_error = _validate_source_text(source_bytes)
            if encoding_error:
                results_by_path[path] = {
                    "symbols": [{"id": module_id, "kind": "module", "name": module_name, "path": path,
                                 "range": {"start_byte": 0, "end_byte": len(source_bytes)}, "analysis_status": "unknown"}],
                    "relations": [],
                    "diagnostics": [{"code": encoding_error, "severity": "error", "message": "Python source encoding is unsupported or invalid.",
                                     "path": path, "range": {"start_byte": 0, "end_byte": 0}}],
                }
                continue
            assert source is not None
            try:
                tree = ast.parse(source, filename=path, mode="exec", type_comments=True)
                aggregate_nodes += _count_ast_nodes(tree)
                if aggregate_nodes > MAX_AST_NODES:
                    raise ProviderAnalysisError("Python request AST node budget exceeded")
            except (SyntaxError, ValueError):
                results_by_path[path] = {
                    "symbols": [{"id": module_id, "kind": "module", "name": module_name, "path": path,
                                 "range": {"start_byte": 0, "end_byte": len(source_bytes)}, "analysis_status": "unknown"}],
                    "relations": [],
                    "diagnostics": [{"code": "PYTHON_SYNTAX_INVALID", "severity": "error",
                                     "message": "Python source has invalid syntax.", "path": path,
                                     "range": {"start_byte": 0, "end_byte": 0}}],
                }
                continue
            except (RecursionError, MemoryError) as exc:
                raise ProviderAnalysisError("Python AST parser exceeded its bounded resources") from exc
            analyzer = FileAnalyzer(path, source, source_bytes, module_id, {
                name: [modules.get(candidate, _identity("sym", candidate, "module", name, 0, len(source_by_path[candidate])))
                       for candidate in module_paths]
                for name, module_paths in module_index.items()
            }, roots)
            analyzer.symbols.append({"id": module_id, "kind": "module", "name": module_name, "path": path,
                                     "range": {"start_byte": 0, "end_byte": len(source_bytes)}, "analysis_status": "syntax"})
            analyzer._symbol_ids.add(module_id)
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    for alias in node.names:
                        if alias.name == "importlib":
                            analyzer.dynamic_import_aliases.add(alias.asname or "importlib")
                elif isinstance(node, ast.ImportFrom) and node.module == "importlib":
                    for alias in node.names:
                        if alias.name == "import_module":
                            analyzer.dynamic_import_aliases.add(alias.asname or alias.name)
            try:
                analyzer.visit(tree)
                analyzer.resolve_pending_nonlocal_bindings()
                analyzer.resolve_pending_references()
            except RecursionError as exc:
                raise ProviderAnalysisError("Python AST traversal exceeded its bounded resources") from exc
            results_by_path[path] = {
                "symbols": analyzer.symbols,
                "relations": analyzer.relations,
                "diagnostics": analyzer.diagnostics,
            }
        elif path.endswith(".ipynb"):
            results_by_path[path] = {
                "symbols": [], "relations": [],
                "diagnostics": [{"code": "NOTEBOOK_METADATA_UNSUPPORTED", "severity": "warning",
                                 "message": "Notebook cells and metadata are not inspected or executed by this provider version.",
                                 "path": path, "range": {"start_byte": 0, "end_byte": 0}}],
            }
        elif path.rsplit("/", 1)[-1].lower() in METADATA_NAMES:
            results_by_path[path] = {
                "symbols": [], "relations": [],
                "diagnostics": [{"code": "PYTHON_ENV_METADATA_UNSUPPORTED", "severity": "warning",
                                 "message": "Environment metadata is recognized but not interpreted by this syntax provider.",
                                 "path": path, "range": {"start_byte": 0, "end_byte": 0}}],
            }
        else:
            results_by_path[path] = {
                "symbols": [], "relations": [],
                "diagnostics": [{"code": "UNSUPPORTED_PYTHON_INPUT", "severity": "warning",
                                 "message": "Input kind is outside the Python AST provider boundary.",
                                 "path": path, "range": {"start_byte": 0, "end_byte": 0}}],
            }

    symbols: list[dict[str, Any]] = []
    relations: list[dict[str, Any]] = []
    diagnostics: list[dict[str, Any]] = []
    if config["analysis_mode"] == "typed_when_available":
        path = python_paths[0]
        diagnostics.append({"code": "TYPED_ANALYSIS_UNAVAILABLE", "severity": "info",
                            "message": "Typed enrichment is not enabled for this Python provider version.",
                            "path": path, "range": {"start_byte": 0, "end_byte": 0}})
    metadata_present = any(path.rsplit("/", 1)[-1].lower() in METADATA_NAMES for path in source_by_path)
    if not metadata_present:
        path = python_paths[0]
        diagnostics.append({"code": "PYTHON_ENV_METADATA_ABSENT", "severity": "info",
                            "message": "No supported Python environment metadata was captured.",
                            "path": path, "range": {"start_byte": 0, "end_byte": 0}})
    for path in sorted(results_by_path, key=_utf8_key):
        item = results_by_path[path]
        if len(symbols) + len(item["symbols"]) > MAX_SYMBOLS:
            raise ProviderAnalysisError("Python provider symbol budget exceeded")
        if len(relations) + len(item["relations"]) > MAX_RELATIONS:
            raise ProviderAnalysisError("Python provider relation budget exceeded")
        if len(diagnostics) + len(item["diagnostics"]) > MAX_DIAGNOSTICS:
            raise ProviderAnalysisError("Python provider diagnostic budget exceeded")
        symbols.extend(item["symbols"])
        relations.extend(item["relations"])
        diagnostics.extend(item["diagnostics"])
    symbols.sort(key=lambda item: (_utf8_key(item["path"]), item["range"]["start_byte"], item["range"]["end_byte"], item["id"]))
    relations.sort(key=lambda item: (_utf8_key(item["path"]), item["range"]["start_byte"], item["range"]["end_byte"], item["id"]))
    diagnostics.sort(key=lambda item: (_utf8_key(item["path"]), item["range"]["start_byte"], item["code"], item["message"]))
    return {
        "schema_version": 1,
        "message_type": "analyze_response",
        "request_id": request["request_id"],
        "provider": language_provider_descriptor,
        "symbols": symbols,
        "relations": relations,
        "diagnostics": diagnostics,
    }
