// Runs the real program under the Supervisor and calls a tool through the
// internal listener, as the plugin does in Magpie's host. The program is
// built without a version, so it asks GitHub for no release, and
// MAGPIE_ADDR points at a closed loopback port.
import assert from "node:assert/strict"
import { execFileSync, spawn } from "node:child_process"
import fs from "node:fs"
import net from "node:net"
import os from "node:os"
import path from "node:path"
import test from "node:test"
import { fileURLToPath } from "node:url"
import { Supervisor, callTool } from "../lamplighter.js"

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..")

const haveGo = (() => {
  try {
    execFileSync("go", ["version"], { stdio: "ignore" })
    return true
  } catch {
    return false
  }
})()

const freePort = () =>
  new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.once("error", reject)
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address()
      srv.close(() => resolve(port))
    })
  })

test("the program starts, answers a tool call and ends with stop", { skip: !haveGo && "no Go", timeout: 180_000 }, async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "lamplighter-"))
  const file = path.join(dir, process.platform === "win32" ? "lamplighter.exe" : "lamplighter")
  let sup
  try {
    execFileSync("go", ["build", "-o", file, "."], { cwd: root, env: { ...process.env, CGO_ENABLED: "0" }, stdio: ["ignore", "ignore", "inherit"] })
    const port = await freePort()
    let child
    const logs = []
    sup = new Supervisor({
      file,
      args: ["-listen", `127.0.0.1:${port}`],
      env: { ...process.env, LAMPLIGHTER_DATA_DIR: path.join(dir, "data"), MAGPIE_ADDR: "127.0.0.1:1" },
      log: (line) => logs.push(line),
      // the real spawn, keeping the child and its log out of the test's
      spawn: (f, a, o) => (child = spawn(f, a, { ...o, stdio: "ignore" })),
    })
    sup.start()
    const out = JSON.parse(await callTool({ port, name: "list_quotas", args: {}, startWait: 30_000 }))
    assert.equal(typeof out, "object")
    assert.equal(out.ready, false, JSON.stringify(out))
    // limit goes to the listener as a string, which it takes
    const events = JSON.parse(await callTool({ port, name: "get_quota_events", args: { limit: 5 } }))
    assert.equal(events.ready, false, JSON.stringify(events))
    await assert.rejects(callTool({ port, name: "no_such_tool", args: {} }), /no_such_tool|unknown|未知/i)
    const exited = new Promise((resolve) => child.once("exit", (code, signal) => resolve({ code, signal })))
    sup.stop()
    const end = await exited
    assert.ok(end.code === 0 || end.signal === "SIGTERM", JSON.stringify(end))
    assert.deepEqual(logs, [])
  } finally {
    sup?.stop()
    fs.rmSync(dir, { recursive: true, force: true })
  }
})
