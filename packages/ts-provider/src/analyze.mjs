import { createHash } from "node:crypto";
import path from "node:path";
import * as typescript from "@typescript/typescript6";
import {
  captureFiles,
  createCapturedCompilerHost,
} from "./captured-root.mjs";
import { validateProviderProfile } from "./profile.mjs";
import { languageProviderDescriptor } from "./descriptor.mjs";

const SUPPORTED_EXTENSIONS = new Set([".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"]);
const FRAMEWORK_MODULES = ["react", "vue", "svelte", "@angular/"];
const MAX_CONFIG_GRAPH = 64;
const HASH_SUFFIX_LENGTH = 32;

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function boundedText(value, fallback = "unknown") {
  const text = String(value ?? fallback).replaceAll("\0", "\uFFFD");
  const points = [...text];
  if (points.length <= 4096) return text || fallback;
  return points.slice(0, 4095).join("") + "…";
}

function stableId(prefix, values) {
  const digest = createHash("sha256").update(values.join("\0"), "utf8").digest("hex");
  return prefix + ":" + digest.slice(0, HASH_SUFFIX_LENGTH);
}

function byteOffsets(text) {
  const offsets = new Uint32Array(text.length + 1);
  let byteOffset = 0;
  for (let index = 0; index < text.length;) {
    const codePoint = text.codePointAt(index);
    const codeUnits = codePoint > 0xffff ? 2 : 1;
    offsets[index] = byteOffset;
    if (codeUnits === 2) {
      offsets[index + 1] = byteOffset;
    }
    byteOffset +=
      codePoint <= 0x7f ? 1 :
      codePoint <= 0x7ff ? 2 :
      codePoint <= 0xffff ? 3 : 4;
    index += codeUnits;
    offsets[index] = byteOffset;
  }
  return offsets;
}

function sourceRange(sourceFile, node, byteOffsetByUtf16) {
  const start = typeof node.getStart === "function" ? node.getStart(sourceFile, false) : node.pos;
  const end = typeof node.end === "number" ? node.end : start;
  return {
    start_byte: byteOffsetByUtf16[Math.max(0, Math.min(start, byteOffsetByUtf16.length - 1))],
    end_byte: byteOffsetByUtf16[Math.max(0, Math.min(end, byteOffsetByUtf16.length - 1))],
  };
}

function scriptKind(fileName) {
  const extension = path.posix.extname(fileName).toLowerCase();
  if (extension === ".tsx") return typescript.ScriptKind.TSX;
  if (extension === ".jsx") return typescript.ScriptKind.JSX;
  if (extension === ".js" || extension === ".mjs" || extension === ".cjs") return typescript.ScriptKind.JS;
  return typescript.ScriptKind.TS;
}

function kindForDeclaration(node, ts) {
  if (ts.isImportClause(node) || ts.isImportSpecifier(node) || ts.isNamespaceImport(node) || ts.isImportEqualsDeclaration(node)) return "import";
  if (ts.isExportSpecifier(node)) return "export";
  if (ts.isParameter(node)) return "parameter";
  if (ts.isTypeParameterDeclaration(node)) return "type-parameter";
  if (ts.isFunctionDeclaration(node) || ts.isFunctionExpression(node) || ts.isArrowFunction(node)) return "function";
  if (ts.isMethodDeclaration(node) || ts.isMethodSignature(node) || ts.isGetAccessor(node) || ts.isSetAccessor(node)) return "method";
  if (ts.isPropertyDeclaration(node) || ts.isPropertySignature(node) || ts.isPropertyAssignment(node) || ts.isShorthandPropertyAssignment(node)) return "property";
  if (ts.isVariableDeclaration(node) || ts.isBindingElement(node)) return "variable";
  if (ts.isClassDeclaration(node) || ts.isClassExpression(node)) return "class";
  if (ts.isInterfaceDeclaration(node)) return "interface";
  if (ts.isTypeAliasDeclaration(node)) return "type";
  if (ts.isEnumDeclaration(node)) return "enum";
  if (ts.isEnumMember(node)) return "enum-member";
  if (ts.isModuleDeclaration(node)) return "namespace";
  if (ts.isCatchClause(node)) return "catch";
  return "symbol";
}

