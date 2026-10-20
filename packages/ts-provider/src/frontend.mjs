import { createHash } from "node:crypto";
import path from "node:path";
import * as typescript from "@typescript/typescript6";
import { captureFiles } from "./captured-root.mjs";
import { languageProviderDescriptor } from "./descriptor.mjs";
import { validateProviderProfile } from "./profile.mjs";

const SOURCE_EXTENSIONS = new Set([".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"]);
const JSX_EXTENSIONS = new Set([".tsx", ".jsx"]);
const UNSUPPORTED_FRAMEWORK_MODULES = [
  "vue",
  "vue-router",
  "svelte",
  "@angular/",
  "next/router",
  "next/navigation",
  "next/link",
];
const MAX_FACTS = 10_000;
const MAX_DIAGNOSTICS = 10_000;
const MAX_FRONTEND_TEXT_BYTES = 4096;

export const frontendCapabilities = Object.freeze({
  schema_version: 1,
  verified_frameworks: Object.freeze(["react", "react-router-dom"]),
  route_patterns: Object.freeze(["named Route import from react-router-dom with static absolute path and element"]),
  component_patterns: Object.freeze(["named function or const arrow function in TSX/JSX containing JSX outside nested functions"]),
  client_patterns: Object.freeze(["global fetch with a static absolute path and an optional literal method option"]),
  unsupported_frameworks: Object.freeze(["Vue", "Svelte", "Angular", "Next.js", "other router conventions"]),
  generated_clients: "unsupported_calls_emit_gap",
  service_namespace: "caller_supplied_optional; links require exact service equality",
  route_component_binding: "same_file_unique_component_declaration_only",
  target_code: "never_execute",
});

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function validText(value) {
  return typeof value === "string" && value.length > 0 &&
    Buffer.from(value, "utf8").toString("utf8") === value;
}

function stableId(values) {
  const digest = createHash("sha256").update(values.join("\0"), "utf8").digest("hex");
  return "frontend:" + digest.slice(0, 48);
}

function byteOffsets(text) {
  const offsets = new Uint32Array(text.length + 1);
  let byteOffset = 0;
  for (let index = 0; index < text.length;) {
    const codePoint = text.codePointAt(index);
    const codeUnits = codePoint > 0xffff ? 2 : 1;
    offsets[index] = byteOffset;
    if (codeUnits === 2) offsets[index + 1] = byteOffset;
    byteOffset += codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
    index += codeUnits;
    offsets[index] = byteOffset;
  }
  return offsets;
}

function sourceRange(node, sourceFile, offsets) {
  const start = typeof node.getStart === "function" ? node.getStart(sourceFile, false) : node.pos;
  const end = typeof node.end === "number" ? node.end : start;
  const boundedStart = Math.max(0, Math.min(start, offsets.length - 1));
  const boundedEnd = Math.max(boundedStart, Math.min(end, offsets.length - 1));
  return { start_byte: offsets[boundedStart], end_byte: offsets[boundedEnd] };
}

function scriptKind(filePath) {
  const extension = path.posix.extname(filePath).toLowerCase();
  if (extension === ".tsx") return typescript.ScriptKind.TSX;
  if (extension === ".jsx") return typescript.ScriptKind.JSX;
  if ([".js", ".mjs", ".cjs"].includes(extension)) return typescript.ScriptKind.JS;
  return typescript.ScriptKind.TS;
}

