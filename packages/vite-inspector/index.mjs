import { createHash } from "node:crypto";
import { lstat, link, mkdir, open, readFile, realpath, unlink } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

import { parseSync } from "vite";

const MAX_SOURCE_BYTES = 2 * 1024 * 1024;
const MAX_MANIFEST_BYTES = 64 * 1024 * 1024;
const MAX_ENTRIES = 100_000;
const PROVIDER_VERSION = "vite-inspector/1.0.0";
const HOST_ATTRIBUTE = "data-mce4d-anchor";

export function canonicalEncode(value, depth = 0) {
  if (depth > 32) throw new TypeError("MCE-C14N1 maximum nesting depth exceeded");
  if (value === null) return Buffer.from("n");
  if (value === false) return Buffer.from("f");
  if (value === true) return Buffer.from("t");
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) throw new TypeError("MCE-C14N1 accepts safe integers only");
    return Buffer.from(`i${Object.is(value, -0) ? "0" : value};`);
  }
  if (typeof value === "string") {
    if (!value.isWellFormed()) throw new TypeError("MCE-C14N1 rejects lone UTF-16 surrogates");
    const bytes = Buffer.from(value, "utf8");
    return Buffer.concat([Buffer.from(`s${bytes.length}:`), bytes]);
  }
  if (Array.isArray(value)) {
    if (value.length > 100_000) throw new TypeError("MCE-C14N1 array limit exceeded");
    return Buffer.concat([
      Buffer.from(`a${value.length}:`),
      ...value.map((entry) => canonicalEncode(entry, depth + 1)),
    ]);
  }
  if (typeof value === "object") {
    const entries = Object.entries(value);
    if (entries.length > 4096) throw new TypeError("MCE-C14N1 object key limit exceeded");
    entries.sort(([left], [right]) => Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8")));
    return Buffer.concat([
      Buffer.from(`o${entries.length}:`),
      ...entries.flatMap(([key, entry]) => [canonicalEncode(key, depth + 1), canonicalEncode(entry, depth + 1)]),
    ]);
  }
  throw new TypeError(`MCE-C14N1 unsupported value type: ${typeof value}`);
}

export function hashValue(domain, value) {
  return createHash("sha256")
    .update(Buffer.from("MCE4D\0C14N1\0", "binary"))
    .update(canonicalEncode(domain))
    .update(canonicalEncode(value))
    .digest("hex");
}

export function canonicalJSON1(value, depth = 0) {
  if (depth > 32) throw new TypeError("MCE-MANIFEST-JSON1 maximum nesting depth exceeded");
  if (value === null) return "null";
  if (value === true) return "true";
  if (value === false) return "false";
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) throw new TypeError("MCE-MANIFEST-JSON1 accepts safe integers only");
    return String(Object.is(value, -0) ? 0 : value);
  }
  if (typeof value === "string") {
    if (!value.isWellFormed()) throw new TypeError("MCE-MANIFEST-JSON1 rejects lone UTF-16 surrogates");
    return JSON.stringify(value).replaceAll(" ", "\\u2028").replaceAll(" ", "\\u2029");
  }
  if (Array.isArray(value)) {
    if (value.length > 100_000) throw new TypeError("MCE-MANIFEST-JSON1 array limit exceeded");
    return `[${value.map((entry) => canonicalJSON1(entry, depth + 1)).join(",")}]`;
  }
  if (typeof value === "object") {
    const entries = Object.entries(value);
    if (entries.length > 4096) throw new TypeError("MCE-MANIFEST-JSON1 object key limit exceeded");
    entries.sort(([left], [right]) => Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8")));
    return `{${entries.map(([key, entry]) => `${canonicalJSON1(key, depth + 1)}:${canonicalJSON1(entry, depth + 1)}`).join(",")}}`;
  }
  throw new TypeError(`MCE-MANIFEST-JSON1 unsupported value type: ${typeof value}`);
}

