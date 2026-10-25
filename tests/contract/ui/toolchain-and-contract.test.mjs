import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, readdir, rm, symlink, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import test from "node:test";

import * as react from "react";
import * as reactDOM from "react-dom";
import { ReactFlow } from "@xyflow/react";
import { graphlib, layout } from "@dagrejs/dagre";
import { build, createServer } from "vite";
import reactPlugin from "@vitejs/plugin-react";
import ts from "typescript";
import { JSDOM } from "jsdom";
import { render } from "@testing-library/react";
import { chromium, devices } from "@playwright/test";
import {
  canonicalEncode,
  canonicalJSON1,
  createViteInspector,
  hashRaw,
  hashValue,
} from "../../../packages/vite-inspector/index.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "../../..");
const readJSON = async (path) => JSON.parse(await readFile(join(root, path), "utf8"));

test("Node and npm match the sealed workspace runtime", () => {
  assert.equal(process.version, "v24.22.0");
  assert.match(process.env.npm_config_user_agent ?? "", /^npm\/11\.19\.1\s/);
});

test("workspace manifests keep every direct dependency exact", async () => {
  const workspace = await readJSON("package.json");
  const explorer = await readJSON("web/explorer/package.json");
  const lock = await readJSON("package-lock.json");
  assert.equal(workspace.packageManager, "npm@11.19.1");
  assert.deepEqual(workspace.workspaces, ["web/explorer"]);
  assert.deepEqual(lock.packages[""].workspaces, ["web/explorer"]);
  assert.equal(lock.lockfileVersion, 3);

  for (const [name, version] of Object.entries({
    ...explorer.dependencies,
    ...explorer.devDependencies,
  })) {
    assert.match(version, /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/, `${name} must be exact`);
    assert.equal(lock.packages[`node_modules/${name}`]?.version, version, `${name} lock pin`);
    assert.ok(lock.packages[`node_modules/${name}`]?.integrity, `${name} registry integrity`);
  }
  assert.equal((await readJSON("node_modules/@xyflow/react/package.json")).license, "MIT");
  assert.equal((await readJSON("node_modules/@dagrejs/dagre/package.json")).license, "MIT");
  assert.equal(Object.hasOwn(explorer.dependencies, "@xyflow/react-pro"), false);
});

test("toolchain seal binds package lock, six CI contexts, and browser revision", async () => {
  const seal = await readJSON("artifacts/preflight/T081/toolchain-seal.json");
  const lockBytes = await readFile(join(root, "package-lock.json"));
  const planBytes = await readFile(join(root, "tools/ci/check-plan.json"));
  const workflow = await readFile(join(root, ".github/workflows/bootstrap.yml"), "utf8");
  const checkPlan = JSON.parse(planBytes.toString("utf8"));
  const digest = (bytes) => createHash("sha256").update(bytes).digest("hex");
  const canonicalLockBytes = Buffer.from(lockBytes.toString("utf8").replace(/\r\n/g, "\n"), "utf8");

  assert.equal(seal.status, "SEALED_FOR_CONTRACT_TESTS");
  assert.equal(seal.integrity.package_lock_sha256, digest(canonicalLockBytes));
  assert.equal(seal.integrity.check_plan_sha256, digest(planBytes));
  assert.deepEqual(checkPlan.checks.map((check) => check.context), seal.ci.check_contexts);
  assert.ok(workflow.includes(`actions/setup-node@${seal.ci.node_action_commit}`));
  assert.ok(workflow.includes(`node-version: ${seal.runtime.node}`));
  assert.ok(workflow.includes(seal.ci.install_command));
  assert.ok(workflow.includes(seal.ci.contract_command));
  assert.equal(seal.license_metadata_observation.project_license, "NOASSERTION");
  assert.equal(seal.license_metadata_observation.distribution_approval, "NOT_ASSERTED");
});