function importFacts(sourceFile) {
  const routeNames = new Set();
  const generatedNames = new Set();
  const generatedNamespaces = new Set();
  const modules = new Set();
  let reactModule = false;

  for (const statement of sourceFile.statements) {
    if (!typescript.isImportDeclaration(statement) || !typescript.isStringLiteral(statement.moduleSpecifier)) continue;
    const moduleName = statement.moduleSpecifier.text;
    modules.add(moduleName);
    if ((moduleName === "react" || moduleName === "react/jsx-runtime" || moduleName === "react-router-dom") &&
        (!statement.importClause || !statement.importClause.isTypeOnly)) {
      reactModule = true;
    }
    if (moduleName === "react-router-dom" && statement.importClause?.namedBindings &&
        typescript.isNamedImports(statement.importClause.namedBindings)) {
      for (const item of statement.importClause.namedBindings.elements) {
        const importedName = item.propertyName?.text ?? item.name.text;
        if (importedName === "Route" && !statement.importClause.isTypeOnly && !item.isTypeOnly) {
          routeNames.add(item.name.text);
        }
      }
    }

    const generatedModule = /(^|[./_-])generated([./_-]|$)/i.test(moduleName) ||
      ["openapi-fetch", "@hey-api/client-fetch", "orval"].includes(moduleName);
    if (generatedModule && statement.importClause?.namedBindings) {
      const bindings = statement.importClause.namedBindings;
      if (typescript.isNamedImports(bindings)) {
        for (const item of bindings.elements) generatedNames.add(item.name.text);
      } else if (typescript.isNamespaceImport(bindings)) {
        generatedNamespaces.add(bindings.name.text);
      }
    }
  }
  return { routeNames, generatedNames, generatedNamespaces, modules, reactModule };
}

function isFunctionNode(node) {
  return typescript.isFunctionDeclaration(node) || typescript.isFunctionExpression(node) ||
    typescript.isArrowFunction(node) || typescript.isMethodDeclaration(node);
}

function containsJsx(root) {
  let found = false;
  function visit(node, isRoot) {
    if (!isRoot && isFunctionNode(node)) return;
    if (typescript.isJsxElement(node) || typescript.isJsxSelfClosingElement(node)) {
      found = true;
      return;
    }
    typescript.forEachChild(node, (child) => visit(child, false));
  }
  visit(root, true);
  return found;
}

function componentDeclarations(sourceFile) {
  const declarations = [];
  function visit(node) {
    if (typescript.isFunctionDeclaration(node) && node.name && /^[A-Z]/.test(node.name.text) &&
        node.body && containsJsx(node.body)) {
      declarations.push({ name: node.name.text, nameNode: node.name });
    } else if (typescript.isVariableDeclaration(node) && typescript.isIdentifier(node.name) &&
        /^[A-Z]/.test(node.name.text) && node.initializer &&
        (typescript.isArrowFunction(node.initializer) || typescript.isFunctionExpression(node.initializer)) &&
        node.initializer.body && containsJsx(node.initializer.body)) {
      declarations.push({ name: node.name.text, nameNode: node.name });
    }
    typescript.forEachChild(node, visit);
  }
  visit(sourceFile);
  return declarations;
}

function staticString(node) {
  if (node && typescript.isStringLiteralLike(node)) return node.text;
  return null;
}

function staticJsxAttribute(attribute) {
  if (!attribute || !typescript.isJsxAttribute(attribute)) return null;
  if (attribute.initializer && typescript.isStringLiteral(attribute.initializer)) {
    return attribute.initializer.text;
  }
  if (attribute.initializer && typescript.isJsxExpression(attribute.initializer)) {
    return staticString(attribute.initializer.expression);
  }
  return null;
}

function jsxComponentName(node) {
  const element = typescript.isJsxElement(node) ? node.openingElement : node;
  return typescript.isIdentifier(element.tagName) && /^[A-Z]/.test(element.tagName.text)
    ? element.tagName.text
    : null;
}

function attributeNamed(attributes, name) {
  return attributes.properties.find((item) =>
    typescript.isJsxAttribute(item) && typescript.isIdentifier(item.name) && item.name.text === name,
  ) ?? null;
}

