import { normalizeRepoPath } from "./captured-root.mjs";

const HASH_PATTERN = /^[0-9a-f]{64}$/;
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const REQUEST_KEYS = [
  "schema_version",
  "message_type",
  "request_id",
  "language",
  "profile",
  "files",
  "execution_policy",
];
const PROFILE_KEYS = ["schema_id", "schema_version", "profile_id", "config"];
const CONFIG_KEYS = [
  "schema_version",
  "language",
  "tsconfig_paths",
  "compiler_digest",
  "node_digest",
  "provider_digest",
  "allow_project_plugins",
  "analysis_mode",
];
const POLICY_KEYS = [
  "target_code",
  "project_scripts",
  "project_plugins",
  "notebook_cells",
  "network",
];
const LANGUAGES = new Set(["typescript", "javascript", "tsx", "jsx"]);

export const PINNED_COMPILER_DIGEST =
  "a4a28b171436f8794f04854d885a222914a275da85751cf7e3a1f06bb30f6ea5";
export const PINNED_NODE_DIGEST =
  "f9683a19877e12a9b167305c8db71040d812876359de21f0d2e97c3f321f80c3";

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function hasExactKeys(value, expected) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value);
  return actual.length === expected.length && expected.every((key) => Object.hasOwn(value, key));
}

export function validateProviderProfile(request, expectedProviderDigest) {
  if (!hasExactKeys(request, REQUEST_KEYS)) {
    throw new Error("provider request has missing or unknown fields");
  }
  if (
    request.schema_version !== 1 ||
    request.message_type !== "analyze_request" ||
    typeof request.request_id !== "string" ||
    !UUID_PATTERN.test(request.request_id)
  ) {
    throw new Error("provider request protocol identity is invalid");
  }
  if (!LANGUAGES.has(request.language)) {
    throw new Error("provider language is unsupported");
  }

  const profile = request.profile;
  if (
    !hasExactKeys(profile, PROFILE_KEYS) ||
    profile.schema_id !== "typescript-profile" ||
    profile.schema_version !== 1 ||
    typeof profile.profile_id !== "string" ||
    !HASH_PATTERN.test(profile.profile_id)
  ) {
    throw new Error("provider profile identity is invalid");
  }

  const config = profile.config;
  if (!hasExactKeys(config, CONFIG_KEYS)) {
    throw new Error("TypeScript profile config has missing or unknown fields");
  }
  if (
    config.schema_version !== 1 ||
    config.language !== "typescript_javascript" ||
    config.allow_project_plugins !== false ||
    !["syntax", "typed_when_available"].includes(config.analysis_mode) ||
    config.compiler_digest !== PINNED_COMPILER_DIGEST ||
    config.node_digest !== PINNED_NODE_DIGEST ||
    config.provider_digest !== expectedProviderDigest
  ) {
    throw new Error("TypeScript profile does not match the sealed provider toolchain");
  }
  if (process.versions.node !== "24.22.0") {
    throw new Error("provider Node.js runtime does not match the sealed version");
  }
  if (
    !Array.isArray(config.tsconfig_paths) ||
    config.tsconfig_paths.length === 0 ||
    config.tsconfig_paths.length > 64
  ) {
    throw new Error("TypeScript profile must name captured tsconfig files");
  }

  const paths = config.tsconfig_paths.map(normalizeRepoPath);
  const sorted = [...paths].sort(compareUtf8);
  if (new Set(paths).size !== paths.length || paths.some((value, index) => value !== sorted[index])) {
    throw new Error("TypeScript profile config paths must be unique and sorted");
  }

  if (!Array.isArray(request.files) || request.files.length === 0) {
    throw new Error("provider request has no captured files");
  }
  const policy = request.execution_policy;
  if (
    !hasExactKeys(policy, POLICY_KEYS) ||
    policy.target_code !== "never_execute" ||
    policy.project_scripts !== "never_execute" ||
    policy.project_plugins !== "never_execute" ||
    policy.notebook_cells !== "never_execute" ||
    policy.network !== false
  ) {
    throw new Error("provider execution policy is not closed");
  }

  return { profile, config, paths };
}
