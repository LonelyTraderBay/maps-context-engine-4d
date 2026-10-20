import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { TextDecoder } from "node:util";

const utf8 = new TextDecoder("utf-8", { fatal: true });
const MAX_FILES = 4096;
const MAX_SOURCE_BYTES = 10 * 1024 * 1024;
const MAX_PATH_BYTES = 1024 * 1024;

function validUtf8String(value) {
  return typeof value === "string" && Buffer.from(value, "utf8").toString("utf8") === value;
}

export function normalizeRepoPath(value) {
  if (
    !validUtf8String(value) ||
    value.length === 0 ||
    Buffer.byteLength(value, "utf8") > 4096 ||
    value.includes("\0") ||
    value.includes("\\") ||
    value.includes(":") ||
    value.startsWith("/") ||
    path.posix.normalize(value) !== value ||
    value.split("/").some((part) => part === "" || part === "." || part === "..")
  ) {
    throw new Error("invalid captured repository path");
  }
  return value;
}

export function captureFiles(input) {
  if (!Array.isArray(input) || input.length === 0 || input.length > MAX_FILES) {
    throw new Error("captured file count is outside the provider limit");
  }

  const files = new Map();
  const pathKeys = new Set();
  let sourceBytes = 0;
  let pathBytes = 0;

  for (const file of input) {
    if (
      !file ||
      typeof file !== "object" ||
      Array.isArray(file) ||
      Object.keys(file).length !== 3 ||
      !Object.hasOwn(file, "path") ||
      !Object.hasOwn(file, "content_base64") ||
      !Object.hasOwn(file, "content_sha256")
    ) {
      throw new Error("captured file has missing or unknown fields");
    }
    const repoPath = normalizeRepoPath(file.path);
    if (pathKeys.has(repoPath)) {
      throw new Error("duplicate captured repository path");
    }
    pathKeys.add(repoPath);
    pathBytes += Buffer.byteLength(repoPath, "utf8");
    if (pathBytes > MAX_PATH_BYTES) {
      throw new Error("captured path byte budget exceeded");
    }

    if (typeof file.content_base64 !== "string") {
      throw new Error("captured file content must be base64");
    }
    const bytes = Buffer.from(file.content_base64, "base64");
    if (bytes.toString("base64") !== file.content_base64) {
      throw new Error("captured file content has invalid base64 encoding");
    }
    const contentHash = createHash("sha256").update(bytes).digest("hex");
    if (file.content_sha256 !== contentHash) {
      throw new Error("captured file digest mismatch");
    }
    sourceBytes += bytes.length;
    if (sourceBytes > MAX_SOURCE_BYTES) {
      throw new Error("captured source byte budget exceeded");
    }

    let text;
    try {
      text = utf8.decode(bytes);
    } catch {
      throw new Error("captured source is not valid UTF-8");
    }
    files.set(repoPath, { path: repoPath, bytes, text });
  }

  return files;
}

