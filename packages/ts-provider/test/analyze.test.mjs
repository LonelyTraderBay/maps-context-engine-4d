import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { dirname, resolve } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { analyzeProviderRequest, languageProviderDescriptor } from "../src/index.mjs";
import { computeProviderManifestDigest } from "../src/descriptor.mjs";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const compilerDigest = "a4a28b171436f8794f04854d885a222914a275da85751cf7e3a1f06bb30f6ea5";
const nodeDigest = "f9683a19877e12a9b167305c8db71040d812876359de21f0d2e97c3f321f80c3";
const requestId = "123e4567-e89b-12d3-a456-426614174000";
const validIdentifier = /^[A-Za-z][A-Za-z0-9._:/+-]{0,127}$/;

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function capturedFile(path, text) {
  const bytes = Buffer.from(text, "utf8");
  return {
    path,
    content_base64: bytes.toString("base64"),
    content_sha256: createHash("sha256").update(bytes).digest("hex"),
  };
}

function makeRequest(fileText, options = {}) {
  const configPaths = options.configPaths ?? ["tsconfig.json"];
  const configPath = options.configPath ?? configPaths[0];
  const config = options.config ?? {
    compilerOptions: {
      baseUrl: ".",
      module: "ESNext",
      moduleResolution: "Bundler",
      skipLibCheck: true,
      strict: true,
      target: "ES2022",
    },
    include: ["src/**/*"],
  };
  const files = Object.entries(fileText).map(([path, text]) => capturedFile(path, text));
  if (!Object.hasOwn(fileText, configPath)) {
    files.push(capturedFile(configPath, JSON.stringify(config)));
  }
  files.sort((left, right) => compareUtf8(left.path, right.path));

  return {
    schema_version: 1,
    message_type: "analyze_request",
    request_id: requestId,
    language: "typescript",
    profile: {
      schema_id: "typescript-profile",
      schema_version: 1,
      profile_id: "c".repeat(64),
      config: {
        schema_version: 1,
        language: "typescript_javascript",
        tsconfig_paths: [...configPaths].sort(compareUtf8),
        compiler_digest: compilerDigest,
        node_digest: nodeDigest,
        provider_digest: languageProviderDescriptor.digest,
        allow_project_plugins: false,
        analysis_mode: options.mode ?? "typed_when_available",
      },
    },
    files,
    execution_policy: {
      target_code: "never_execute",
      project_scripts: "never_execute",
      project_plugins: "never_execute",
      notebook_cells: "never_execute",
      network: false,
    },
  };
}

function assertResultContract(result, request) {
  const sourceByPath = new Map(
    request.files.map((file) => [file.path, Buffer.from(file.content_base64, "base64")]),
  );
  const symbols = new Map();
  for (const symbol of result.symbols) {
    assert.match(symbol.id, validIdentifier);
    assert.match(symbol.kind, validIdentifier);
    assert.ok(symbol.name.length > 0);
    assert.ok([...symbol.name].length <= 4096);
    assert.ok(!symbol.name.includes("\0"));
    assert.ok(sourceByPath.has(symbol.path));
    assert.ok(symbol.range.start_byte >= 0);
    assert.ok(symbol.range.end_byte >= symbol.range.start_byte);
    assert.ok(symbol.range.end_byte <= sourceByPath.get(symbol.path).length);
    assert.ok(["syntax", "typed", "unknown"].includes(symbol.analysis_status));
    assert.ok(!symbols.has(symbol.id), "symbol IDs must be unique");
    symbols.set(symbol.id, symbol);
  }
  for (const relation of result.relations) {
    assert.match(relation.id, validIdentifier);
    assert.match(relation.kind, validIdentifier);
    assert.ok(symbols.has(relation.source_symbol_id));
    if (relation.target_symbol_id !== null) assert.ok(symbols.has(relation.target_symbol_id));
    assert.ok(sourceByPath.has(relation.path));
    assert.ok(relation.range.end_byte <= sourceByPath.get(relation.path).length);
    assert.ok(["declared", "inferred", "unknown"].includes(relation.evidence_kind));
    assert.ok(["syntax", "typed", "unknown"].includes(relation.analysis_status));
  }
  for (const diagnostic of result.diagnostics) {
    assert.match(diagnostic.code, validIdentifier);
    assert.ok(sourceByPath.has(diagnostic.path));
    assert.ok([...diagnostic.message].length <= 4096);
    assert.ok(!diagnostic.message.includes("\0"));
    assert.ok(diagnostic.range.end_byte <= sourceByPath.get(diagnostic.path).length);
  }
  assert.equal(result.schema_version, 1);
  assert.equal(result.message_type, "analyze_response");
  assert.equal(result.request_id, requestId);
  assert.deepEqual(result.provider, languageProviderDescriptor);
}

