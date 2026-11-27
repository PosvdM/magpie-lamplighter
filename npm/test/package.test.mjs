import assert from "node:assert/strict"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import test from "node:test"
import { fileURLToPath } from "node:url"
import { DEFAULT_PORT } from "../lamplighter.js"
import { GOOGLE_WORDS, PLATFORMS, problems } from "../../scripts/check-dist.mjs"

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..")
const pkg = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8"))

test("the default port is main.go's -listen default and the page service's", () => {
  const main = fs.readFileSync(path.join(root, "main.go"), "utf8")
  assert.match(main, new RegExp(`flag\\.String\\("listen", "127\\.0\\.0\\.1:${DEFAULT_PORT}"`))
  assert.deepEqual(pkg.magpie.page.service, { port: DEFAULT_PORT, option: "port" })
  assert.equal(pkg.magpie.options.port, DEFAULT_PORT)
  const open = fs.readFileSync(path.join(root, "npm", "page", "open.js"), "utf8")
  assert.match(open, new RegExp(`const DEFAULT_PORT = ${DEFAULT_PORT}\\b`))
})

test("the page and its icon are in the page's folder, inside the package", () => {
  const page = pkg.magpie.page
  assert.equal(page.title, "Lamplighter")
  assert.ok(page.file.endsWith(".html"))
  assert.ok(fs.statSync(path.join(root, page.file)).isFile())
  assert.equal(path.dirname(page.icon), path.dirname(page.file))
  assert.ok(fs.statSync(path.join(root, page.icon)).isFile())
})

test("the package ships the entry, the page and the programs", () => {
  const shipped = (file) => pkg.files.some((f) => (f.endsWith("/") ? file.startsWith(f) : file === f))
  assert.equal(pkg.main, "npm/index.js")
  for (const f of ["npm/index.js", "npm/lamplighter.js", "npm/tools.js", pkg.magpie.page.file, pkg.magpie.page.icon, "npm/page/open.js", "dist/bin/linux-amd64/lamplighter"]) {
    assert.ok(shipped(f), f)
  }
  assert.ok(!shipped("npm/test/package.test.mjs"))
  assert.equal(pkg.scripts.prepack, "node scripts/check-dist.mjs")
  // Magpie installs with bun add --ignore-scripts: nothing may depend on
  // an install script
  for (const s of ["preinstall", "install", "postinstall"]) assert.equal(pkg.scripts[s], undefined)
  assert.equal(pkg.dependencies, undefined)
})

test("the entry gives the host one plugin function", async () => {
  const mod = await import("../index.js")
  assert.deepEqual(Object.keys(mod), ["default"])
  assert.equal(mod.default.id, "magpie-lamplighter")
  assert.equal(typeof mod.default.server, "function")
})

test("check-dist finds a missing, empty or stale build, and Google's names in a program", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "lamplighter-"))
  try {
    fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify({ version: "1.2.3" }))
    assert.ok(problems(dir).some((p) => p.includes("VERSION is missing")))
    const bin = path.join(dir, "dist", "bin")
    fs.mkdirSync(bin, { recursive: true })
    fs.writeFileSync(path.join(bin, "VERSION"), "1.2.2\n")
    for (const p of PLATFORMS) {
      fs.mkdirSync(path.join(bin, p))
      const file = path.join(bin, p, p.startsWith("windows-") ? "lamplighter.exe" : "lamplighter")
      fs.writeFileSync(file, "x")
      fs.chmodSync(file, 0o755)
    }
    assert.deepEqual(problems(dir), ["dist/bin is built for 1.2.2, package.json says 1.2.3"])
    fs.writeFileSync(path.join(bin, "VERSION"), "1.2.3\n")
    assert.deepEqual(problems(dir), [])
    // The first word of testdata/google-words.txt, in other case.
    const word = GOOGLE_WORDS.source.split("|")[0].toUpperCase()
    fs.writeFileSync(path.join(bin, "linux-amd64", "lamplighter"), `x\0https://oauth2.${word}.com/token\0`)
    assert.deepEqual(problems(dir), [`dist/bin/linux-amd64/lamplighter contains "${word}"`])
    fs.writeFileSync(path.join(bin, "linux-amd64", "lamplighter"), "x\0plugin-auth.json\0")
    fs.writeFileSync(path.join(bin, "linux-arm64", "lamplighter"), "")
    fs.rmSync(path.join(bin, "darwin-amd64", "lamplighter"))
    assert.equal(problems(dir).length, 2)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})
