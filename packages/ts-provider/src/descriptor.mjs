import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function providerDigest() {
  const sourceDirectory = dirname(fileURLToPath(import.meta.url));
  const packageDirectory = resolve(sourceDirectory, "..");
  const sourcePaths = readdirSync(sourceDirectory)
    .filter((name) => name.endsWith(".mjs"))
    .map((name) => "src/" + name);
  const paths = ["package-lock.json", "package.json", ...sourcePaths].sort(compareUtf8);
  const manifest = paths.map((relativePath) => {
    const bytes = readFileSync(resolve(packageDirectory, relativePath));
    return {
      byte_length: bytes.length,
      path: relativePath,
      sha256: createHash("sha256").update(bytes).digest("hex"),
    };
  });
  return createHash("sha256").update(JSON.stringify(manifest), "utf8").digest("hex");
}

export const languageProviderDescriptor = Object.freeze({
  id: "typescript-v6",
  version: "1.0.0",
  license: "Apache-2.0",
  digest: providerDigest(),
  languages: Object.freeze(["javascript", "jsx", "tsx", "typescript"]),
  capabilities: Object.freeze([
    Object.freeze({ id: "diagnostics.gaps", version: "1.0.0" }),
    Object.freeze({ id: "relations.imports", version: "1.0.0" }),
    Object.freeze({ id: "relations.references", version: "1.0.0" }),
    Object.freeze({ id: "symbols.syntax", version: "1.0.0" }),
    Object.freeze({ id: "symbols.typed", version: "1.0.0" }),
  ]),
});