test("provider digest matches the canonical package source manifest", () => {
  const sourceDirectory = resolve(packageRoot, "src");
  const paths = [
    "package-lock.json",
    "package.json",
    ...readdirSync(sourceDirectory).filter((name) => name.endsWith(".mjs")).map((name) => "src/" + name),
  ].sort(compareUtf8);
  const files = paths.map((path) => ({
    path,
    bytes: readFileSync(resolve(packageRoot, path)),
  }));
  assert.equal(languageProviderDescriptor.digest, computeProviderManifestDigest(files));
});

test("provider digest is stable across LF, CRLF, and lone-CR checkout bytes", () => {
  const source = "export const label = \"café\";\nexport const ready = true;\n";
  const json = "{\n  \"type\": \"module\"\n}\n";
  const toFiles = (newline) => [
    { path: "src/main.mjs", bytes: Buffer.from(source.replace(/\n/g, newline), "utf8") },
    { path: "package.json", bytes: Buffer.from(json.replace(/\n/g, newline), "utf8") },
  ];
  const lfDigest = computeProviderManifestDigest(toFiles("\n"));
  assert.equal(computeProviderManifestDigest(toFiles("\r\n")), lfDigest);
  assert.equal(computeProviderManifestDigest(toFiles("\r")), lfDigest);
});

test("provider digest rejects manifest content that is not UTF-8", () => {
  assert.throws(
    () => computeProviderManifestDigest([{ path: "src/main.mjs", bytes: Buffer.from([0xc3, 0x28]) }]),
    /encoded data/,
  );
});

test("analysis emits syntax symbols for JavaScript and JSX files", () => {
  const request = makeRequest({
    "src/legacy.js": "export function count() { return 1; }",
    "src/view.jsx": "export const View = () => <main />;",
  }, {
    mode: "syntax",
    config: { include: ["src/**/*"] },
  });
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.symbols.some((symbol) =>
    symbol.path === "src/legacy.js" && symbol.name === "count" && symbol.analysis_status === "syntax",
  ));
  assert.ok(result.symbols.some((symbol) =>
    symbol.path === "src/view.jsx" && symbol.name === "View" && symbol.analysis_status === "syntax",
  ));
});