test("TypeScript bootstrap keeps strict nullable and indexed access checks", async () => {
  const config = await readJSON("web/explorer/tsconfig.json");
  assert.equal(config.compilerOptions.strict, true);
  assert.equal(config.compilerOptions.noUncheckedIndexedAccess, true);
  assert.equal(config.compilerOptions.exactOptionalPropertyTypes, true);
  assert.equal(config.compilerOptions.moduleResolution, "Bundler");
  assert.equal(config.compilerOptions.jsx, "react-jsx");
});

test("Explorer API and bridge reuse the closed S43 contracts", async () => {
  const operations = await readJSON("contracts/operations.v1.json");
  const schema = await readJSON("contracts/mce4d.v1.schema.json");
  const defs = schema.$defs;
  const bridgeTypes = new Set(defs.BridgeMessage.oneOf.map((entry) => entry.$ref.split("/").at(-1)));

  assert.deepEqual(operations["POST /preview/status"], ["PreviewStatusRequest", "PreviewStatusData"]);
  assert.ok(operations["POST /preview/attach"]);
  assert.ok(operations["POST /preview/bind"]);
  assert.ok(bridgeTypes.has("Bridge_CHALLENGE"));
  assert.ok(bridgeTypes.has("Bridge_READY"));
  assert.ok(bridgeTypes.has("Bridge_BOUND_ACK"));
  assert.ok(bridgeTypes.has("Bridge_MODE_ACK"));
  assert.ok(bridgeTypes.has("Bridge_PING"));
  assert.ok(bridgeTypes.has("Bridge_PONG"));
  assert.deepEqual(Object.keys(defs.DOMAnchorRef.properties).sort(), ["anchor_id", "build_map_id"]);
  assert.equal(defs.DOMAnchorRef.additionalProperties, false);
  assert.equal(defs.BuildManifest.additionalProperties, false);
  assert.deepEqual(Object.keys(defs.PreviewStatusRequest.properties), ["session_id"]);
  assert.deepEqual(Object.keys(defs.PreviewStatusData.properties).sort(), [
    "authorization_epoch",
    "binding_id",
    "document_epoch",
    "expires_at",
    "hmr_epoch",
    "interaction_allowed",
    "mode_state",
    "operation",
    "permission_profile_id",
    "permission_status",
    "permission_version",
    "revision",
    "server_time",
    "session_id",
    "state",
  ]);
  assert.deepEqual(Object.keys(defs.PreviewBindRequest.properties).sort(), [
    "build_map_id",
    "capabilities",
    "document_epoch",
    "expected_revision",
    "hmr_epoch",
    "pairing_nonce",
    "session_id",
  ]);
  assert.deepEqual(Object.keys(defs.PreviewAttachRequest.properties).sort(), [
    "build_map_id",
    "origin",
    "path",
    "permission_profile_id",
    "scope",
  ]);
  assert.deepEqual(Object.keys(defs.PreviewPermissionAdminRequest.properties).sort(), [
    "action",
    "cursor",
    "expected_version",
    "limit",
    "profile",
    "profile_id",
    "reason",
  ]);
  assert.deepEqual(Object.keys(defs.PreviewPermissionSummary.properties).sort(), [
    "declared_status",
    "digest",
    "effective_status",
    "profile",
    "profile_id",
    "version",
  ]);
  assert.equal(defs.PreviewBindRequest.additionalProperties, false);
  assert.equal(defs.PreviewAttachRequest.additionalProperties, false);
  assert.equal(defs.PreviewPermissionAdminRequest.additionalProperties, false);
  assert.equal(defs.PreviewPermissionSummary.additionalProperties, false);
  assert.equal(defs.PreviewStatusRequest.additionalProperties, false);
  assert.equal("generation" in defs.BuildManifest.properties, false);
  assert.equal("actor_id" in defs.PreviewAttachRequest.properties, false);
  assert.equal(defs.PreviewStatusData.additionalProperties, false);
});