function isDeclarationName(node, ts) {
  const parent = node.parent;
  if (!parent) return false;
  if (parent.name === node && ts.isDeclaration(parent)) return true;
  if (
    (ts.isImportSpecifier(parent) || ts.isExportSpecifier(parent)) &&
    (parent.name === node || parent.propertyName === node)
  ) {
    return true;
  }
  if (
    (ts.isImportClause(parent) || ts.isNamespaceImport(parent) || ts.isImportEqualsDeclaration(parent)) &&
    parent.name === node
  ) {
    return true;
  }
  return false;
}

function createDiagnostic(diagnostics, seen, code, severity, message, repoPath, range) {
  const key = [code, repoPath, range.start_byte, range.end_byte].join(":");
  if (seen.has(key)) return;
  seen.add(key);
  diagnostics.push({
    code,
    severity,
    message: boundedText(message),
    path: repoPath,
    range,
  });
}

function configDiagnostic(diagnostics, code, message, repoPath, vfs) {
  const diagnosticPath = vfs.getCapturedFile(repoPath)
    ? repoPath
    : vfs.files.values().next().value.path;
  diagnostics.push({
    code,
    severity: "warning",
    message: boundedText(message),
    path: diagnosticPath,
    range: { start_byte: 0, end_byte: 0 },
  });
}

function resolveCapturedConfigPath(configPath, requestedPath, files) {
  if (
    typeof requestedPath !== "string" ||
    requestedPath.length === 0 ||
    requestedPath.includes("\\") ||
    requestedPath.includes(":") ||
    requestedPath.startsWith("/")
  ) {
    return null;
  }
  const resolved = path.posix.normalize(
    path.posix.resolve("/", path.posix.dirname(configPath), requestedPath).slice(1),
  );
  if (resolved === ".." || resolved.startsWith("../")) return null;

  const candidates = [];
  if (resolved.endsWith(".json")) {
    candidates.push(resolved);
  } else {
    candidates.push(resolved + ".json", path.posix.join(resolved, "tsconfig.json"));
  }
  return candidates.find((candidate) => files.has(candidate)) ?? null;
}

function parseJsonConfig(ts, vfs, configPath) {
  const file = vfs.getCapturedFile(configPath);
  if (!file) return { ok: false, code: "config-not-captured" };
  const parsedText = ts.parseConfigFileTextToJson(vfs.absolutePath(configPath), file.text);
  if (parsedText.error) return { ok: false, code: "config-invalid" };
  if (!parsedText.config || typeof parsedText.config !== "object" || Array.isArray(parsedText.config)) {
    return { ok: false, code: "config-invalid" };
  }
  return { ok: true, config: parsedText.config };
}

function preflightConfigTree(ts, vfs, configPath, visited, configs, allowedPaths) {
  if (!allowedPaths.has(configPath)) {
    return { ok: false, code: "config-reference-not-allowlisted" };
  }
  if (visited.has(configPath)) return { ok: true };
  if (visited.size >= MAX_CONFIG_GRAPH) return { ok: false, code: "config-reference-limit" };
  visited.add(configPath);
  const parsed = parseJsonConfig(ts, vfs, configPath);
  if (!parsed.ok) return parsed;
  configs.set(configPath, parsed.config);

  const compilerOptions = parsed.config.compilerOptions ?? {};
  if (
    !compilerOptions ||
    typeof compilerOptions !== "object" ||
    Array.isArray(compilerOptions) ||
    Object.hasOwn(compilerOptions, "plugins")
  ) {
    return { ok: false, code: "project-plugins-rejected" };
  }

  const extensions = parsed.config.extends === undefined
    ? []
    : Array.isArray(parsed.config.extends)
      ? parsed.config.extends
      : [parsed.config.extends];
  for (const extension of extensions) {
    const resolved = resolveCapturedConfigPath(configPath, extension, vfs.files);
    if (!resolved) return { ok: false, code: "config-reference-not-captured" };
    if (!allowedPaths.has(resolved)) return { ok: false, code: "config-reference-not-allowlisted" };
    const result = preflightConfigTree(ts, vfs, resolved, visited, configs, allowedPaths);
    if (!result.ok) return result;
  }

  const references = parsed.config.references ?? [];
  if (!Array.isArray(references)) return { ok: false, code: "project-reference-invalid" };
  for (const reference of references) {
    const resolved = resolveCapturedConfigPath(configPath, reference?.path, vfs.files);
    if (!resolved) return { ok: false, code: "project-reference-not-captured" };
    if (!allowedPaths.has(resolved)) return { ok: false, code: "config-reference-not-allowlisted" };
    const result = preflightConfigTree(ts, vfs, resolved, visited, configs, allowedPaths);
    if (!result.ok) return result;
  }
  return { ok: true };
}