test("typed graph binds path aliases, same-name declarations, references and re-exports", () => {
  const request = makeRequest({
    "src/shared/item.ts": [
      "export interface Item { readonly label: string }",
      'export const sharedValue = "ok";',
    ].join("\n"),
    "src/barrel.ts": [
      'export { sharedValue as publicValue } from "@shared/item";',
      'export * from "@shared/item";',
    ].join("\n"),
    "src/app.ts": [
      'import { publicValue } from "./barrel";',
      'import type { Item } from "@shared/item";',
      "export const duplicate = publicValue;",
      "export const item: Item = { label: duplicate };",
      "export function first() { const local = 1; return local; }",
      'export function second() { const local = "x"; return local; }',
    ].join("\n"),
    "src/other.ts": 'export const duplicate = 2;',
  }, {
    config: {
      compilerOptions: {
        baseUrl: ".",
        module: "ESNext",
        moduleResolution: "Bundler",
        paths: { "@shared/*": ["src/shared/*"] },
        skipLibCheck: true,
        strict: true,
        target: "ES2022",
      },
      include: ["src/**/*"],
    },
  });

  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.symbols.some((symbol) => symbol.path === "src/app.ts" && symbol.name === "item" && symbol.analysis_status === "typed"));

  const duplicates = result.symbols.filter((symbol) => symbol.name === "duplicate");
  assert.equal(duplicates.length, 2);
  assert.equal(new Set(duplicates.map((symbol) => symbol.id)).size, 2);
  assert.deepEqual(new Set(duplicates.map((symbol) => symbol.path)), new Set(["src/app.ts", "src/other.ts"]));
  const localSymbols = result.symbols.filter((symbol) => symbol.path === "src/app.ts" && symbol.name === "local");
  assert.equal(localSymbols.length, 2);
  assert.equal(new Set(localSymbols.map((symbol) => symbol.id)).size, 2);
  for (const local of localSymbols) {
    assert.ok(result.relations.some((relation) =>
      relation.kind === "references" &&
      relation.path === "src/app.ts" &&
      relation.target_symbol_id === local.id,
    ));
  }

  const sharedValue = result.symbols.find((symbol) => symbol.path === "src/shared/item.ts" && symbol.name === "sharedValue");
  assert.ok(sharedValue);
  assert.ok(result.relations.some((relation) =>
    relation.path === "src/barrel.ts" &&
    relation.kind === "reexports" &&
    relation.target_symbol_id === sharedValue.id &&
    relation.analysis_status === "typed",
  ));
  assert.ok(result.relations.some((relation) =>
    relation.path === "src/app.ts" &&
    relation.kind === "references" &&
    relation.target_symbol_id === sharedValue.id,
  ));
});

test("syntax mode records declared imports and keeps dynamic imports unknown", () => {
  const request = makeRequest({
    "src/main.ts": [
      'import { value } from "./missing";',
      'export const result = import("./runtime-choice");',
      "void value;",
    ].join("\n"),
  }, {
    mode: "syntax",
    config: { include: ["src/**/*"] },
  });

  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.relations.some((relation) =>
    relation.kind === "imports" &&
    relation.evidence_kind === "declared" &&
    relation.analysis_status === "syntax" &&
    relation.target_symbol_id === null,
  ));
  assert.ok(result.relations.some((relation) =>
    relation.kind === "dynamic-import" &&
    relation.evidence_kind === "unknown" &&
    relation.analysis_status === "unknown" &&
    relation.target_symbol_id === null,
  ));
  assert.ok(result.diagnostics.some((diagnostic) => diagnostic.code === "dynamic-import-unresolved"));
});

test("any, dynamic imports, require and framework syntax remain explicit gaps", () => {
  const request = makeRequest({
    "src/gaps.ts": [
      'export const value: any = { answer: 42 };',
      "export const result = value.answer;",
      'export const loaded = import("./runtime-choice");',
      'const required = require("runtime-choice");',
    ].join("\n"),
    "src/view.tsx": "export const View = () => <main />;",
  }, {
    config: {
      compilerOptions: { allowJs: true, jsx: "Preserve", module: "ESNext", moduleResolution: "Bundler", skipLibCheck: true, target: "ES2022" },
      include: ["src/**/*"],
    },
  });
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.diagnostics.some((diagnostic) =>
    diagnostic.path === "src/gaps.ts" && diagnostic.code === "typescript-any",
  ));
  assert.ok(result.relations.some((relation) =>
    relation.path === "src/gaps.ts" && relation.kind === "dynamic-import" && relation.analysis_status === "unknown",
  ));
  assert.ok(result.relations.some((relation) =>
    relation.path === "src/gaps.ts" && relation.kind === "commonjs-require" && relation.analysis_status === "unknown",
  ));
  assert.ok(result.diagnostics.some((diagnostic) =>
    diagnostic.path === "src/view.tsx" && diagnostic.code === "framework-semantics-unsupported",
  ));
});

