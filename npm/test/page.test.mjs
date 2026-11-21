import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import test from "node:test"
import { fileURLToPath } from "node:url"
import vm from "node:vm"

const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "page")
const script = fs.readFileSync(path.join(dir, "open.js"), "utf8")

// open runs open.js with a bridge (magpie) and answers where the page
// went.
function open(magpie) {
  return new Promise((resolve) => {
    const context = { setTimeout, location: { replace: resolve } }
    if (magpie !== undefined) context.magpie = magpie
    vm.runInNewContext(script, context)
  })
}

const bridge = ({ service = "", options = {}, ready = Promise.resolve({}) } = {}) => ({
  ready,
  service,
  options: { get: async () => options },
})

test("through magpie web the page goes to the service Magpie gives", async () => {
  assert.equal(await open(bridge({ service: "https://magpie.example/_magpie/service/tok/", options: { port: 9000 } })), "https://magpie.example/_magpie/service/tok/")
})

test("in the app's window the page goes to 127.0.0.1 on the port option", async () => {
  assert.equal(await open(bridge({ options: { port: 9000 } })), "http://127.0.0.1:9000/")
  assert.equal(await open(bridge({ options: { port: "9001" } })), "http://127.0.0.1:9001/")
})

test("without a port option the page goes to the default port", async () => {
  assert.equal(await open(bridge()), "http://127.0.0.1:8317/")
  assert.equal(await open(bridge({ options: { port: "x" } })), "http://127.0.0.1:8317/")
  const failing = bridge()
  failing.options.get = async () => {
    throw new Error("no options")
  }
  assert.equal(await open(failing), "http://127.0.0.1:8317/")
})

test("without the bridge the page goes to the default port", async () => {
  assert.equal(await open(undefined), "http://127.0.0.1:8317/")
  assert.equal(await open(bridge({ ready: Promise.reject(new Error("broken")) })), "http://127.0.0.1:8317/")
})

test("the page loads the bridge before its script and has no inline script", () => {
  const html = fs.readFileSync(path.join(dir, "index.html"), "utf8")
  const scripts = [...html.matchAll(/<script([^>]*)>([\s\S]*?)<\/script>/g)]
  assert.deepEqual(scripts.map((m) => m[1].match(/src="([^"]+)"/)?.[1]), ["_magpie/bridge.js", "open.js"])
  for (const m of scripts) assert.equal(m[2].trim(), "")
})
