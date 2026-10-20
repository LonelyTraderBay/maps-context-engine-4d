import * as typescript from "@typescript/typescript6";
import { languageProviderDescriptor } from "./descriptor.mjs";

const EXPECTED_COMPILER_VERSION = "6.0.3";

if (typescript.version !== EXPECTED_COMPILER_VERSION) {
  throw new Error(
    "TypeScript compatibility API version mismatch: expected " +
      EXPECTED_COMPILER_VERSION +
      ", got " +
      (typescript.version ?? "unknown"),
  );
}

export const providerDescriptor = Object.freeze({
  schemaVersion: 1,
  language: "typescript_javascript",
  compilerPackage: "@typescript/typescript6",
  compilerPackageVersion: "6.0.2",
  compilerVersion: typescript.version,
  providerDigest: languageProviderDescriptor.digest,
  languages: languageProviderDescriptor.languages,
  capabilities: languageProviderDescriptor.capabilities,
  defaultAnalysisMode: "syntax",
  optionalAnalysisMode: "typed_when_available",
  allowProjectPlugins: false,
  configRoots: "owned_captured_roots_only",
  targetScripts: "never_execute",
  targetBuilds: "never_start",
});

export { languageProviderDescriptor, typescript };
export { analyzeProviderRequest } from "./analyze.mjs";
export { runProviderCLI } from "./protocol.mjs";
