import { TextDecoder } from "node:util";
import { analyzeProviderRequest } from "./analyze.mjs";

const MAX_HEADER_BYTES = 64 * 1024;
const MAX_BODY_BYTES = 16 * 1024 * 1024;
const SEPARATOR = Buffer.from("\r\n\r\n", "ascii");
const utf8 = new TextDecoder("utf-8", { fatal: true });

function decodeFrame(frame) {
  if (!Buffer.isBuffer(frame) || frame.length > MAX_HEADER_BYTES + SEPARATOR.length + MAX_BODY_BYTES) {
    throw new Error("provider frame exceeds its byte budget");
  }
  const separator = frame.indexOf(SEPARATOR);
  if (separator < 0 || separator > MAX_HEADER_BYTES) {
    throw new Error("provider frame header is incomplete or too large");
  }
  const header = frame.subarray(0, separator).toString("ascii");
  const match = /^Content-Length: (0|[1-9][0-9]*)$/.exec(header);
  if (!match) {
    throw new Error("provider frame must contain one canonical Content-Length header");
  }
  const length = Number(match[1]);
  const bodyStart = separator + SEPARATOR.length;
  if (!Number.isSafeInteger(length) || length < 1 || length > MAX_BODY_BYTES || bodyStart + length !== frame.length) {
    throw new Error("provider frame length is invalid or has trailing bytes");
  }
  return JSON.parse(utf8.decode(frame.subarray(bodyStart)));
}

function encodeFrame(value) {
  const body = Buffer.from(JSON.stringify(value), "utf8");
  if (body.length < 1 || body.length > MAX_BODY_BYTES) {
    throw new Error("provider response exceeds its byte budget");
  }
  return Buffer.concat([
    Buffer.from("Content-Length: " + body.length + "\r\n\r\n", "ascii"),
    body,
  ]);
}

async function readInput(stream) {
  const chunks = [];
  let total = 0;
  for await (const chunk of stream) {
    total += chunk.length;
    if (total > MAX_HEADER_BYTES + SEPARATOR.length + MAX_BODY_BYTES) {
      throw new Error("provider request exceeds its byte budget");
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks, total);
}

export async function runProviderCLI(input = process.stdin, output = process.stdout) {
  const request = decodeFrame(await readInput(input));
  const result = analyzeProviderRequest(request);
  const frame = encodeFrame(result);
  await new Promise((resolve, reject) => {
    output.write(frame, (error) => (error ? reject(error) : resolve()));
  });
}