test("project plugins are rejected before compiler program creation", () => {
  const request = makeRequest({
    "src/main.ts": "export const safe = true;",
    "plugin.cjs": 'throw new Error("target plugin must never execute");',
  }, {
    config: {
      compilerOptions: {
        plugins: [{ name: "./plugin.cjs" }],
        strict: true,
      },
      include: ["src/**/*"],
    },
  });

  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.diagnostics.some((diagnostic) => diagnostic.code === "project-plugins-rejected"));
  assert.ok(result.symbols.some((symbol) => symbol.name === "safe" && symbol.analysis_status === "syntax"));
  assert.ok(!result.diagnostics.some((diagnostic) => diagnostic.code === "typescript-program-unavailable"));
});

test("extends outside the captured root is reported without loading it", () => {
  const request = makeRequest(
    { "src/main.ts": "export const safe = true;" },
    { config: { extends: "https://example.invalid/tsconfig.json", include: ["src/**/*"] } },
  );
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.diagnostics.some((diagnostic) => diagnostic.code === "config-reference-not-captured"));
  assert.ok(result.symbols.some((symbol) => symbol.name === "safe" && symbol.analysis_status === "syntax"));
});

test("missing profile config keeps diagnostics attached to a captured file", () => {
  const request = makeRequest(
    { "src/main.ts": "export const safe = true;" },
    { configPath: "missing/tsconfig.json", configPaths: ["missing/tsconfig.json"], mode: "syntax" },
  );
  request.files = request.files.filter((file) => file.path !== "missing/tsconfig.json");
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.diagnostics.some((diagnostic) =>
    diagnostic.code === "config-not-captured" && diagnostic.path === "src/main.ts",
  ));
});

test("captured config references must also be named by the profile allowlist", () => {
  const request = makeRequest({
    "src/main.ts": "export const safe = true;",
    "base/tsconfig.json": JSON.stringify({ compilerOptions: { strict: true }, include: ["src/**/*"] }),
  }, {
    config: { extends: "./base/tsconfig.json", include: ["src/**/*"] },
    configPaths: ["tsconfig.json"],
  });
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.diagnostics.some((diagnostic) => diagnostic.code === "config-reference-not-allowlisted"));
  assert.ok(result.symbols.some((symbol) => symbol.name === "safe" && symbol.analysis_status === "syntax"));
});

test("captured project references are parsed from their allowlisted configs", () => {
  const request = makeRequest({
    "base/tsconfig.json": JSON.stringify({
      compilerOptions: { composite: true, declaration: true, module: "ESNext", moduleResolution: "Bundler", skipLibCheck: true, target: "ES2022" },
      include: ["src/**/*"],
    }),
    "base/src/index.ts": "export interface Base { id: string }",
    "app/tsconfig.json": JSON.stringify({
      compilerOptions: {
        baseUrl: ".",
        composite: true,
        module: "ESNext",
        moduleResolution: "Bundler",
        paths: { "@base/*": ["../base/src/*"] },
        skipLibCheck: true,
        target: "ES2022",
      },
      include: ["src/**/*"],
      references: [{ path: "../base" }],
    }),
    "app/src/main.ts": [
      'import type { Base } from "@base/index";',
      "export const value: Base = { id: 'ok' };",
    ].join("\n"),
  }, {
    configPaths: ["app/tsconfig.json", "base/tsconfig.json"],
    configPath: "app/tsconfig.json",
  });

  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  assert.ok(result.symbols.some((symbol) =>
    symbol.path === "app/src/main.ts" &&
    symbol.name === "value" &&
    symbol.analysis_status === "typed",
  ));
  const baseType = result.symbols.find((symbol) =>
    symbol.path === "base/src/index.ts" && symbol.name === "Base",
  );
  assert.ok(baseType, "referenced project declarations must join the symbol graph");
  assert.ok(result.relations.some((relation) =>
    relation.path === "app/src/main.ts" &&
    relation.kind === "imports" &&
    relation.target_symbol_id === baseType.id &&
    relation.analysis_status === "typed",
  ));
  assert.ok(!result.diagnostics.some((diagnostic) => diagnostic.code === "project-reference-not-captured"));
});