function parseCapturedConfigs(ts, vfs, configPaths, diagnostics) {
  const rawConfigs = new Map();
  const visited = new Set();
  const allowedPaths = new Set(configPaths);
  for (const configPath of configPaths) {
    const result = preflightConfigTree(ts, vfs, configPath, visited, rawConfigs, allowedPaths);
    if (!result.ok) {
      configDiagnostic(
        diagnostics,
        result.code,
        result.code === "project-plugins-rejected"
          ? "Project compiler plugins are disabled and were rejected before project resolution."
          : "Project configuration is incomplete or outside the captured root; typed analysis is unavailable.",
        configPath,
        vfs,
      );
      return null;
    }
  }

  const parseHost = {
    useCaseSensitiveFileNames: ts.sys.useCaseSensitiveFileNames,
    fileExists: vfs.host.fileExists,
    readFile: vfs.host.readFile,
    readDirectory: vfs.host.readDirectory,
    trace: undefined,
    onUnRecoverableConfigFileDiagnostic: () => {},
  };
  const parsedByPath = new Map();
  const parsedByAbsolutePath = new Map();
  for (const [configPath, rawConfig] of rawConfigs) {
    const absoluteConfigPath = vfs.absolutePath(configPath);
    const configSource = ts.parseJsonText(absoluteConfigPath, JSON.stringify(rawConfig));
    const parsed = ts.parseJsonSourceFileConfigFileContent(
      configSource,
      parseHost,
      path.posix.dirname(absoluteConfigPath),
      {},
      absoluteConfigPath,
    );
    if (parsed.options.plugins !== undefined) {
      configDiagnostic(
        diagnostics,
        "project-plugins-rejected",
        "Project compiler plugins are disabled and were rejected before project resolution.",
        configPath,
        vfs,
      );
      return null;
    }
    for (const error of parsed.errors) {
      configDiagnostic(
        diagnostics,
        "typescript-config-diagnostic",
        ts.flattenDiagnosticMessageText(error.messageText, " "),
        configPath,
        vfs,
      );
    }
    parsedByPath.set(configPath, parsed);
    parsedByAbsolutePath.set(ts.normalizePath(absoluteConfigPath), parsed);
  }

  vfs.host.getParsedCommandLine = (fileName) =>
    parsedByAbsolutePath.get(ts.normalizePath(fileName));
  return parsedByPath;
}

function resolveAlias(checker, symbol, ts) {
  if (!symbol) return null;
  if ((symbol.flags & ts.SymbolFlags.Alias) === 0) return symbol;
  try {
    return checker.getAliasedSymbol(symbol);
  } catch {
    return null;
  }
}

function symbolName(symbol, fallback) {
  try {
    const name = symbol?.getName?.();
    return typeof name === "string" && name.length > 0 ? name : fallback;
  } catch {
    return fallback;
  }
}

function isLibraryDeclaration(declaration, vfs) {
  if (!declaration?.getSourceFile) return false;
  return !vfs.isCapturedPath(declaration.getSourceFile().fileName);
}

function tsDiagnosticPath(diagnostic, vfs) {
  if (!diagnostic.file) return null;
  return vfs.repoPathFor(diagnostic.file.fileName);
}

