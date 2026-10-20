import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";

const baseURL = process.env.MCE4D_EXPLORER_URL;
const bootstrapCode = process.env.MCE4D_EXPLORER_BOOTSTRAP_CODE;
const expectedSource = process.env.MCE4D_EXPLORER_SOURCE;

assert.ok(baseURL, "MCE4D_EXPLORER_URL is required");
assert.ok(bootstrapCode, "MCE4D_EXPLORER_BOOTSTRAP_CODE is required");
assert.ok(expectedSource, "MCE4D_EXPLORER_SOURCE is required");
assert.equal(new URL(baseURL).hostname, "127.0.0.1", "Explorer must use IPv4 loopback");

const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const pageErrors = [];
  const crossOriginRequests = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  page.on("request", (request) => {
    if (new URL(request.url()).origin !== new URL(baseURL).origin) {
      crossOriginRequests.push(request.url());
    }
  });

  const documentResponse = await page.goto(baseURL, { waitUntil: "networkidle" });
  assert.equal(documentResponse?.status(), 200, "Explorer shell must load from the local server");
  const staticHeaders = await documentResponse.allHeaders();
  assert.equal(staticHeaders["x-frame-options"], "DENY", "Explorer must refuse framing");
  assert.match(staticHeaders["content-security-policy"] ?? "", /frame-ancestors 'none'/);
  assert.equal(staticHeaders["x-content-type-options"], "nosniff");
  assert.equal(staticHeaders["cache-control"], "no-store");
  await page.getByLabel("Mã bootstrap").fill(bootstrapCode);
  await page.getByRole("button", { name: "Kết nối an toàn" }).click();

  const graph = page.getByRole("listbox", { name: "Các nút trong đồ thị" });
  await graph.waitFor({ state: "visible" });
  const mainNode = graph.getByRole("option").filter({ hasText: "explorerfixture.Main" });
  await mainNode.waitFor({ state: "visible" });
  await mainNode.click();
  await assert.equal(await mainNode.getAttribute("aria-selected"), "true");

  await page.getByRole("button").filter({ hasText: "src/main.go" }).click();
  await page.getByRole("tab", { name: "Source" }).click();
  const source = page.locator(".source-view pre");
  await source.waitFor({ state: "visible" });
  await assert.equal(await source.textContent(), expectedSource);

  await page.setViewportSize({ width: 390, height: 844 });
  const splitter = page.getByRole("separator", { name: "Điều chỉnh độ rộng hai pane" });
  await expect(splitter).toHaveAttribute("aria-orientation", "horizontal");
  await assert.equal(await mainNode.getAttribute("aria-selected"), "true");
  await assert.deepEqual(crossOriginRequests, []);
  await assert.deepEqual(pageErrors, []);

  process.stdout.write(JSON.stringify({
    result: "PASS",
    route: new URL(baseURL).origin,
    graphNode: "explorerfixture.Main",
    sourceBytes: Buffer.byteLength(expectedSource, "utf8"),
    responsiveSplitter: "horizontal",
    pageErrors: pageErrors.length,
    crossOriginRequests: crossOriginRequests.length,
  }) + "\n");
} finally {
  await browser.close();
}
