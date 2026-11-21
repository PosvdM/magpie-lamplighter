// Checks before npm packs the package (prepack) that dist/bin holds the
// program for every platform, built for package.json's version by
// scripts/build.sh. A package without them would install and then fail to
// start in Magpie, which installs plugins without running their scripts.
import fs from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"

export const PLATFORMS = ["linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64", "windows-arm64"]

export function problems(root) {
  const out = []
  const version = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8")).version
  const bin = path.join(root, "dist", "bin")
  let built = ""
  try {
    built = fs.readFileSync(path.join(bin, "VERSION"), "utf8").trim()
  } catch {
    out.push("dist/bin/VERSION is missing")
  }
  if (built && built !== version) out.push(`dist/bin is built for ${built}, package.json says ${version}`)
  for (const p of PLATFORMS) {
    const file = path.join(bin, p, p.startsWith("windows-") ? "lamplighter.exe" : "lamplighter")
    let st
    try {
      st = fs.statSync(file)
    } catch {
      out.push(`${path.relative(root, file)} is missing`)
      continue
    }
    if (!st.isFile() || st.size === 0) out.push(`${path.relative(root, file)} is empty`)
    else if (!p.startsWith("windows-") && (st.mode & 0o111) === 0) out.push(`${path.relative(root, file)} is not executable`)
  }
  return out
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = path.join(path.dirname(fileURLToPath(import.meta.url)), "..")
  const found = problems(root)
  if (found.length) {
    for (const p of found) console.error(p)
    console.error("Run `sh scripts/build.sh` first.")
    process.exit(1)
  }
}
