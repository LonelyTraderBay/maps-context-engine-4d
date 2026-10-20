import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import test from "node:test";

import * as react from "react";
import * as reactDOM from "react-dom";
import { ReactFlow } from "@xyflow/react";
import { graphlib, layout } from "@dagrejs/dagre";
import { createServer } from "vite";
import reactPlugin from "@vitejs/plugin-react";
import ts from "typescript";
import { JSDOM } from "jsdom";
import { render } from "@testing-library/react";
import { chromium, devices } from "@playwright/test";

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