function createPrograms(ts, vfs, configPaths, diagnostics, diagnosticKeys) {
  const parsedByPath = parseCapturedConfigs(ts, vfs, configPaths, diagnostics);
  if (!parsedByPath) return [];

  const programs = [];
  for (const [configPath, parsed] of parsedByPath) {
    const options = {
      ...parsed.options,
      allowJs: true,
      noEmit: true,
      jsx: parsed.options.jsx ?? ts.JsxEmit.Preserve,
      forceConsistentCasingInFileNames: true,
    };
    const rootNames = parsed.fileNames.filter((fileName) => vfs.isCapturedPath(fileName));
    try {
      const program = ts.createProgram({
        rootNames,
        options,
        host: vfs.host,
        projectReferences: parsed.projectReferences,
      });
      programs.push({ program, checker: program.getTypeChecker(), configPath });
      for (const diagnostic of ts.getPreEmitDiagnostics(program)) {
        const repoPath = tsDiagnosticPath(diagnostic, vfs);
        if (!repoPath || !SUPPORTED_EXTENSIONS.has(path.posix.extname(repoPath).toLowerCase())) {
          continue;
        }
        const sourceFile = diagnostic.file;
        const range = sourceRange(
          sourceFile,
          {
            getStart: () => diagnostic.start ?? 0,
            end: (diagnostic.start ?? 0) + (diagnostic.length ?? 0),
          },
          byteOffsets(sourceFile.text),
        );
        createDiagnostic(
          diagnostics,
          diagnosticKeys,
          "typescript-" + String(diagnostic.code),
          diagnostic.category === ts.DiagnosticCategory.Error ? "error" : "warning",
          ts.flattenDiagnosticMessageText(diagnostic.messageText, " "),
          repoPath,
          range,
        );
      }
    } catch {
      configDiagnostic(
        diagnostics,
        "typescript-program-unavailable",
        "The captured project could not be represented by the pinned TypeScript program.",
        configPath,
        vfs,
      );
    }
  }
  return programs;
}

function addSymbol(symbolsById, symbolIds, tsSymbol, record, bindCompilerSymbol) {
  if (bindCompilerSymbol && tsSymbol && symbolIds.has(tsSymbol)) {
    return symbolIds.get(tsSymbol);
  }
  const id = record.id;
  if (!symbolsById.has(id)) symbolsById.set(id, record);
  if (bindCompilerSymbol && tsSymbol) symbolIds.set(tsSymbol, id);
  return id;
}