test("pinned React and graph tools expose the intended public APIs", () => {
  assert.equal(react.version, "19.3.1");
  assert.equal(reactDOM.version, "19.3.1");
  assert.ok(ReactFlow);
  assert.equal(typeof graphlib.Graph, "function");
  assert.equal(typeof layout, "function");

  const graph = new graphlib.Graph({ directed: true });
  graph.setGraph({ rankdir: "LR" });
  graph.setDefaultEdgeLabel(() => ({}));
  graph.setNode("a", { width: 80, height: 32 });
  graph.setNode("b", { width: 80, height: 32 });
  graph.setEdge("a", "b");
  graph.setEdge("b", "a");
  layout(graph);
  assert.ok(graph.edge("a", "b"));
  assert.ok(graph.edge("b", "a"));
});

test("pinned compiler, component test, and browser tools expose expected entry points", async () => {
  assert.equal(ts.version, "7.0.2");
  assert.ok(new JSDOM("<!doctype html><html><body></body></html>").window.document.body);
  assert.equal(typeof render, "function");
  assert.equal(typeof createServer, "function");
  assert.equal(typeof reactPlugin, "function");
  assert.equal(typeof chromium.launch, "function");
  assert.ok(devices["Desktop Chrome"]);
  assert.match(chromium.executablePath(), /chromium(?:-headless-shell)?-1248/);
  const browserManifest = await readJSON("node_modules/playwright-core/browsers.json");
  for (const browserName of ["chromium", "chromium-headless-shell"]) {
    const browser = browserManifest.browsers.find((entry) => entry.name === browserName);
    assert.equal(browser.revision, "1248");
    assert.equal(browser.browserVersion, "156.0.8078.4");
  }
});

test("fixture plan distinguishes design coverage from future browser acceptance", async () => {
  const plan = await readJSON("artifacts/preflight/T081/explorer-fixture-plan.json");
  assert.equal(plan.status, "SEALED_TEST_PLAN_NOT_PRODUCT_EXECUTION");
  assert.equal(plan.product_runtime_claim, "NOT_RUN");
  assert.equal(plan.target_execution, "NOT_IN_T081_SCOPE");
  assert.equal(plan.resource_budget_policy.fixed_byte_limits, null);
  assert.deepEqual(
    plan.fixtures.map((fixture) => fixture.case_id),
    ["C081", "C082", "C123", "C124", "C125", "C126", "C140", "C141", "C142", "C152", "C153", "C174", "C175", "C176", "C180"],
  );
  assert.ok(plan.fixtures.every((fixture) =>
    fixture.expected_oracle && fixture.owner_task === "T081" && fixture.execution_status === "SEALED_NOT_EXECUTED",
  ));
});

test("Node producer matches the shared C14N1, manifest JSON, and raw source vectors", async () => {
  const vectors = await readJSON("tests/contract/c14n-vectors.v1.json");
  for (const vector of vectors.vectors) {
    assert.equal(canonicalEncode(vector.value).toString("hex"), vector.encoding_hex, vector.name);
    assert.equal(hashValue(vector.domain, vector.value), vector.digest, vector.name);
  }
  for (const vector of vectors.manifest_json_vectors) {
    const bytes = Buffer.from(canonicalJSON1(vector.value), "utf8");
    assert.equal(bytes.toString("hex"), vector.canonical_hex, vector.name);
    assert.equal(hashRaw(bytes), vector.sha256, vector.name);
  }
  for (const vector of vectors.raw_source_vectors) {
    assert.equal(hashRaw(Buffer.from(vector.bytes_hex, "hex")), vector.sha256, vector.name);
  }
  assert.throws(() => canonicalEncode(1.5), /safe integers/);
  assert.throws(() => canonicalEncode(Number.MAX_SAFE_INTEGER + 1), /safe integers/);
  assert.throws(() => canonicalEncode("\ud800"), /lone UTF-16 surrogates/);
});

