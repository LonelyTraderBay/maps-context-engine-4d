import { runProviderCLI } from "./protocol.mjs";

try {
  await runProviderCLI();
} catch {
  process.stderr.write("TypeScript provider rejected the request.\n");
  process.exitCode = 1;
}