export function analyzeProviderRequest(request) {
  const { config, paths: configPaths } = validateProviderProfile(
    request,
    languageProviderDescriptor.digest,
  );
  const files = captureFiles(request.files);
  const diagnostics = [];
  const diagnosticKeys = new Set();
  const vfs = createCapturedCompilerHost(typescript, files);
  const sourceEntries = [...files.values()]
    .filter((file) => SUPPORTED_EXTENSIONS.has(path.posix.extname(file.path).toLowerCase()))
    .sort((left, right) => compareUtf8(left.path, right.path));
  if (sourceEntries.length === 0) {
    throw new Error("provider request contains no supported TypeScript or JavaScript source");
  }

  const programs =
    config.analysis_mode === "typed_when_available"
      ? createPrograms(typescript, vfs, configPaths, diagnostics, diagnosticKeys)
      : (() => {
          const rawConfigs = new Map();
          const visited = new Set();
          const allowedPaths = new Set(configPaths);
          for (const configPath of configPaths) {
            const result = preflightConfigTree(typescript, vfs, configPath, visited, rawConfigs, allowedPaths);
            if (!result.ok) {
              configDiagnostic(
                diagnostics,
                result.code,
                result.code === "project-plugins-rejected"
                  ? "Project compiler plugins are disabled and were rejected before project resolution."
                  : "Project configuration is incomplete or outside the captured root; typed analysis is unavailable.",
                configPath,
                vfs,
              );
              return [];
            }
          }
          return [];
        })();

  const programByFile = new Map();
  const programInfos = programs.map((item) => ({
    ...item,
    symbolIds: new Map(),
    moduleSymbols: new Map(),
  }));
  for (const info of programInfos) {
    for (const sourceFile of info.program.getSourceFiles()) {
      const repoPath = vfs.repoPathFor(sourceFile.fileName);
      if (repoPath && !programByFile.has(repoPath)) {
        programByFile.set(repoPath, info);
      }
    }
  }
  if (config.analysis_mode === "typed_when_available") {
    for (const entry of sourceEntries) {
      if (!programByFile.has(entry.path)) {
        configDiagnostic(
          diagnostics,
          "typed-analysis-unavailable",
          "This captured source was not part of a resolved project program; syntax facts remain available.",
          entry.path,
          vfs,
        );
      }
    }
  }

  const symbolsById = new Map();
  const relationsById = new Map();
  const sourceFiles = new Map();
  const byteOffsetsByPath = new Map();
  const moduleSymbolIds = new Map();

  function addRelation(pathName, sourceSymbolId, targetSymbolId, kind, evidenceKind, status, range) {
    if (!sourceSymbolId) return;
    const target = targetSymbolId ?? null;
    const id = stableId("rel", [pathName, String(range.start_byte), String(range.end_byte), kind, sourceSymbolId, target ?? "unknown"]);
    if (relationsById.has(id)) return;
    relationsById.set(id, {
      id,
      source_symbol_id: sourceSymbolId,
      target_symbol_id: target,
      kind,
      evidence_kind: evidenceKind,
      path: pathName,
      range,
      analysis_status: status,
    });
  }

  for (const entry of sourceEntries) {
    const fileName = vfs.absolutePath(entry.path);
    const info = programByFile.get(entry.path);
    const sourceFile = info?.program.getSourceFile(fileName) ??
      typescript.createSourceFile(fileName, entry.text, typescript.ScriptTarget.Latest, true, scriptKind(entry.path));
    sourceFiles.set(entry.path, sourceFile);
    const offsets = byteOffsets(entry.text);
    byteOffsetsByPath.set(entry.path, offsets);
    const moduleId = stableId("sym", [entry.path, "module", "0"]);
    moduleSymbolIds.set(entry.path, moduleId);
    symbolsById.set(moduleId, {
      id: moduleId,
      kind: "module",
      name: entry.path,
      path: entry.path,
      range: { start_byte: 0, end_byte: 0 },
      analysis_status: info ? "typed" : "syntax",
    });
    if (info && sourceFile.symbol) {
      info.moduleSymbols.set(sourceFile.symbol, moduleId);
    }
  }

  for (const entry of sourceEntries) {
    const info = programByFile.get(entry.path);
    const sourceFile = sourceFiles.get(entry.path);
    const offsets = byteOffsetsByPath.get(entry.path);
    const status = info ? "typed" : "syntax";
    const moduleId = moduleSymbolIds.get(entry.path);

    function visit(node) {
      if (typescript.isIdentifier(node) && isDeclarationName(node, typescript)) {
        const parent = node.parent;
        const tsSymbol = info?.checker.getSymbolAtLocation(node) ?? null;
        const range = sourceRange(sourceFile, node, offsets);
        const kind = kindForDeclaration(parent, typescript);
        const name = symbolName(tsSymbol, node.text);
        const record = {
          id: stableId("sym", [entry.path, String(range.start_byte), kind, name]),
          kind,
          name: boundedText(name),
          path: entry.path,
          range,
          analysis_status: status,
        };
        const id = addSymbol(
          symbolsById,
          info?.symbolIds ?? new Map(),
          tsSymbol,
          record,
          kind !== "import" && kind !== "export",
        );
        const declarationSource = parent.parent && info
          ? findEnclosingSymbolId(parent.parent, sourceFile, info, typescript, moduleId)
          : moduleId;
        addRelation(
          entry.path,
          declarationSource ?? moduleId,
          id,
          "declares",
          "declared",
          status,
          range,
        );
      }
      typescript.forEachChild(node, visit);
    }
    visit(sourceFile);
  }

  function findEnclosingSymbolId(node, sourceFile, info, ts, fallback) {
    let current = node;
    while (current && current !== sourceFile) {
      const name = current.name;
      if (name && ts.isIdentifier(name)) {
        const symbol = info.checker.getSymbolAtLocation(name);
        if (symbol) {
          const id = info.symbolIds.get(symbol);
          if (id) return id;
        }
      }
      current = current.parent;
    }
    return fallback;
  }

  function targetSymbolId(info, symbol) {
    if (!symbol || !info) return null;
    const resolved = resolveAlias(info.checker, symbol, typescript);
    if (!resolved) return null;
    return info.symbolIds.get(resolved) ?? info.moduleSymbols.get(resolved) ?? null;
  }

  function enclosingSymbolId(node, sourceFile, info, fallback) {
    let current = node.parent;
    while (current && current !== sourceFile) {
      const name = current.name;
      if (name && typescript.isIdentifier(name)) {
        const symbol = info.checker.getSymbolAtLocation(name);
        const id = symbol && info.symbolIds.get(symbol);
        if (id) return id;
      }
      current = current.parent;
    }
    return fallback;
  }

  for (const entry of sourceEntries) {
    const info = programByFile.get(entry.path);
    const sourceFile = sourceFiles.get(entry.path);
    const offsets = byteOffsetsByPath.get(entry.path);
    const status = info ? "typed" : "syntax";
    const moduleId = moduleSymbolIds.get(entry.path);
    const frameworkSeen = new Set();

    function visit(node) {
      if (node.kind === typescript.SyntaxKind.AnyKeyword) {
        createDiagnostic(
          diagnostics,
          diagnosticKeys,
          "typescript-any",
          "warning",
          "An any annotation weakens binding evidence for this source range.",
          entry.path,
          sourceRange(sourceFile, node, offsets),
        );
      }

      if (info && typescript.isIdentifier(node) && !isDeclarationName(node, typescript)) {
        const symbol = info.checker.getSymbolAtLocation(node);
        const resolved = resolveAlias(info.checker, symbol, typescript);
        const targetId = targetSymbolId(info, resolved);
        if (targetId) {
          addRelation(
            entry.path,
            enclosingSymbolId(node, sourceFile, info, moduleId),
            targetId,
            "references",
            "inferred",
            "typed",
            sourceRange(sourceFile, node, offsets),
          );
        } else if (resolved && isLibraryDeclaration(resolved.declarations?.[0], vfs)) {
          // Standard library symbols are known to the pinned compiler but are not repository symbols.
        }
        try {
          const type = info.checker.getTypeAtLocation(node);
          if ((type.flags & typescript.TypeFlags.Any) !== 0) {
            createDiagnostic(
              diagnostics,
              diagnosticKeys,
              "typescript-any",
              "warning",
              "The compiler resolved this expression to any; binding evidence is incomplete.",
              entry.path,
              sourceRange(sourceFile, node, offsets),
            );
          }
        } catch {
          // Syntax and unresolved type gaps are represented separately.
        }
      }

      if (typescript.isCallExpression(node)) {
        const dynamicImport = node.expression.kind === typescript.SyntaxKind.ImportKeyword;
        const commonJSRequire =
          typescript.isIdentifier(node.expression) && node.expression.text === "require";
        if (dynamicImport || commonJSRequire) {
          const kind = dynamicImport ? "dynamic-import" : "commonjs-require";
          const code = dynamicImport ? "dynamic-import-unresolved" : "commonjs-require-unresolved";
          const range = sourceRange(sourceFile, node.expression, offsets);
          addRelation(entry.path, moduleId, null, kind, "unknown", "unknown", range);
          createDiagnostic(
            diagnostics,
            diagnosticKeys,
            code,
            "warning",
            "Dynamic dependency behavior is not resolved as a static repository binding.",
            entry.path,
            range,
          );
        }
      }

      if (typescript.isImportDeclaration(node) || typescript.isExportDeclaration(node)) {
        const specifier = node.moduleSpecifier;
        if (specifier && typescript.isStringLiteralLike(specifier)) {
          const range = sourceRange(sourceFile, specifier, offsets);
          const moduleSymbol = info?.checker.getSymbolAtLocation(specifier) ?? null;
          const moduleTarget = targetSymbolId(info, moduleSymbol);
          const isExport = typescript.isExportDeclaration(node);
          const relationKind = isExport ? "reexports" : "imports";
          const syntaxStatus = info ? "typed" : "syntax";
          const bindings = [];

          if (isExport && node.exportClause && typescript.isNamedExports(node.exportClause)) {
            bindings.push(...node.exportClause.elements);
          } else if (!isExport && node.importClause) {
            if (node.importClause.name) bindings.push(node.importClause);
            const namedBindings = node.importClause.namedBindings;
            if (namedBindings && typescript.isNamedImports(namedBindings)) {
              bindings.push(...namedBindings.elements);
            } else if (namedBindings && typescript.isNamespaceImport(namedBindings)) {
              bindings.push(namedBindings);
            }
          }

          if (isExport && info && node.exportClause === undefined && moduleSymbol) {
            let exports = [];
            try {
              exports = info.checker.getExportsOfModule(moduleSymbol);
            } catch {
              exports = [];
            }
            let linked = false;
            for (const exported of exports) {
              const targetId = targetSymbolId(info, exported);
              if (!targetId) continue;
              linked = true;
              addRelation(entry.path, moduleId, targetId, "reexports", "declared", "typed", range);
            }
            if (!linked) {
              addRelation(
                entry.path,
                moduleId,
                moduleTarget,
                "reexports",
                moduleTarget ? "declared" : "unknown",
                moduleTarget ? "typed" : "unknown",
                range,
              );
            }
          } else if (bindings.length === 0) {
            addRelation(
              entry.path,
              moduleId,
              moduleTarget,
              relationKind,
              moduleTarget ? "declared" : info ? "unknown" : "declared",
              moduleTarget ? "typed" : syntaxStatus,
              range,
            );
          } else {
            for (const binding of bindings) {
              const bindingName = binding.name ?? binding;
              const bindingSymbol = info?.checker.getSymbolAtLocation(bindingName) ?? null;
              const targetId = targetSymbolId(info, bindingSymbol);
              addRelation(
                entry.path,
                moduleId,
                targetId,
                relationKind,
                targetId ? "declared" : info ? "unknown" : "declared",
                targetId ? "typed" : info ? "unknown" : "syntax",
                sourceRange(sourceFile, bindingName, offsets),
              );
            }
          }

          if (info && !moduleTarget) {
            createDiagnostic(
              diagnostics,
              diagnosticKeys,
              "module-unresolved",
              "warning",
              "Static module specifier did not bind to a captured repository source file.",
              entry.path,
              range,
            );
          }
          const moduleText = specifier.text;
          if (FRAMEWORK_MODULES.some((name) => moduleText === name || moduleText.startsWith(name + "/"))) {
            frameworkSeen.add(moduleText);
          }
        }
      }
      if (node.kind === typescript.SyntaxKind.JsxOpeningElement || node.kind === typescript.SyntaxKind.JsxSelfClosingElement) {
        frameworkSeen.add("jsx-runtime");
      }
      typescript.forEachChild(node, visit);
    }
    visit(sourceFile);

    for (const framework of frameworkSeen) {
      createDiagnostic(
        diagnostics,
        diagnosticKeys,
        "framework-semantics-unsupported",
        "warning",
        "Framework runtime and component behavior are not inferred by this provider.",
        entry.path,
        { start_byte: 0, end_byte: 0 },
      );
    }
  }

  const result = {
    schema_version: 1,
    message_type: "analyze_response",
    request_id: request.request_id,
    provider: languageProviderDescriptor,
    symbols: [...symbolsById.values()].sort((left, right) =>
      compareUtf8(left.path, right.path) ||
      left.range.start_byte - right.range.start_byte ||
      compareUtf8(left.id, right.id),
    ),
    relations: [...relationsById.values()].sort((left, right) =>
      compareUtf8(left.path, right.path) ||
      left.range.start_byte - right.range.start_byte ||
      compareUtf8(left.id, right.id),
    ),
    diagnostics: diagnostics.sort((left, right) =>
      compareUtf8(left.path, right.path) ||
      left.range.start_byte - right.range.start_byte ||
      compareUtf8(left.code, right.code),
    ),
  };
  return result;
}
