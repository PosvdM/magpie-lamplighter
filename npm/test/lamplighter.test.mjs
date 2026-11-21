import assert from "node:assert/strict"
import { EventEmitter } from "node:events"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import test from "node:test"
import { DEFAULT_PORT, Supervisor, binaryPath, callTool, childEnv, makeExecutable, platformDir, portOf } from "../lamplighter.js"

test("platformDir maps Node's names to the GOOS-GOARCH folders the package ships", () => {
  assert.equal(platformDir("linux", "x64"), "linux-amd64")
  assert.equal(platformDir("linux", "arm64"), "linux-arm64")
  assert.equal(platformDir("darwin", "x64"), "darwin-amd64")
  assert.equal(platformDir("darwin", "arm64"), "darwin-arm64")
  assert.equal(platformDir("win32", "x64"), "windows-amd64")
  assert.equal(platformDir("win32", "arm64"), "windows-arm64")
  assert.equal(platformDir("freebsd", "x64"), "")
  assert.equal(platformDir("linux", "ia32"), "")
})

test("binaryPath names the program, with .exe on Windows", () => {
  assert.equal(binaryPath("/p", "linux", "x64"), path.join("/p", "dist", "bin", "linux-amd64", "lamplighter"))
  assert.equal(binaryPath("/p", "win32", "arm64"), path.join("/p", "dist", "bin", "windows-arm64", "lamplighter.exe"))
  assert.equal(binaryPath("/p", "aix", "ppc64"), "")
})

test("portOf reads a port as Magpie reads the service option", () => {
  assert.equal(portOf(undefined), DEFAULT_PORT)
  assert.equal(portOf(9000), 9000)
  assert.equal(portOf(" 9001 "), 9001)
  assert.equal(portOf(0), DEFAULT_PORT)
  assert.equal(portOf(65536), DEFAULT_PORT)
  assert.equal(portOf(80.5), DEFAULT_PORT)
  assert.equal(portOf("80a"), DEFAULT_PORT)
  assert.equal(portOf(true), DEFAULT_PORT)
})

test("childEnv passes MAGPIE_ADDR and adds the config dir from the host's cwd", () => {
  const env = childEnv({ MAGPIE_ADDR: "127.0.0.1:3425", PATH: "/bin" }, "/home/u/.config/magpie")
  assert.deepEqual(env, { MAGPIE_ADDR: "127.0.0.1:3425", PATH: "/bin", MAGPIE_CONFIG_DIR: "/home/u/.config/magpie" })
  assert.equal(childEnv({ MAGPIE_CONFIG_DIR: "/portable/config" }, "/elsewhere").MAGPIE_CONFIG_DIR, "/portable/config")
})

test("childEnv maps the host's MAGPIE_*_PROXY back", () => {
  const env = childEnv({ MAGPIE_HTTPS_PROXY: "http://127.0.0.1:7890", MAGPIE_HTTP_PROXY: "http://127.0.0.1:7891", MAGPIE_NO_PROXY: "corp.example" }, "/c")
  assert.equal(env.HTTPS_PROXY, "http://127.0.0.1:7890")
  assert.equal(env.HTTP_PROXY, "http://127.0.0.1:7891")
  assert.equal(env.NO_PROXY, "corp.example")
  assert.equal(env.MAGPIE_HTTPS_PROXY, "http://127.0.0.1:7890", "the MAGPIE_ names stay")
})

test("childEnv takes ALL_PROXY for a missing one and keeps the host's own", () => {
  const all = childEnv({ MAGPIE_ALL_PROXY: "http://127.0.0.1:1080", MAGPIE_HTTP_PROXY: "http://h:1" }, "/c")
  assert.equal(all.HTTPS_PROXY, "http://127.0.0.1:1080")
  assert.equal(all.HTTP_PROXY, "http://h:1")
  assert.equal(all.NO_PROXY, undefined)
  const own = childEnv({ MAGPIE_HTTPS_PROXY: "http://m:1", https_proxy: "http://own:2", MAGPIE_NO_PROXY: "x", NO_PROXY: "" }, "/c")
  assert.equal(own.HTTPS_PROXY, undefined)
  assert.equal(own.https_proxy, "http://own:2")
  assert.equal(own.NO_PROXY, "")
  // "direct" in Magpie: no proxy at all
  assert.deepEqual(childEnv({ MAGPIE_HTTPS_PROXY: "" }, "/c"), { MAGPIE_HTTPS_PROXY: "", MAGPIE_CONFIG_DIR: "/c" })
})