test("profile, content encoding and captured paths fail closed", () => {
  const base = makeRequest({ "src/main.ts": "export const safe = true;" }, { mode: "syntax" });
  const withUnknown = structuredClone(base);
  withUnknown.untrusted = true;
  assert.throws(() => analyzeProviderRequest(withUnknown), /missing or unknown fields/);

  const withBadDigest = structuredClone(base);
  withBadDigest.files[1].content_sha256 = "0".repeat(64);
  assert.throws(() => analyzeProviderRequest(withBadDigest), /digest mismatch/);

  const withTraversal = structuredClone(base);
  withTraversal.files[1].path = "../outside.ts";
  assert.throws(() => analyzeProviderRequest(withTraversal), /invalid captured repository path/);

  const withBadBase64 = structuredClone(base);
  withBadBase64.files[1].content_base64 = "%%%";
  assert.throws(() => analyzeProviderRequest(withBadBase64), /invalid base64/);
});

test("schema text limits are applied to oversized symbol names", () => {
  const longName = "x".repeat(5000);
  const request = makeRequest({
    "src/long-name.ts": "export const " + longName + " = 1;",
  }, { mode: "syntax", config: { include: ["src/**/*"] } });
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  const symbol = result.symbols.find((item) => item.path === "src/long-name.ts" && item.kind === "variable");
  assert.ok(symbol);
  assert.equal([...symbol.name].length, 4096);
});

test("symbol byte ranges account for non-ASCII source text", () => {
  const request = makeRequest({
    "src/unicode.ts": "const café = 1;\nexport { café };",
  }, { mode: "syntax", config: { include: ["src/**/*"] } });
  const result = analyzeProviderRequest(request);
  assertResultContract(result, request);
  const symbol = result.symbols.find((item) => item.path === "src/unicode.ts" && item.name === "café");
  assert.ok(symbol);
  const source = Buffer.from("const café = 1;\nexport { café };", "utf8");
  assert.equal(source.subarray(symbol.range.start_byte, symbol.range.end_byte).toString("utf8"), "café");
});

test("framed CLI returns one bounded protocol response without extra stdout", () => {
  const request = makeRequest({ "src/main.ts": "export const result = 42;" }, { mode: "syntax", config: { include: ["src/**/*"] } });
  const body = Buffer.from(JSON.stringify(request), "utf8");
  const frame = Buffer.concat([Buffer.from("Content-Length: " + body.length + "\r\n\r\n"), body]);
  const cliPath = resolve(packageRoot, "src/cli.mjs");
  const child = spawnSync(process.execPath, [cliPath], { input: frame, maxBuffer: 2 * 1024 * 1024 });
  assert.equal(child.status, 0, child.stderr.toString("utf8"));
  const output = child.stdout;
  const separator = output.indexOf(Buffer.from("\r\n\r\n"));
  assert.ok(separator > 0);
  const match = /^Content-Length: ([1-9][0-9]*)$/.exec(output.subarray(0, separator).toString("ascii"));
  assert.ok(match);
  const responseBytes = output.subarray(separator + 4);
  assert.equal(responseBytes.length, Number(match[1]));
  const response = JSON.parse(responseBytes.toString("utf8"));
  assert.equal(response.message_type, "analyze_response");
  assert.equal(response.request_id, requestId);
});

test("framed CLI rejects trailing bytes and writes no response frame", () => {
  const request = makeRequest({ "src/main.ts": "export const result = 42;" }, { mode: "syntax", config: { include: ["src/**/*"] } });
  const body = Buffer.from(JSON.stringify(request), "utf8");
  const frame = Buffer.concat([Buffer.from("Content-Length: " + body.length + "\r\n\r\n"), body, Buffer.from("x")]);
  const child = spawnSync(process.execPath, [resolve(packageRoot, "src/cli.mjs")], { input: frame, maxBuffer: 2 * 1024 * 1024 });
  assert.equal(child.status, 1);
  assert.equal(child.stdout.length, 0);
});
