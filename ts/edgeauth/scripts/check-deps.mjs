// The root package.json is what apps install; this package only builds and
// tests it. Their jose ranges must agree, or the tests run another jose.
import { readFileSync } from "node:fs";

const read = (path) => JSON.parse(readFileSync(new URL(path, import.meta.url), "utf8"));
const root = read("../../../package.json").dependencies;
const dev = read("../package.json").dependencies;
if (JSON.stringify(root) !== JSON.stringify(dev)) {
  console.error(`dependencies differ: root ${JSON.stringify(root)}, ts/edgeauth ${JSON.stringify(dev)}`);
  process.exit(1);
}