test("Vite inspector is explicit dev-only, emits source-byte anchors, and is stripped from production", async () => {
  await mkdir(join(root, "work"), { recursive: true });
  const fixtureRoot = await mkdtemp(join(root, "work", "t085-vite-fixture-"));
  const source = `export function App(props: { label: string }) {\n  const label = props.label || "đường dẫn";\n  const handleClick = () => console.log(label);\n  return <main><button onClick={handleClick}>{label}</button></main>;\n}\n`;
  const sourceBytes = Buffer.from(source, "utf8");
  const worktreeId = "86000000-0000-4000-8000-000000000001";
  const profileId = "9".repeat(64);
  let server;
  try {
    await mkdir(join(fixtureRoot, "src"), { recursive: true });
    await writeFile(join(fixtureRoot, "index.html"), '<div id="root"></div><script type="module" src="/src/main.tsx"></script>\n');
    await writeFile(join(fixtureRoot, "src", "App.tsx"), source);
    await writeFile(join(fixtureRoot, "src", "main.tsx"), 'import { App } from "./App"; console.log(App);\n');

    const inspector = createViteInspector({
      enabled: true,
      mode: "development",
      worktreeId,
      profileId,
      outputDirectory: ".mce4d/maps",
    });
    server = await createServer({
      configFile: false,
      root: fixtureRoot,
      mode: "development",
      appType: "custom",
      server: { middlewareMode: true, hmr: false },
      plugins: [inspector],
    });
    const transformed = await server.transformRequest("/src/App.tsx");
    assert.ok(transformed);
    assert.match(transformed.code, /data-mce4d-anchor/);
    assert.match(transformed.code, /onClick:\s*handleClick/);
    assert.ok(transformed.map);
    assert.deepEqual(transformed.map.sources, ["src/App.tsx"]);
    const resolved = server.config;
    const expectedBuildConfigDigest = hashValue("ui-build-config-v1", {
      root: ".",
      mode: resolved.mode,
      command: resolved.command,
      base: resolved.base,
      build: {
        target: resolved.build.target,
        outDir: resolved.build.outDir,
        minify: resolved.build.minify,
        cssCodeSplit: resolved.build.cssCodeSplit,
      },
    });
    const repeatedTransform = await inspector.transform(source, join(fixtureRoot, "src", "App.tsx"));
    assert.ok(repeatedTransform);

    const disabledInspector = createViteInspector({ enabled: false, mode: "development" });
    await disabledInspector.configResolved({ command: "serve", mode: "development" });
    assert.equal(await disabledInspector.transform(source, join(fixtureRoot, "src", "App.tsx")), null);
    await server.close();
    server = undefined;

    const mapDirectory = join(fixtureRoot, ".mce4d", "maps");
    const mapFiles = (await readdir(mapDirectory)).filter((name) => name.endsWith(".json"));
    assert.equal(mapFiles.length, 1);
    assert.equal((await readdir(mapDirectory)).filter((name) => name.endsWith(".tmp")).length, 0);
    const manifestBytes = await readFile(join(mapDirectory, mapFiles[0]));
    const manifest = JSON.parse(manifestBytes.toString("utf8"));
    assert.equal(manifestBytes.toString("utf8"), canonicalJSON1(manifest));
    assert.equal(manifest.mode, "development");
    assert.equal(manifest.module_coverage, "loaded_modules_only");
    assert.equal(Object.hasOwn(manifest, "generation_id"), false);
    assert.equal(manifest.sources.length, 1);
    assert.equal(manifest.sources[0].path, "src/App.tsx");
    assert.equal(manifest.sources[0].content_hash, hashRaw(sourceBytes));
    assert.equal(manifest.provider_digest, hashRaw(await readFile(join(root, "packages/vite-inspector/index.mjs"))));
    assert.equal(manifest.build_config_digest, expectedBuildConfigDigest);
    assert.equal(manifest.input_manifest_digest, hashValue("ui-input-manifest-v1", manifest.sources));
    const mapBody = { ...manifest };
    delete mapBody.build_map_id;
    assert.equal(manifest.build_map_id, hashValue("build-map-v1", mapBody));
    assert.ok(manifest.anchors.some((anchor) => anchor.kind === "component_definition"));
    assert.ok(manifest.anchors.some((anchor) => anchor.kind === "element_site"));
    assert.ok(manifest.anchors.some((anchor) => anchor.kind === "event_binding_site"));
    for (const anchor of manifest.anchors) {
      assert.ok(anchor.range.start_byte >= 0);
      assert.ok(anchor.range.end_byte > anchor.range.start_byte);
      assert.ok(anchor.range.end_byte <= sourceBytes.length);
      assert.equal(manifest.sources[0].content_hash, anchor.content_hash);
      const rangeBytes = sourceBytes.subarray(anchor.range.start_byte, anchor.range.end_byte);
      assert.ok(Buffer.from(rangeBytes.toString("utf8"), "utf8").equals(rangeBytes), "anchor range must end on UTF-8 boundaries");
      assert.equal(anchor.anchor_id, hashValue("anchor-v1", {
        path: anchor.path,
        content_hash: anchor.content_hash,
        kind: anchor.kind,
        start_byte: anchor.range.start_byte,
        end_byte: anchor.range.end_byte,
        provider_digest: manifest.provider_digest,
      }));
    }

    const entry = `import React from "react"; import { createRoot } from "react-dom/client"; function App(){ return <main><button>ok</button></main>; } createRoot(document.getElementById("root")).render(<App/>);`;
    await writeFile(join(fixtureRoot, "src", "main.tsx"), entry);
    const baselineOut = "dist-baseline";
    const inspectedOut = "dist-inspector-opt-in";
    const baseBuild = {
      configFile: false,
      root: fixtureRoot,
      mode: "development",
      plugins: [reactPlugin()],
      build: { outDir: baselineOut, emptyOutDir: true, sourcemap: false, minify: false },
    };
    await build(baseBuild);
    await build({
      ...baseBuild,
      plugins: [createViteInspector({
        enabled: true,
        mode: "development",
        worktreeId,
        profileId,
        outputDirectory: ".mce4d/maps",
      }), reactPlugin()],
      build: { ...baseBuild.build, outDir: inspectedOut },
    });
    const collectFiles = async (directory) => {
      const result = new Map();
      const visit = async (current) => {
        for (const entryName of await readdir(current, { withFileTypes: true })) {
          const fullPath = join(current, entryName.name);
          if (entryName.isDirectory()) await visit(fullPath);
          else result.set(fullPath.slice(directory.length + 1).replaceAll("\\", "/"), await readFile(fullPath));
        }
      };
      await visit(directory);
      return result;
    };
    const baseline = await collectFiles(join(fixtureRoot, baselineOut));
    const inspected = await collectFiles(join(fixtureRoot, inspectedOut));
    assert.deepEqual([...inspected.keys()].sort(), [...baseline.keys()].sort());
    for (const [name, bytes] of baseline) {
      assert.deepEqual(inspected.get(name), bytes, `production output differs with inspector enabled: ${name}`);
      const text = bytes.toString("utf8");
      assert.doesNotMatch(text, /data-mce4d-anchor|mce4d-vite-inspector|build_map_id/);
    }
  } finally {
    if (server) await server.close();
    await rm(fixtureRoot, { recursive: true, force: true });
  }
});