export function hashRaw(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function isPascalCase(name) {
  return /^[A-Z]/u.test(name);
}

function forEachASTChild(node, visit) {
  for (const [key, value] of Object.entries(node)) {
    if (key === "parent" || key === "loc" || key === "range" || key.startsWith("_")) continue;
    if (Array.isArray(value)) {
      for (const child of value) {
        if (child && typeof child === "object" && typeof child.type === "string") visit(child);
      }
    } else if (value && typeof value === "object" && typeof value.type === "string") {
      visit(value);
    }
  }
}

function hasJSX(node) {
  let found = false;
  const visit = (current) => {
    if (found) return;
    if (current.type === "JSXElement" || current.type === "JSXFragment") {
      found = true;
      return;
    }
    forEachASTChild(current, visit);
  };
  visit(node);
  return found;
}

function componentDefinitions(program) {
  const found = new Set();
  const addIfComponent = (node, name) => {
    if (name && isPascalCase(name) && hasJSX(node)) {
      if (found.size >= MAX_ENTRIES) throw new Error(`MCE4D inspector component count exceeds ${MAX_ENTRIES}`);
      found.add(node);
    }
  };
  const visit = (node) => {
    if (node.type === "FunctionDeclaration" || node.type === "ClassDeclaration") {
      addIfComponent(node, node.id?.name);
    } else if (node.type === "VariableDeclaration") {
      for (const declaration of node.declarations) {
        const initializer = declaration.init;
        if (declaration.id?.type === "Identifier" && initializer &&
            (initializer.type === "ArrowFunctionExpression" || initializer.type === "FunctionExpression")) {
          addIfComponent(declaration, declaration.id.name);
        }
      }
    }
    forEachASTChild(node, visit);
  };
  visit(program);
  return found;
}

function nativeHost(tagName) {
  return (tagName?.type === "JSXIdentifier" && /^[a-z]/u.test(tagName.name)) || tagName?.type === "JSXNamespacedName";
}

function byteOffsets(source) {
  const offsets = new Uint32Array(source.length + 1);
  let index = 0;
  let byte = 0;
  while (index < source.length) {
    const point = source.codePointAt(index);
    const text = String.fromCodePoint(point);
    const units = point > 0xffff ? 2 : 1;
    offsets[index] = byte;
    if (units === 2) offsets[index + 1] = byte;
    byte += Buffer.byteLength(text, "utf8");
    index += units;
    offsets[index] = byte;
  }
  return offsets;
}

function relativeSourcePath(root, filename) {
  const relative = path.relative(root, filename).split(path.sep).join("/");
  if (!relative || relative === ".." || relative.startsWith("../") || path.posix.isAbsolute(relative) || relative.includes("\\")) {
    return null;
  }
  if (relative.split("/").some((segment) => segment === "" || segment === "." || segment === "..")) return null;
  return relative;
}

function relativeOutputPath(root, candidate) {
  const relative = path.relative(root, candidate);
  if (!relative || relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
    throw new Error("MCE4D manifest output directory must be a child of the Vite root");
  }
  return relative;
}

async function validateOutputPath(rootReal, outputDirectory) {
  const relative = relativeOutputPath(rootReal, outputDirectory);
  let current = rootReal;
  for (const segment of relative.split(path.sep)) {
    current = path.join(current, segment);
    let info;
    try {
      info = await lstat(current);
    } catch (error) {
      if (error?.code === "ENOENT") return;
      throw error;
    }
    if (info.isSymbolicLink() || !info.isDirectory()) {
      throw new Error("MCE4D manifest output path contains a symlink or non-directory component");
    }
    const actual = await realpath(current);
    const actualRelative = path.relative(rootReal, actual);
    if (actualRelative === ".." || actualRelative.startsWith(`..${path.sep}`) || path.isAbsolute(actualRelative)) {
      throw new Error("MCE4D manifest output directory escaped the Vite root");
    }
  }
}

async function ensureOutputDirectory(rootReal, outputDirectory) {
  const relative = relativeOutputPath(rootReal, outputDirectory);
  let current = rootReal;
  for (const segment of relative.split(path.sep)) {
    current = path.join(current, segment);
    try {
      await mkdir(current);
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
    }
    await validateOutputPath(rootReal, current);
  }
  return realpath(outputDirectory);
}

function sourceMap(code, sourcePath, edits) {
  const lines = code.split("\n");
  const lineStarts = [0];
  for (let index = 0; index < code.length; index += 1) if (code.charCodeAt(index) === 10) lineStarts.push(index + 1);
  const lineSegments = lines.map(() => []);
  const editLines = new Map();
  for (const edit of edits) {
    let low = 0;
    let high = lineStarts.length;
    while (low + 1 < high) {
      const middle = (low + high) >> 1;
      if (lineStarts[middle] <= edit.position) low = middle;
      else high = middle;
    }
    const list = editLines.get(low) ?? [];
    list.push({ ...edit, originalColumn: edit.position - lineStarts[low] });
    editLines.set(low, list);
  }
  for (let line = 0; line < lines.length; line += 1) {
    const segments = lineSegments[line];
    segments.push({ generated: 0, originalLine: line, originalColumn: 0 });
    let added = 0;
    for (const edit of (editLines.get(line) ?? []).sort((a, b) => a.position - b.position)) {
      const before = edit.originalColumn + added;
      segments.push({ generated: before, originalLine: line, originalColumn: edit.originalColumn });
      added += edit.text.length;
      segments.push({ generated: before + edit.text.length, originalLine: line, originalColumn: edit.originalColumn });
    }
  }
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  const vlq = (input) => {
    let value = input < 0 ? ((-input) << 1) | 1 : input << 1;
    let output = "";
    do {
      let digit = value & 31;
      value >>>= 5;
      if (value > 0) digit |= 32;
      output += alphabet[digit];
    } while (value > 0);
    return output;
  };
  let previousSource = 0;
  let previousOriginalLine = 0;
  let previousOriginalColumn = 0;
  const mappings = lineSegments.map((segments) => {
    let previousGeneratedColumn = 0;
    return segments.map((segment) => {
      const encoded = [
        vlq(segment.generated - previousGeneratedColumn),
        vlq(0 - previousSource),
        vlq(segment.originalLine - previousOriginalLine),
        vlq(segment.originalColumn - previousOriginalColumn),
      ].join("");
      previousGeneratedColumn = segment.generated;
      previousSource = 0;
      previousOriginalLine = segment.originalLine;
      previousOriginalColumn = segment.originalColumn;
      return encoded;
    }).join(",");
  }).join(";");
  return { version: 3, file: sourcePath, sources: [sourcePath], sourcesContent: [code], names: [], mappings };
}

function insertions(sourceFile, source, pathName, sourceHash, providerDigest, ownerByNode) {
  const offsets = byteOffsets(source);
  const anchors = [];
  const edits = [];
  const anchorByNode = new Map();
  const add = (node, kind, ownerAnchorID = null) => {
    if (anchors.length >= MAX_ENTRIES) throw new Error(`MCE4D inspector anchor count exceeds ${MAX_ENTRIES}: ${pathName}`);
    const start = node.start;
    const end = node.end;
    if (start < 0 || end < start || end > source.length) throw new Error(`invalid AST source range in ${pathName}`);
    const range = { start_byte: offsets[start], end_byte: offsets[end] };
    const value = {
      path: pathName,
      content_hash: sourceHash,
      kind,
      start_byte: range.start_byte,
      end_byte: range.end_byte,
      provider_digest: providerDigest,
    };
    const anchorID = hashValue("anchor-v1", value);
    const entry = {
      anchor_id: anchorID,
      kind,
      path: pathName,
      content_hash: sourceHash,
      range,
      owner_anchor_id: ownerAnchorID,
    };
    anchors.push(entry);
    anchorByNode.set(node, anchorID);
    return anchorID;
  };

  for (const definition of ownerByNode) {
    const name = definition.id?.name;
    if (!name) continue;
    add(definition, "component_definition", null);
  }

  const visit = (node, activeOwner = null) => {
    let nextOwner = activeOwner;
    if (ownerByNode.has(node)) {
      nextOwner = anchorByNode.get(node) ?? null;
    }
    if (node.type === "JSXOpeningElement") {
      const kind = nativeHost(node.name) ? "element_site" : "component_render_site";
      const anchorID = add(node, kind, nextOwner);
      if (kind === "element_site") {
        if (node.attributes.some((attribute) => attribute.type === "JSXAttribute" &&
            attribute.name?.type === "JSXIdentifier" && attribute.name.name === HOST_ATTRIBUTE)) {
          throw new Error(`source already declares ${HOST_ATTRIBUTE}: ${pathName}`);
        }
        const start = node.start;
        const end = node.end;
        const nodeText = source.slice(start, end);
        const close = nodeText.lastIndexOf(">");
        if (close < 0) throw new Error(`cannot locate JSX closing delimiter in ${pathName}`);
        const insertionPoint = node.selfClosing && nodeText[close - 1] === "/" ? start + close - 1 : start + close;
        edits.push({ position: insertionPoint, text: ` ${HOST_ATTRIBUTE}="${anchorID}"` });
      }
    }
    if (node.type === "JSXAttribute" && node.name?.type === "JSXIdentifier" && /^on[A-Z]/u.test(node.name.name)) {
      add(node, "event_binding_site", nextOwner);
    }
    forEachASTChild(node, (child) => visit(child, nextOwner));
  };
  visit(sourceFile);
  return { anchors, edits };
}

function normalizedManifestBody({ worktreeId, profileId, providerDigest, buildConfigDigest, mode, modules }) {
  const sources = [...modules.entries()].map(([pathName, module]) => ({ path: pathName, content_hash: module.contentHash }));
  sources.sort((left, right) => Buffer.compare(Buffer.from(left.path), Buffer.from(right.path)));
  const anchors = [...modules.values()].flatMap((module) => module.anchors);
  anchors.sort((left, right) => {
    const pathOrder = Buffer.compare(Buffer.from(left.path), Buffer.from(right.path));
    if (pathOrder) return pathOrder;
    if (left.range.start_byte !== right.range.start_byte) return left.range.start_byte - right.range.start_byte;
    if (left.range.end_byte !== right.range.end_byte) return left.range.end_byte - right.range.end_byte;
    if (left.kind !== right.kind) return left.kind < right.kind ? -1 : 1;
    return left.anchor_id < right.anchor_id ? -1 : left.anchor_id > right.anchor_id ? 1 : 0;
  });
  const inputManifestDigest = hashValue("ui-input-manifest-v1", sources);
  return {
    schema_version: 1,
    worktree_id: worktreeId,
    profile_id: profileId,
    provider_version: PROVIDER_VERSION,
    build_config_digest: buildConfigDigest,
    input_manifest_digest: inputManifestDigest,
    mode,
    module_coverage: "loaded_modules_only",
    sources,
    anchors,
    provider_digest: providerDigest,
  };
}

async function publishImmutable(root, outputDirectory, mapID, bytes) {
  const rootReal = await realpath(root);
  const outputReal = await ensureOutputDirectory(rootReal, outputDirectory);
  const finalPath = path.join(outputReal, `${mapID}.json`);
  const temporaryPath = `${finalPath}.tmp`;
  try {
    const existingInfo = await lstat(finalPath);
    if (!existingInfo.isFile() || existingInfo.isSymbolicLink()) throw new Error("existing BuildManifest path is not a regular file");
    const existing = await readFile(finalPath);
    if (!existing.equals(bytes)) throw new Error("BuildManifest ID already exists with different bytes");
    return;
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  const handle = await open(temporaryPath, "wx");
  let closed = false;
  try {
    await handle.writeFile(bytes);
    await handle.sync();
    await handle.close();
    closed = true;
    try {
      // A same-volume exclusive hard link publishes the flushed bytes atomically
      // and refuses to replace an existing immutable artifact.
      await link(temporaryPath, finalPath);
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
      const existing = await readFile(finalPath);
      if (!existing.equals(bytes)) throw new Error("BuildManifest ID already exists with different bytes");
    }
  } finally {
    if (!closed) await handle.close().catch(() => {});
    await unlink(temporaryPath).catch((error) => { if (error?.code !== "ENOENT") throw error; });
  }
}

export function createViteInspector(options = {}) {
  const enabled = options.enabled === true;
  const modules = new Map();
  let totalAnchorCount = 0;
  let active = false;
  let config;
  let root;
  let rootReal;
  let outputDirectory;
  let providerDigest;
  let buildConfigDigest;
  let manifestRevision = 0;
  let writeRunning = false;
  let writeTask = Promise.resolve();
  let watcher;
  let unlinkListener;

  const scheduleManifestWrite = () => {
    if (!active) return Promise.resolve();
    manifestRevision++;
    if (writeRunning) return writeTask;
    writeRunning = true;
    writeTask = (async () => {
      try {
        while (true) {
          const writingRevision = manifestRevision;
          const body = normalizedManifestBody({
            worktreeId: options.worktreeId,
            profileId: options.profileId,
            providerDigest,
            buildConfigDigest,
            mode: config.mode,
            modules,
          });
          const buildMapID = hashValue("build-map-v1", body);
          const manifest = { ...body, build_map_id: buildMapID };
          const bytes = Buffer.from(canonicalJSON1(manifest), "utf8");
          if (bytes.length > MAX_MANIFEST_BYTES) {
            throw new Error(`MCE4D BuildManifest exceeds ${MAX_MANIFEST_BYTES} bytes`);
          }
          await publishImmutable(rootReal, outputDirectory, buildMapID, bytes);
          // Clear the running state in the same synchronous turn as this check.
          // A transform arriving afterward starts a new writer instead of having
          // its dirty revision erased by a later promise finalizer.
          if (writingRevision === manifestRevision) {
            writeRunning = false;
            return;
          }
        }
      } catch (error) {
        writeRunning = false;
        throw error;
      }
    })();
    return writeTask;
  };

  return {
    name: "mce4d-vite-inspector",
    enforce: "pre",
    async configResolved(resolved) {
      config = resolved;
      active = enabled && resolved.command === "serve" && (resolved.mode === "development" || resolved.mode === "test") && options.mode === resolved.mode;
      if (!active) return;
      if (!options.worktreeId || !options.profileId || !options.outputDirectory) {
        throw new Error("MCE4D inspector requires explicit worktreeId, profileId, and outputDirectory when enabled");
      }
      if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/u.test(options.worktreeId)) {
        throw new Error("MCE4D inspector worktreeId must be a canonical UUID");
      }
      for (const [name, value] of [["profileId", options.profileId]]) {
        if (!/^[0-9a-f]{64}$/u.test(value)) throw new Error(`MCE4D inspector ${name} must be a lowercase SHA-256 digest`);
      }
      root = path.resolve(resolved.root);
      rootReal = await realpath(root);
      outputDirectory = path.resolve(rootReal, options.outputDirectory);
      await validateOutputPath(rootReal, outputDirectory);
      providerDigest = hashRaw(await readFile(fileURLToPath(import.meta.url)));
      const configMaterial = {
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
      };
      buildConfigDigest = hashValue("ui-build-config-v1", configMaterial);
    },
    async transform(code, id) {
      if (!active) return null;
      const cleanID = id.split("?")[0];
      if (!cleanID || cleanID.startsWith("\0") || !/\.(?:tsx|jsx)$/iu.test(cleanID)) return null;
      const filename = path.resolve(cleanID);
      let actualFilename;
      try {
        actualFilename = await realpath(filename);
      } catch (error) {
        if (error?.code === "ENOENT") return null;
        throw error;
      }
      const relativePath = relativeSourcePath(rootReal, actualFilename);
      if (!relativePath || relativePath.split("/").includes("node_modules") || relativePath.startsWith(".git/")) return null;
      if (!code.isWellFormed()) throw new Error(`MCE4D inspector rejects malformed UTF-16 source: ${relativePath}`);
      if (Buffer.byteLength(code, "utf8") > MAX_SOURCE_BYTES) throw new Error(`MCE4D inspector source exceeds ${MAX_SOURCE_BYTES} bytes: ${relativePath}`);
      const sourceInfo = await lstat(actualFilename);
      if (!sourceInfo.isFile() || sourceInfo.isSymbolicLink()) return null;
      if (sourceInfo.size > MAX_SOURCE_BYTES) throw new Error(`MCE4D inspector source exceeds ${MAX_SOURCE_BYTES} bytes: ${relativePath}`);
      const sourceBytes = await readFile(actualFilename);
      if (!sourceBytes.equals(Buffer.from(code, "utf8"))) {
        throw new Error(`MCE4D inspector cannot verify original source bytes before transformation: ${relativePath}`);
      }
      if (sourceBytes.length > MAX_SOURCE_BYTES) throw new Error(`MCE4D inspector source exceeds ${MAX_SOURCE_BYTES} bytes: ${relativePath}`);
      const parseResult = parseSync(relativePath, code, { lang: /\.tsx$/iu.test(filename) ? "tsx" : "jsx" });
      if (parseResult.errors.length) throw new Error(`MCE4D inspector could not parse JSX source: ${relativePath}`);
      const contentHash = hashRaw(sourceBytes);
      const definitions = componentDefinitions(parseResult.program);
      const result = insertions(parseResult.program, code, relativePath, contentHash, providerDigest, definitions);
      const next = code;
      const editsDescending = [...result.edits].sort((left, right) => right.position - left.position);
      let transformed = next;
      for (const edit of editsDescending) transformed = `${transformed.slice(0, edit.position)}${edit.text}${transformed.slice(edit.position)}`;
      const moduleAnchors = [];
      // The helper returns anchors before Vite transforms the source; the source map
      // maps all untouched text and insertion boundaries back to those original bytes.
      moduleAnchors.push(...result.anchors);
      const previous = modules.get(relativePath);
      const nextAnchorCount = totalAnchorCount - (previous?.anchors.length ?? 0) + moduleAnchors.length;
      if (nextAnchorCount > MAX_ENTRIES) throw new Error(`MCE4D BuildManifest anchor count exceeds ${MAX_ENTRIES}`);
      if (!previous && modules.size >= MAX_ENTRIES) throw new Error(`MCE4D BuildManifest module count exceeds ${MAX_ENTRIES}`);
      modules.set(relativePath, { contentHash, anchors: moduleAnchors });
      totalAnchorCount = nextAnchorCount;
      await scheduleManifestWrite();
      return { code: transformed, map: sourceMap(code, relativePath, result.edits) };
    },
    configureServer(server) {
      if (!active) return;
      watcher = server.watcher;
      unlinkListener = (filename) => {
        const relativePath = relativeSourcePath(root, filename);
        const removed = relativePath ? modules.get(relativePath) : undefined;
        if (removed && modules.delete(relativePath)) {
          totalAnchorCount -= removed.anchors.length;
          void scheduleManifestWrite();
        }
      };
      watcher.on("unlink", unlinkListener);
    },
    async closeBundle() {
      await writeTask;
      if (watcher && unlinkListener) watcher.off("unlink", unlinkListener);
    },
  };
}
