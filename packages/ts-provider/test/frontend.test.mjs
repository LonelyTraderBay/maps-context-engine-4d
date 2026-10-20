import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import {
  analyzeFrontendRequest,
  frontendCapabilities,
  languageProviderDescriptor,
} from "../src/index.mjs";

const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const requestId = "123e4567-e89b-12d3-a456-426614174000";
const profileId = "c".repeat(64);

function capturedFile(path, text) {
  const bytes = Buffer.from(text, "utf8");
  return {
    path,
    content_base64: bytes.toString("base64"),
    content_sha256: createHash("sha256").update(bytes).digest("hex"),
  };
}

function makeRequest(sources) {
  const files = Object.entries(sources).map(([path, text]) => capturedFile(path, text));
  files.push(capturedFile("tsconfig.json", JSON.stringify({ compilerOptions: { jsx: "preserve" } })));
  files.sort((left, right) => Buffer.compare(Buffer.from(left.path), Buffer.from(right.path)));
  return {
    schema_version: 1,
    message_type: "analyze_request",
    request_id: requestId,
    language: "tsx",
    profile: {
      schema_id: "typescript-profile",
      schema_version: 1,
      profile_id: profileId,
      config: {
        schema_version: 1,
        language: "typescript_javascript",
        tsconfig_paths: ["tsconfig.json"],
        compiler_digest: "a4a28b171436f8794f04854d885a222914a275da85751cf7e3a1f06bb30f6ea5",
        node_digest: "f9683a19877e12a9b167305c8db71040d812876359de21f0d2e97c3f321f80c3",
        provider_digest: languageProviderDescriptor.digest,
        allow_project_plugins: false,
        analysis_mode: "syntax",
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

test("React Router TSX facts retain service namespace and UTF-8 source ranges", () => {
  const source = [
    'import React from "react";',
    'import { Route as AppRoute } from "react-router-dom";',
    "export function UsersPage() { return <main>Users 😀</main>; }",
    'export const App = () => <AppRoute path="/users" element={<UsersPage />} />;',
    "export async function loadUsers() { // 😀\n  return fetch(\"/api/users\", { method: \"get\" });\n}",
  ].join("\n");
  const result = analyzeFrontendRequest(makeRequest({ "src/app.tsx": source }), { service_id: "storefront" });

  assert.equal(result.schema_version, 1);
  assert.equal(result.message_type, "frontend_analysis_response");
  assert.equal(result.service_id, "storefront");
  assert.deepEqual(result.capabilities, frontendCapabilities);
  assert.deepEqual(result.facts.map((fact) => fact.kind), ["component", "component", "route", "api_client"]);

  const component = result.facts.find((fact) => fact.kind === "component" && fact.name === "UsersPage");
  assert.ok(result.facts.some((fact) => fact.kind === "component" && fact.name === "App"));
  const route = result.facts.find((fact) => fact.kind === "route");
  const client = result.facts.find((fact) => fact.kind === "api_client");
  assert.equal(component.name, "UsersPage");
  assert.equal(route.path, "/users");
  assert.equal(route.component_name, "UsersPage");
  assert.equal(route.component_id, component.id);
  assert.equal(client.path, "/api/users");
  assert.equal(client.method, "GET");
  assert.equal(client.analysis_status, "exact_in_supported_scope");

  const routeStart = source.indexOf("<AppRoute");
  const clientStart = source.indexOf("fetch(");
  assert.equal(route.source.range.start_byte, Buffer.byteLength(source.slice(0, routeStart)));
  assert.equal(client.source.range.start_byte, Buffer.byteLength(source.slice(0, clientStart)));
  assert.equal(client.source.content_hash,
    createHash("sha256").update(Buffer.from(source, "utf8")).digest("hex"));
});

test("dynamic paths, generated clients, and absent namespace remain gaps", () => {
  const dynamicPath = "const load = (id) => fetch(`" + "/api/${id}" + "`);";
  const source = [
    'import { getUser } from "./generated/client.js";',
    dynamicPath,
    "getUser();",
    'fetch("/api/static");',
  ].join("\n");
  const result = analyzeFrontendRequest(makeRequest({ "src/api.ts": source }));

  assert.equal(result.service_id, null);
  assert.equal(result.facts.length, 1);
  assert.equal(result.facts[0].analysis_status, "candidate");
  assert.equal(result.facts[0].service_id, null);
  assert.deepEqual(new Set(result.diagnostics.map((item) => item.code)), new Set([
    "frontend-service-namespace-missing",
    "frontend-client-dynamic-path",
    "frontend-generated-client-unsupported",
  ]));
});

test("unsupported framework imports do not become React route facts", () => {
  const source = [
    'import React from "react";',
    'import { Route } from "vue-router";',
    'export const App = () => <Route path="/users" />;',
  ].join("\n");
  const result = analyzeFrontendRequest(makeRequest({ "src/app.tsx": source }), { service_id: "storefront" });

  assert.equal(result.facts.length, 0);
  assert.ok(result.diagnostics.some((item) => item.code === "frontend-framework-unsupported"));
});

test("shadowed fetch is not treated as a native API client", () => {
  const source = [
    'import { fetch } from "./transport.js";',
    'fetch("/api/users");',
  ].join("\n");
  const result = analyzeFrontendRequest(makeRequest({ "src/api.ts": source }), { service_id: "storefront" });

  assert.equal(result.facts.length, 0);
  assert.ok(result.diagnostics.some((item) => item.code === "frontend-client-fetch-shadowed"));
});

test("dynamic fetch method overrides are retained as gaps", () => {
  const source = 'fetch("/api/users", { method });\nfetch("/api/admin", { ["method"]: "POST" });';
  const result = analyzeFrontendRequest(makeRequest({ "src/api.ts": source }), { service_id: "storefront" });

  assert.equal(result.facts.length, 0);
  assert.equal(result.diagnostics.filter((item) => item.code === "frontend-client-dynamic-method").length, 2);
});

test("frontend service namespace rejects invalid values", () => {
  assert.throws(() => analyzeFrontendRequest(makeRequest({ "src/api.ts": "fetch('/api');" }), { service_id: 42 }),
    /service_id/);
});

test("frontend facts stop at the deterministic fact budget", () => {
  const source = Array.from({ length: 10_001 }, () => 'fetch("/api/items");').join("\n");
  const result = analyzeFrontendRequest(makeRequest({ "src/api.ts": source }), { service_id: "storefront" });

  assert.equal(result.facts.length, 10_000);
  assert.equal(result.truncated, true);
  assert.ok(result.diagnostics.some((item) => item.code === "frontend-fact-limit"));
});

test("owned provider profile digest matches the source manifest", async () => {
  const profile = JSON.parse(await readFile(resolve(packageRoot, "fixtures/profile.json"), "utf8"));
  assert.equal(profile.provider_digest, languageProviderDescriptor.digest);
});
