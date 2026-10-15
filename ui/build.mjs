// Builds the console page: copies `src/app.js` and `src/app.css` into `dist/`, which
// `index.html` loads. There is no bundler and no framework; the page is hand-written CSS and JS.
//
// `npm --prefix ui run build` from the project root is the whole of what a person runs to
// rebuild the page. See `README.md` beside this file.

import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const srcDir = join(here, "src");
const outDir = join(here, "dist");

mkdirSync(outDir, { recursive: true });
for (const name of ["app.js", "app.css"]) {
  copyFileSync(join(srcDir, name), join(outDir, name));
  console.log(`built ${name} -> ${join("dist", name)}`);
}