test("makeExecutable adds the execute bits a package manager left off", { skip: process.platform === "win32" }, () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "lamplighter-"))
  try {
    const file = path.join(dir, "lamplighter")
    fs.writeFileSync(file, "#!/bin/sh\n")
    fs.chmodSync(file, 0o644)
    makeExecutable(file)
    assert.equal(fs.statSync(file).mode & 0o777, 0o755)
    fs.chmodSync(file, 0o700)
    makeExecutable(file)
    assert.equal(fs.statSync(file).mode & 0o777, 0o700, "an executable file is left as it is")
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// fakeRun is a Supervisor with a spawn, a clock and timers of its own.
function fakeRun(opts = {}) {
  const children = []
  const timers = []
  const logs = []
  const clock = { t: 0 }
  const sup = new Supervisor({
    file: "/bin/lamplighter",
    args: ["-listen", "127.0.0.1:9000"],
    env: { MAGPIE_ADDR: "127.0.0.1:3425" },
    spawn(file, args, options) {
      if (opts.throws) throw new Error("spawn failed")
      const child = new EventEmitter()
      Object.assign(child, { file, args, options, exitCode: null, signalCode: null, killed: 0 })
      child.kill = () => {
        child.killed++
        return true
      }
      children.push(child)
      return child
    },
    log: (line) => logs.push(line),
    now: () => clock.t,
    setTimer(fn, ms) {
      const t = { fn, ms, cleared: false }
      timers.push(t)
      return t
    },
    clearTimer(t) {
      t.cleared = true
    },
  })
  const fire = () => {
    const t = timers.filter((x) => !x.cleared).at(-1)
    t.cleared = true
    t.fn()
  }
  return { sup, children, timers, logs, clock, fire }
}

test("Supervisor starts the program with the listener, the environment and no stdin", () => {
  const { sup, children } = fakeRun()
  sup.start()
  sup.start()
  assert.equal(children.length, 1, "started once")
  const c = children[0]
  assert.equal(c.file, "/bin/lamplighter")
  assert.deepEqual(c.args, ["-listen", "127.0.0.1:9000"])
  assert.equal(c.options.env.MAGPIE_ADDR, "127.0.0.1:3425")
  assert.equal(c.options.stdio[0], "ignore")
  assert.equal(c.options.windowsHide, true)
})

test("Supervisor starts the program again, backing off after quick exits", () => {
  const { sup, children, timers, clock, fire, logs } = fakeRun()
  sup.start()
  children[0].emit("exit", 1, null)
  assert.equal(timers.at(-1).ms, 1000)
  assert.match(logs.at(-1), /exited with code 1/)
  fire()
  assert.equal(children.length, 2)
  children[1].emit("exit", 1, null)
  assert.equal(timers.at(-1).ms, 2000)
  fire()
  children[2].emit("exit", 1, null)
  assert.equal(timers.at(-1).ms, 4000)
  // a long run starts the back-off over
  fire()
  clock.t += 120_000
  children[3].emit("exit", null, "SIGKILL")
  assert.equal(timers.at(-1).ms, 1000)
  assert.match(logs.at(-1), /exited on SIGKILL/)
})

test("Supervisor caps the back-off", () => {
  const { sup, children, timers, fire } = fakeRun()
  sup.start()
  for (let i = 0; i < 10; i++) {
    children.at(-1).emit("exit", 1, null)
    fire()
  }
  assert.equal(timers.at(-1).ms, 30_000)
})

test("Supervisor takes a spawn error and an exit after it as one end", () => {
  const { sup, children, timers } = fakeRun()
  sup.start()
  children[0].emit("error", new Error("EACCES"))
  children[0].emit("exit", -2, null)
  assert.equal(timers.length, 1)
})

test("Supervisor tries again when spawn throws", () => {
  const { sup, timers, logs } = fakeRun({ throws: true })
  sup.start()
  assert.equal(timers.length, 1)
  assert.match(logs[0], /cannot start/)
})

test("Supervisor.stop ends the program and starts it no more", () => {
  const { sup, children, timers } = fakeRun()
  sup.start()
  sup.stop()
  assert.equal(children[0].killed, 1)
  children[0].emit("exit", null, "SIGTERM")
  assert.equal(timers.length, 0)
  sup.start()
  assert.equal(children.length, 1)
})

test("Supervisor.stop cancels a pending start", () => {
  const { sup, children, timers } = fakeRun()
  sup.start()
  children[0].emit("exit", 1, null)
  sup.stop()
  assert.equal(timers[0].cleared, true)
})

const answer = (status, body) => ({ status, ok: status >= 200 && status < 300, text: async () => (typeof body === "string" ? body : JSON.stringify(body)) })

test("callTool posts the string arguments and gives the result as JSON", async () => {
  const seen = []
  const out = await callTool({
    port: 9000,
    name: "get_quota_events",
    args: { since: "2026-11-01T00:00:00Z", limit: 5, ignored: undefined },
    fetch: async (url, init) => {
      seen.push({ url, init })
      return answer(200, { result: { ready: true, events: [] } })
    },
  })
  assert.equal(seen[0].url, "http://127.0.0.1:9000/api/tools/get_quota_events")
  assert.equal(seen[0].init.method, "POST")
  assert.deepEqual(JSON.parse(seen[0].init.body), { since: "2026-11-01T00:00:00Z", limit: "5" })
  assert.deepEqual(JSON.parse(out), { ready: true, events: [] })
})

test("callTool throws the tool's error", async () => {
  await assert.rejects(callTool({ port: 1, name: "list_quotas", args: {}, fetch: async () => answer(200, { error: "unknown account" }) }), /^Error: unknown account$/)
})

test("callTool reports an HTTP error with the listener's message", async () => {
  await assert.rejects(callTool({ port: 1, name: "list_quotas", args: {}, fetch: async () => answer(400, { error: "bad body" }) }), /HTTP 400: bad body/)
  await assert.rejects(callTool({ port: 1, name: "list_quotas", args: {}, fetch: async () => answer(502, "Bad Gateway") }), /HTTP 502: Bad Gateway/)
})

test("callTool refuses arguments that are no strings", async () => {
  await assert.rejects(callTool({ port: 1, name: "list_quotas", args: { provider: 3 }, fetch: async () => assert.fail("sent") }), /arguments must be strings/)
  await assert.rejects(callTool({ port: 1, name: "get_quota_events", args: { limit: 2.5 }, fetch: async () => assert.fail("sent") }), /limit must be an integer/)
})

test("callTool waits for a listener that is starting", async () => {
  let tries = 0
  const out = await callTool({
    port: 1,
    name: "list_quotas",
    args: {},
    retryEvery: 1,
    fetch: async () => {
      if (++tries < 3) throw new TypeError("fetch failed: ECONNREFUSED")
      return answer(200, { result: 1 })
    },
  })
  assert.equal(out, "1")
  assert.equal(tries, 3)
})

test("callTool gives up on a listener that stays down", async () => {
  let t = 0
  await assert.rejects(
    callTool({ port: 7, name: "list_quotas", args: {}, retryEvery: 1, startWait: 100, now: () => (t += 60), fetch: async () => { throw new TypeError("ECONNREFUSED") } }),
    /not running on 127\.0\.0\.1:7/,
  )
})

test("callTool stops when the call is aborted", async () => {
  const ctl = new AbortController()
  ctl.abort()
  await assert.rejects(
    callTool({ port: 1, name: "list_quotas", args: {}, signal: ctl.signal, fetch: async () => { throw new DOMException("aborted", "AbortError") } }),
    /did not answer in time/,
  )
})
