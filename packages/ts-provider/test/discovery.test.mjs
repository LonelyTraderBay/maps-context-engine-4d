import assert from "node:assert/strict";
import test from "node:test";
import { providerDescriptor, typescript } from "../src/index.mjs";

test("discovery binds the supported compiler and closed profile", () => {
  assert.equal(providerDescriptor.schemaVersion, 1);
  assert.equal(providerDescriptor.language, "typescript_javascript");
  assert.equal(providerDescriptor.compilerPackage, "@typescript/typescript6");
  assert.equal(providerDescriptor.compilerPackageVersion, "6.0.2");
  assert.equal(providerDescriptor.compilerVersion, "6.0.3");
  assert.deepEqual(providerDescriptor.languages, ["typescript", "javascript", "tsx", "jsx"]);
  assert.equal(providerDescriptor.defaultAnalysisMode, "syntax");
  assert.equal(providerDescriptor.optionalAnalysisMode, "typed_when_available");
  assert.equal(providerDescriptor.allowProjectPlugins, false);
  assert.equal(providerDescriptor.configRoots, "owned_captured_roots_only");
  assert.equal(providerDescriptor.targetScripts, "never_execute");
  assert.equal(providerDescriptor.targetBuilds, "never_start");
});

test("pinned API exposes compiler and language-service entrypoints", () => {
  for (const name of [
    "createSourceFile",
    "createProgram",
    "createLanguageService",
    "parseJsonConfigFileContent",
    "resolveModuleName",
  ]) {
    assert.equal(typeof typescript[name], "function", `${name} must be available`);
  }
});

test("syntax parsing covers TypeScript, JavaScript, TSX, and JSX fixtures", () => {
  const fixtures = [
    ["fixture.ts", "export const count: number = 1;", typescript.ScriptKind.TS],
    ["fixture.js", "export const count = 1;", typescript.ScriptKind.JS],
    ["fixture.tsx", "export const View = () => <main />;", typescript.ScriptKind.TSX],
    ["fixture.jsx", "export const View = () => <main />;", typescript.ScriptKind.JSX],
  ];

  for (const [fileName, text, scriptKind] of fixtures) {
    const source = typescript.createSourceFile(
      fileName,
      text,
      typescript.ScriptTarget.Latest,
      true,
      scriptKind,
    );
    assert.equal(source.parseDiagnostics.length, 0, `${fileName} must parse`);
    assert.equal(source.fileName, fileName);
    assert.equal(source.scriptKind, scriptKind);
  }
});

test("owned profile fixture is closed and does not enable plugins", async () => {
  const { readFile } = await import("node:fs/promises");
  const { fileURLToPath } = await import("node:url");
  const { dirname, resolve } = await import("node:path");
  const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
  const profilePath = resolve(packageRoot, "fixtures/profile.json");
  const profile = JSON.parse(await readFile(profilePath, "utf8"));

  assert.deepEqual(profile, {
    schema_version: 1,
    language: "typescript_javascript",
    tsconfig_paths: ["packages/ts-provider/fixtures/tsconfig.json"],
    compiler_digest: "a4a28b171436f8794f04854d885a222914a275da85751cf7e3a1f06bb30f6ea5",
    node_digest: "f9683a19877e12a9b167305c8db71040d812876359de21f0d2e97c3f321f80c3",
    provider_digest: "d4bc53df84723838d650a0a7acf1c4cbcbd516bfd626d9b56bdf01410e8b7f07",
    allow_project_plugins: false,
    analysis_mode: "syntax",
  });
});