export function createCapturedCompilerHost(typescript, files) {
  const caseSensitive = Boolean(typescript.sys.useCaseSensitiveFileNames);
  const root = typescript.normalizePath(path.resolve(path.sep, "__mce4d_captured_root__"));
  const rootPrefix = root.endsWith("/") ? root : root + "/";
  const canonical = (value) => (caseSensitive ? value : value.toLowerCase());
  const byCanonicalPath = new Map();
  const directories = new Map();
  const directoryNames = new Set([canonical(root)]);
  const defaultHost = typescript.createCompilerHost({}, true);
  const defaultLibRoot = typescript.normalizePath(
    path.dirname(typescript.getDefaultLibFilePath({})),
  );
  const defaultLibPrefix = defaultLibRoot.endsWith("/") ? defaultLibRoot : defaultLibRoot + "/";

  for (const [repoPath, file] of files) {
    const key = canonical(repoPath);
    if (byCanonicalPath.has(key)) {
      throw new Error("captured files collide under the compiler path rules");
    }
    byCanonicalPath.set(key, file);
    const parts = repoPath.split("/");
    let parent = "";
    for (let index = 0; index < parts.length - 1; index += 1) {
      parent = parent ? parent + "/" + parts[index] : parts[index];
      directoryNames.add(canonical(root + "/" + parent));
    }
  }

  function absolutePath(repoPath) {
    return typescript.normalizePath(path.resolve(root, ...repoPath.split("/")));
  }

  function repoPathFor(absolutePathValue) {
    const normalized = typescript.normalizePath(absolutePathValue);
    if (!canonical(normalized).startsWith(canonical(rootPrefix))) {
      return null;
    }
    return normalized.slice(rootPrefix.length);
  }

  function trustedLibraryPath(value) {
    const normalized = typescript.normalizePath(value);
    return (
      (canonical(normalized) === canonical(defaultLibRoot) ||
        canonical(normalized).startsWith(canonical(defaultLibPrefix))) &&
      /\.d\.ts$/i.test(normalized)
    );
  }

  function readFile(fileName) {
    const repoPath = repoPathFor(fileName);
    if (repoPath !== null) {
      return byCanonicalPath.get(canonical(repoPath))?.text;
    }
    if (!trustedLibraryPath(fileName)) {
      return undefined;
    }
    try {
      return fs.readFileSync(fileName, "utf8");
    } catch {
      return undefined;
    }
  }

  function fileExists(fileName) {
    const repoPath = repoPathFor(fileName);
    if (repoPath !== null) {
      return byCanonicalPath.has(canonical(repoPath));
    }
    if (!trustedLibraryPath(fileName)) {
      return false;
    }
    try {
      return fs.statSync(fileName).isFile();
    } catch {
      return false;
    }
  }

  function directEntries(directoryName) {
    const normalizedDirectory = typescript.normalizePath(directoryName);
    const repoDirectory = repoPathFor(normalizedDirectory);
    if (repoDirectory === null && canonical(normalizedDirectory) !== canonical(root)) {
      return { files: [], directories: [] };
    }
    const parentParts = repoDirectory === null || repoDirectory === ""
      ? []
      : repoDirectory.split("/");
    const key = canonical(normalizedDirectory);
    let current = directories.get(key);
    if (!current) {
      current = { files: new Set(), directories: new Set() };
      directories.set(key, current);
      for (const file of byCanonicalPath.values()) {
        const parts = file.path.split("/");
        if (parts.length <= parentParts.length) continue;
        let matchesParent = true;
        for (let index = 0; index < parentParts.length; index += 1) {
          if (canonical(parts[index]) !== canonical(parentParts[index])) {
            matchesParent = false;
            break;
          }
        }
        if (!matchesParent) continue;
        const next = parts[parentParts.length];
        if (parts.length === parentParts.length + 1) {
          current.files.add(next);
        } else {
          current.directories.add(next);
        }
      }
    }
    return {
      files: [...current.files].sort(),
      directories: [...current.directories].sort(),
    };
  }

  const host = Object.create(defaultHost);
  host.getCurrentDirectory = () => root;
  host.useSourceOfProjectReferenceRedirect = () => true;
  host.getCanonicalFileName = (fileName) => canonical(fileName);
  host.useCaseSensitiveFileNames = () => caseSensitive;
  host.getNewLine = () => "\n";
  host.realpath = (fileName) => typescript.normalizePath(fileName);
  host.directoryExists = (directoryName) => {
    const normalized = typescript.normalizePath(directoryName);
    if (directoryNames.has(canonical(normalized))) return true;
    if (
      canonical(normalized) !== canonical(defaultLibRoot) &&
      !canonical(normalized).startsWith(canonical(defaultLibPrefix))
    ) {
      return false;
    }
    try {
      return fs.statSync(directoryName).isDirectory();
    } catch {
      return false;
    }
  };
  host.getDirectories = (directoryName) => directEntries(directoryName).directories;
  host.fileExists = fileExists;
  host.readFile = readFile;
  host.readDirectory = (rootDir, extensions, excludes, includes, depth) => {
    const normalizedRoot = typescript.normalizePath(rootDir);
    if (
      canonical(normalizedRoot) !== canonical(root) &&
      !canonical(normalizedRoot).startsWith(canonical(rootPrefix))
    ) {
      return [];
    }
    return typescript.matchFiles(
      normalizedRoot,
      extensions,
      excludes,
      includes,
      caseSensitive,
      root,
      depth,
      directEntries,
      (fileName) => typescript.normalizePath(fileName),
    );
  };
  host.getDefaultLibLocation = () => defaultLibRoot;
  host.getDefaultLibFileName = (options) => typescript.getDefaultLibFileName(options);
  host.getSourceFile = (fileName, languageVersion, onError) => {
    const text = readFile(fileName);
    if (text === undefined) {
      onError?.("source is outside the captured root");
      return undefined;
    }
    return typescript.createSourceFile(
      fileName,
      text,
      languageVersion,
      true,
      typescript.getScriptKindFromFileName(fileName),
    );
  };
  host.getSourceFileByPath = (fileName, _path, languageVersion, onError) =>
    host.getSourceFile(fileName, languageVersion, onError);
  host.writeFile = () => {
    throw new Error("provider emit is disabled");
  };

  return {
    root,
    files,
    host,
    absolutePath,
    repoPathFor,
    isCapturedPath(fileName) {
      const repoPath = repoPathFor(fileName);
      return repoPath !== null && byCanonicalPath.has(canonical(repoPath));
    },
    getCapturedFile(repoPath) {
      return byCanonicalPath.get(canonical(repoPath));
    },
  };
}
