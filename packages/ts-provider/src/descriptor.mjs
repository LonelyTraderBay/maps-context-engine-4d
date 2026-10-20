import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

function compareUtf8(left, right) {
  return Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
}

function canonicalTextBytes(bytes) {
  const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  return Buffer.from(text.replace(/\r\n?/g, "\n"), "utf8");
}

export function computeProviderManifestDigest(files) {
  if (!Array.isArray(files)) throw new TypeError("provider manifest files must be an array");

  const manifest = files
    .map((file) => {
      if (!file || typeof file.path !== "string" || !(file.bytes instanceof Uint8Array)) {
        throw new TypeError("provider manifest entry must contain a path and byte array");
      }
      const bytes = canonicalTextBytes(file.bytes);
      return {
        byte_length: bytes.length,
        path: file.path,
        sha256: createHash("sha256").update(bytes).digest("hex"),
      };
    })
    .sort((left, right) => compareUtf8(left.path, right.path));

  return createHash("sha256").update(JSON.stringify(manifest), "utf8").digest("hex");
}

function providerDigest() {
  const sourceDirectory = dirname(fileURLToPath(import.meta.url));
  const packageDirectory = resolve(sourceDirectory, "..");
  const sourcePaths = readdirSync(sourceDirectory)
    .filter((name) => name.endsWith(".mjs"))
    .map((name) => "src/" + name);
  const paths = ["package-lock.json", "package.json", ...sourcePaths].sort(compareUtf8);
  const files = paths.map((path) => ({
    path,
    bytes: readFileSync(resolve(packageDirectory, path)),
  }));
  return computeProviderManifestDigest(files);
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