test("concurrent Vite transforms coalesce to a manifest containing every loaded module", async () => {
  await mkdir(join(root, "work"), { recursive: true });
  const fixtureRoot = await mkdtemp(join(root, "work", "t085-vite-burst-"));
  const modules = Array.from({ length: 48 }, (_, index) => {
    const name = `Component${String(index).padStart(2, "0")}`;
    return {
      path: `src/${name}.tsx`,
      source: `export function ${name}() { return <button>{${index}}</button>; }\n`,
    };
  });
  const inspector = createViteInspector({
    enabled: true,
    mode: "development",
    worktreeId: "86000000-0000-4000-8000-000000000002",
    profileId: "8".repeat(64),
    outputDirectory: ".mce4d/maps",
  });
  try {
    await mkdir(join(fixtureRoot, "src"), { recursive: true });
    await Promise.all(modules.map(({ path, source }) => writeFile(join(fixtureRoot, path), source)));
    await inspector.configResolved({
      command: "serve",
      mode: "development",
      root: fixtureRoot,
      base: "/",
      build: { target: "es2022", outDir: "dist", minify: false, cssCodeSplit: true },
    });
    const transformed = await Promise.all(modules.map(({ path, source }) => inspector.transform(source, join(fixtureRoot, path))));
    assert.ok(transformed.every(Boolean));
    await inspector.closeBundle();

    const mapDirectory = join(fixtureRoot, ".mce4d", "maps");
    const entries = await readdir(mapDirectory);
    const manifestFiles = entries.filter((name) => name.endsWith(".json"));
    assert.ok(manifestFiles.length > 0);
    assert.ok(manifestFiles.length <= modules.length, "manifest writes stay bounded by accepted transforms");
    assert.equal(entries.filter((name) => name.endsWith(".tmp")).length, 0);
    const manifests = await Promise.all(manifestFiles.map(async (name) => JSON.parse(await readFile(join(mapDirectory, name), "utf8"))));
    const complete = manifests.find((manifest) => manifest.sources.length === modules.length);
    assert.ok(complete, "the last immutable manifest contains every concurrently loaded module");
    assert.deepEqual(complete.sources.map(({ path }) => path), modules.map(({ path }) => path).sort());
  } finally {
    await inspector.closeBundle().catch(() => {});
    await rm(fixtureRoot, { recursive: true, force: true });
  }
});

