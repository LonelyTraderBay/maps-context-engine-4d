import * as typescript from "@typescript/typescript6";

const EXPECTED_COMPILER_VERSION = "6.0.3";

if (typescript.version !== EXPECTED_COMPILER_VERSION) {
  throw new Error(
    `TypeScript compatibility API version mismatch: expected ${EXPECTED_COMPILER_VERSION}, got ${typescript.version ?? "unknown"}`,
  );
}

export const providerDescriptor = Object.freeze({
  schemaVersion: 1,
  language: "typescript_javascript",
  compilerPackage: "@typescript/typescript6",
  compilerPackageVersion: "6.0.2",
  compilerVersion: typescript.version,
  languages: Object.freeze(["typescript", "javascript", "tsx", "jsx"]),
  defaultAnalysisMode: "syntax",
  optionalAnalysisMode: "typed_when_available",
  allowProjectPlugins: false,
  configRoots: "owned_captured_roots_only",
  targetScripts: "never_execute",
  targetBuilds: "never_start",
});

// T043 chỉ phát hiện API đã pin. T044 phải đặt compiler sau lớp đọc config
// và capture-root; consumer không được chuyển thẳng config mục tiêu vào API này.
export { typescript };