function isStaticApiPath(value) {
  return validText(value) && Buffer.byteLength(value, "utf8") <= MAX_FRONTEND_TEXT_BYTES &&
    value.startsWith("/") && !/[?#\\\u0000-\u001f\u007f]/u.test(value);
}

function hasFetchBinding(sourceFile) {
  let shadowed = false;
  function visit(node) {
    if (typescript.isIdentifier(node) && node.text === "fetch") {
      const parent = node.parent;
      if (parent && (
        (typescript.isVariableDeclaration(parent) && parent.name === node) ||
        (typescript.isFunctionDeclaration(parent) && parent.name === node) ||
        (typescript.isParameter(parent) && parent.name === node) ||
        (typescript.isImportSpecifier(parent) && parent.name === node) ||
        (typescript.isImportClause(parent) && parent.name === node)
      )) shadowed = true;
    }
    typescript.forEachChild(node, visit);
  }
  visit(sourceFile);
  return shadowed;
}

function fetchMethod(call) {
  const options = call.arguments[1];
  if (!options) return { method: "GET" };
  if (!typescript.isObjectLiteralExpression(options)) return { error: "frontend-client-dynamic-method" };
  let method = null;
  for (const property of options.properties) {
    if (typescript.isSpreadAssignment(property)) return { error: "frontend-client-dynamic-method" };
    if (property.name && typescript.isComputedPropertyName(property.name)) {
      return { error: "frontend-client-dynamic-method" };
    }
    if (!typescript.isPropertyAssignment(property)) {
      if (property.name && (typescript.isIdentifier(property.name) || typescript.isStringLiteral(property.name)) &&
          property.name.text === "method") {
        return { error: "frontend-client-dynamic-method" };
      }
      continue;
    }
    const name = typescript.isIdentifier(property.name) || typescript.isStringLiteral(property.name)
      ? property.name.text
      : null;
    if (name !== "method") continue;
    if (method !== null) return { error: "frontend-client-duplicate-method" };
    method = staticString(property.initializer);
    if (!method) return { error: "frontend-client-dynamic-method" };
  }
  const normalized = (method ?? "GET").toUpperCase();
  if (!/^[A-Z][A-Z0-9!#$%&'*+.^_`|~-]{0,31}$/.test(normalized)) {
    return { error: "frontend-client-invalid-method" };
  }
  return { method: normalized };
}

function makeFact({ serviceId, profileId, file, kind, name, node, sourceFile, offsets, ...extra }) {
  const range = sourceRange(node, sourceFile, offsets);
  const fact = {
    id: stableId([serviceId ?? "", profileId, kind, file.path, file.content_sha256,
      String(range.start_byte), String(range.end_byte), name, extra.method ?? "", extra.path ?? ""]),
    kind,
    name,
    service_id: serviceId,
    analysis_status: serviceId ? "exact_in_supported_scope" : "candidate",
    source: { path: file.path, content_hash: file.content_sha256, range },
  };
  if (extra.path !== undefined) fact.path = extra.path;
  if (extra.method !== undefined) fact.method = extra.method;
  if (extra.component_name !== undefined) fact.component_name = extra.component_name;
  if (extra.component_id !== undefined) fact.component_id = extra.component_id;
  return fact;
}

export function analyzeFrontendRequest(request, options = {}) {
  if (!options || typeof options !== "object" || Array.isArray(options) ||
      Object.keys(options).some((key) => key !== "service_id")) {
    throw new Error("frontend options contain missing or unknown fields");
  }
  let serviceId = null;
  if (options.service_id !== undefined && options.service_id !== null && options.service_id !== "") {
    if (!validText(options.service_id) || Buffer.byteLength(options.service_id, "utf8") > 192 ||
        /[\u0000-\u001f\u007f]/u.test(options.service_id)) {
      throw new Error("frontend service_id must be a nonempty UTF-8 identifier of at most 192 bytes");
    }
    serviceId = options.service_id;
  }

  const { profile } = validateProviderProfile(request, languageProviderDescriptor.digest);
  const files = captureFiles(request.files);
  const entries = [...files.values()]
    .filter((file) => SOURCE_EXTENSIONS.has(path.posix.extname(file.path).toLowerCase()))
    .map((file) => ({
      ...file,
      content_sha256: createHash("sha256").update(file.bytes).digest("hex"),
    }))
    .sort((left, right) => compareUtf8(left.path, right.path));
  if (entries.length === 0) throw new Error("provider request contains no supported TypeScript or JavaScript source");

  const facts = [];
  const diagnostics = [];
  const diagnosticKeys = new Set();
  let factLimitReached = false;
  let diagnosticLimitReached = false;

  function addDiagnostic(code, message, file, range) {
    const key = [code, file.path, range.start_byte, range.end_byte].join(":");
    if (diagnosticKeys.has(key)) return;
    if (diagnostics.length >= MAX_DIAGNOSTICS) {
      diagnosticLimitReached = true;
      return;
    }
    diagnosticKeys.add(key);
    diagnostics.push({ code, severity: "warning", message, path: file.path, range });
  }

  function addFact(fact) {
    if (facts.length >= MAX_FACTS) {
      factLimitReached = true;
      return;
    }
    facts.push(fact);
    return true;
  }

  if (!serviceId) {
    addDiagnostic("frontend-service-namespace-missing",
      "Frontend facts have source evidence but cannot be linked across services without an explicit service namespace.",
      entries[0], { start_byte: 0, end_byte: 0 });
  }

  for (const file of entries) {
    const sourceFile = typescript.createSourceFile(
      file.path, file.text, typescript.ScriptTarget.Latest, true, scriptKind(file.path),
    );
    const offsets = byteOffsets(file.text);
    if (sourceFile.parseDiagnostics.length > 0) {
      addDiagnostic("frontend-parse-error",
        "Frontend facts were omitted because this captured source has syntax errors.",
        file, { start_byte: 0, end_byte: 0 });
      continue;
    }

    const imports = importFacts(sourceFile);
    const unsupportedModules = [...imports.modules].filter((moduleName) =>
      UNSUPPORTED_FRAMEWORK_MODULES.some((prefix) => moduleName === prefix || moduleName.startsWith(prefix)),
    );
    for (const moduleName of unsupportedModules) {
      addDiagnostic("frontend-framework-unsupported",
        "Framework-specific route and component conventions are outside the verified React/TSX subset.",
        file, { start_byte: 0, end_byte: 0 });
    }

    const extension = path.posix.extname(file.path).toLowerCase();
    const supportsJsx = JSX_EXTENSIONS.has(extension) && imports.reactModule && unsupportedModules.length === 0;
    if (JSX_EXTENSIONS.has(extension) && !supportsJsx) {
      addDiagnostic("frontend-framework-unverified",
        "JSX was parsed, but React/React Router ownership is not explicit enough to emit route or component facts.",
        file, { start_byte: 0, end_byte: 0 });
    }

    const componentIDs = new Map();
    if (supportsJsx) {
      for (const declaration of componentDeclarations(sourceFile)) {
        if (Buffer.byteLength(declaration.name, "utf8") > MAX_FRONTEND_TEXT_BYTES) {
          addDiagnostic("frontend-fact-text-limit",
            "The component name exceeds the frontend fact text limit and was omitted.",
            file, sourceRange(declaration.nameNode, sourceFile, offsets));
          continue;
        }
        const fact = makeFact({
          serviceId, profileId: profile.profile_id, file, kind: "component", name: declaration.name,
          node: declaration.nameNode, sourceFile, offsets,
        });
        if (addFact(fact)) {
          const ids = componentIDs.get(declaration.name) ?? [];
          ids.push(fact.id);
          componentIDs.set(declaration.name, ids);
        }
      }

      if (imports.routeNames.size > 0) {
        function visitRoutes(node) {
          const opening = node;
          if ((typescript.isJsxOpeningElement(opening) || typescript.isJsxSelfClosingElement(opening)) &&
              typescript.isIdentifier(opening.tagName) && imports.routeNames.has(opening.tagName.text)) {
            const pathAttribute = attributeNamed(opening.attributes, "path");
            const routePath = staticJsxAttribute(pathAttribute);
            const routeRange = sourceRange(opening, sourceFile, offsets);
            if (!isStaticApiPath(routePath)) {
              addDiagnostic("frontend-route-dynamic-or-unsupported-path",
                "Only static absolute React Router paths are emitted as route facts.", file, routeRange);
            } else {
              const elementAttribute = attributeNamed(opening.attributes, "element");
              const expression = elementAttribute?.initializer && typescript.isJsxExpression(elementAttribute.initializer)
                ? elementAttribute.initializer.expression
                : null;
              const componentName = expression ? jsxComponentName(expression) : null;
              const ids = componentName ? componentIDs.get(componentName) ?? [] : [];
              if (elementAttribute && !componentName) {
                addDiagnostic("frontend-route-component-unsupported",
                  "The route element does not contain a single static component identifier.", file, routeRange);
              } else if (componentName && ids.length === 0) {
                addDiagnostic("frontend-route-component-unresolved",
                  "The route component has no same-file source declaration; cross-module binding was not inferred.",
                  file, routeRange);
              } else if (componentName && ids.length > 1) {
                addDiagnostic("frontend-route-component-ambiguous",
                  "The route component name has multiple local definitions; no component link was selected.",
                  file, routeRange);
              }
              const fact = makeFact({
                serviceId, profileId: profile.profile_id, file, kind: "route", name: routePath,
                node: opening, sourceFile, offsets, path: routePath,
                ...(componentName ? { component_name: componentName } : {}),
                ...(ids.length === 1 ? { component_id: ids[0] } : {}),
              });
              addFact(fact);
            }
          }
          typescript.forEachChild(node, visitRoutes);
        }
        visitRoutes(sourceFile);
      }
    }

    const fetchIsShadowed = hasFetchBinding(sourceFile);
    function visitClients(node) {
      if (typescript.isCallExpression(node) && typescript.isIdentifier(node.expression) && node.expression.text === "fetch") {
        const range = sourceRange(node, sourceFile, offsets);
        if (fetchIsShadowed) {
          addDiagnostic("frontend-client-fetch-shadowed",
            "A local or imported fetch binding makes the request target ambiguous; no API client fact was emitted.",
            file, range);
        } else if (node.arguments.length === 0) {
          addDiagnostic("frontend-client-dynamic-path", "fetch has no statically captured request path.", file, range);
        } else {
          const requestPath = staticString(node.arguments[0]);
          if (!isStaticApiPath(requestPath)) {
            addDiagnostic("frontend-client-dynamic-path",
              "Only a static absolute fetch path is emitted as an API client fact.", file, range);
          } else {
            const method = fetchMethod(node);
            if (method.error) {
              addDiagnostic(method.error, "The fetch method is not a single static string literal.", file, range);
            } else {
              addFact(makeFact({
                serviceId, profileId: profile.profile_id, file, kind: "api_client", name: "fetch",
                node, sourceFile, offsets, method: method.method, path: requestPath,
              }));
            }
          }
        }
      }
      if (typescript.isCallExpression(node)) {
        const expression = node.expression;
        const generatedCall = typescript.isIdentifier(expression) && imports.generatedNames.has(expression.text) ||
          typescript.isPropertyAccessExpression(expression) && typescript.isIdentifier(expression.expression) &&
            imports.generatedNamespaces.has(expression.expression.text);
        if (generatedCall) {
          addDiagnostic("frontend-generated-client-unsupported",
            "Generated client calls are retained as a gap until their operation binding has source-backed support.",
            file, sourceRange(node, sourceFile, offsets));
        }
      }
      typescript.forEachChild(node, visitClients);
    }
    visitClients(sourceFile);
  }

  if (factLimitReached) {
    addDiagnostic("frontend-fact-limit",
      "The frontend fact budget was reached; additional facts were omitted and the result is partial.",
      entries[entries.length - 1], { start_byte: 0, end_byte: 0 });
  }
  if (diagnosticLimitReached) {
    const marker = {
      code: "frontend-diagnostic-limit",
      severity: "warning",
      message: "The diagnostic budget was reached; additional diagnostics were omitted and the result is partial.",
      path: entries[entries.length - 1].path,
      range: { start_byte: 0, end_byte: 0 },
    };
    if (diagnostics.length >= MAX_DIAGNOSTICS) diagnostics[MAX_DIAGNOSTICS - 1] = marker;
    else diagnostics.push(marker);
  }

  facts.sort((left, right) => compareUtf8(left.source.path, right.source.path) ||
    left.source.range.start_byte - right.source.range.start_byte ||
    left.source.range.end_byte - right.source.range.end_byte ||
    compareUtf8(left.kind, right.kind) || compareUtf8(left.id, right.id));
  diagnostics.sort((left, right) => compareUtf8(left.path, right.path) ||
    left.range.start_byte - right.range.start_byte || compareUtf8(left.code, right.code));

  return {
    schema_version: 1,
    message_type: "frontend_analysis_response",
    request_id: request.request_id,
    profile_id: profile.profile_id,
    service_id: serviceId,
    capabilities: frontendCapabilities,
    truncated: factLimitReached || diagnosticLimitReached,
    facts,
    diagnostics,
  };
}