test("Vite inspector rejects symlinked output parents before creating external directories", async (t) => {
  await mkdir(join(root, "work"), { recursive: true });
  const fixtureRoot = await mkdtemp(join(root, "work", "t085-vite-output-containment-"));
  const projectRoot = join(fixtureRoot, "project");
  const externalRoot = join(fixtureRoot, "external");
  try {
    await mkdir(projectRoot);
    await mkdir(externalRoot);
    try {
      await symlink(externalRoot, join(projectRoot, ".mce4d"), "junction");
    } catch (error) {
      if (process.platform === "win32" && ["EPERM", "EACCES", "ENOTSUP"].includes(error?.code)) {
        t.skip(`directory symlink creation is unavailable: ${error.code}`);
        return;
      }
      throw error;
    }

    const inspector = createViteInspector({
      enabled: true,
      mode: "development",
      worktreeId: "86000000-0000-4000-8000-000000000003",
      profileId: "7".repeat(64),
      outputDirectory: ".mce4d/maps",
    });
    await assert.rejects(inspector.configResolved({
      command: "serve",
      mode: "development",
      root: projectRoot,
      base: "/",
      build: { target: "es2022", outDir: "dist", minify: false, cssCodeSplit: true },
    }), /symlink|escaped/u);
    assert.deepEqual(await readdir(externalRoot), []);
  } finally {
    await rm(fixtureRoot, { recursive: true, force: true });
  }
});

test("Explorer AppShell typechecks, builds, and passes its component and API tests", () => {
  const explorer = join(root, "web/explorer");
  const run = (script, args) => execFileSync(process.execPath, [join(root, script), ...args], {
    cwd: explorer,
    stdio: "inherit",
  });

  run("node_modules/typescript/bin/tsc", ["--noEmit", "--project", "tsconfig.json"]);
  run("node_modules/vite/bin/vite.js", ["build"]);
  run("node_modules/vitest/vitest.mjs", ["run"]);
});
