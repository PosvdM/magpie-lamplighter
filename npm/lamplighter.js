// The parts of the plugin's host side: where the program for this platform
// is, the port it listens on, keeping it running, and calling its tools.
// npm/index.js puts them together; tests drive them with fakes.

import { spawn as childSpawn } from "node:child_process"
import fs from "node:fs"
import path from "node:path"
import { toolArguments } from "./tools.js"

// DEFAULT_PORT is the internal listener's port unless the plugin's port
// option says otherwise: main.go's -listen default, and package.json's
// magpie.page.service.port.
export const DEFAULT_PORT = 8317

// TOOL_TIMEOUT_MS bounds a tool call below Magpie's 55 s (ToolTimeout), so
// the call ends with Lamplighter's own error. The internal listener gives a
// tool 50 s.
export const TOOL_TIMEOUT_MS = 54_000

// START_WAIT_MS is how long a tool call waits for the internal listener to
// accept connections: the program may be starting or restarting.
export const START_WAIT_MS = 10_000

const OS = { linux: "linux", darwin: "darwin", win32: "windows" }
const ARCH = { x64: "amd64", arm64: "arm64" }

// platformDir is the folder under dist/bin of the program for a Node
// platform and architecture, or "" when the package ships none.
export function platformDir(platform = process.platform, arch = process.arch) {
  const os = OS[platform]
  const cpu = ARCH[arch]
  return os && cpu ? `${os}-${cpu}` : ""
}

// binaryPath is the program for this platform in the package at root, or
// "" when the package ships none for it.
export function binaryPath(root, platform = process.platform, arch = process.arch) {
  const dir = platformDir(platform, arch)
  if (!dir) return ""
  return path.join(root, "dist", "bin", dir, platform === "win32" ? "lamplighter.exe" : "lamplighter")
}

// portOf reads a port as Magpie reads magpie.page.service's option
// (plugin.portOf): a whole number from 1 to 65535, or a string of one.
// Anything else is fallback.
export function portOf(value, fallback = DEFAULT_PORT) {
  let n
  if (typeof value === "number") n = value
  else if (typeof value === "string" && /^\s*\d+\s*$/.test(value)) n = Number(value)
  else return fallback
  return Number.isInteger(n) && n >= 1 && n <= 65535 ? n : fallback
}

// childEnv is the program's environment: the host's, plus
//   - MAGPIE_CONFIG_DIR, Magpie's configuration directory, which is the
//     plugin host's working directory (cwd), unless the host has one;
//   - HTTPS_PROXY, HTTP_PROXY and NO_PROXY from the host's MAGPIE_*_PROXY,
//     each unless the host has a variable of that name in any case.
// Magpie gives the host the proxy its own requests take (netproxy.Env:
// the one set in Magpie, else the system's; none for "direct"; a SOCKS5
// one bridged) under MAGPIE_* names, since Bun would otherwise put every
// fetch through it, and host.js maps them back the same way for the
// programs a plugin starts. ALL_PROXY stands in for a missing HTTPS_PROXY
// or HTTP_PROXY, as in host.js. MAGPIE_ADDR is passed as it is.
export function childEnv(env, cwd) {
  const out = { ...env }
  const has = (name) => Object.keys(out).some((k) => k.toUpperCase() === name)
  const magpie = (name) => env[`MAGPIE_${name}`] || env[`MAGPIE_${name.toLowerCase()}`] || ""
  if (!out.MAGPIE_CONFIG_DIR && cwd) out.MAGPIE_CONFIG_DIR = cwd
  const proxies = {
    HTTPS_PROXY: magpie("HTTPS_PROXY") || magpie("ALL_PROXY"),
    HTTP_PROXY: magpie("HTTP_PROXY") || magpie("ALL_PROXY"),
    NO_PROXY: magpie("NO_PROXY"),
  }
  for (const [name, value] of Object.entries(proxies)) {
    if (value && !has(name)) out[name] = value
  }
  return out
}

// makeExecutable sets the execute bits of file when it has none: a package
// manager may unpack the program without them.
export function makeExecutable(file, platform = process.platform) {
  if (platform === "win32") return
  const mode = fs.statSync(file).mode
  if ((mode & 0o111) === 0) fs.chmodSync(file, (mode & 0o777) | 0o755)
}

// Supervisor keeps one program running. It starts it again after it ends
// on its own, waiting minDelay first, twice as long after each quick exit
// up to maxDelay, and minDelay again after a run of steadyAfter or longer.
// A quick exit is usually the port held by another host's program, which
// Magpie's plugin host leaves while it hands its calls to a new host.
export class Supervisor {
  constructor({
    file,
    args = [],
    env = process.env,
    spawn = childSpawn,
    log = (line) => process.stderr.write(`magpie-lamplighter: ${line}\n`),
    minDelay = 1000,
    maxDelay = 30_000,
    steadyAfter = 60_000,
    now = Date.now,
    setTimer = setTimeout,
    clearTimer = clearTimeout,
  }) {
    Object.assign(this, { file, args, env, spawn, log, minDelay, maxDelay, steadyAfter, now, setTimer, clearTimer })
    this.child = null
    this.timer = null
    this.delay = 0
    this.stopped = false
  }

  start() {
    if (this.stopped || this.child || this.timer) return
    this.run()
  }

  run() {
    this.timer = null
    if (this.stopped) return
    const started = this.now()
    let child
    try {
      // stdin is not the plugin host's (its protocol), and the program
      // logs to stderr, which Magpie keeps in its log.
      child = this.spawn(this.file, this.args, { env: this.env, stdio: ["ignore", "inherit", "inherit"], windowsHide: true })
    } catch (e) {
      this.log(`cannot start ${this.file}: ${e?.message ?? e}`)
      this.again(started)
      return
    }
    this.child = child
    let over = false
    const end = (why) => {
      if (over) return
      over = true
      if (this.child === child) this.child = null
      if (this.stopped) return
      this.log(`${why}; starting again in ${Math.round(this.nextDelay(started) / 1000)} s`)
      this.again(started)
    }
    // A program that can't be started gives "error" and may give no
    // "exit".
    child.once("error", (e) => end(`cannot start ${this.file}: ${e?.message ?? e}`))
    child.once("exit", (code, signal) => end(signal ? `exited on ${signal}` : `exited with code ${code}`))
  }

  nextDelay(started) {
    if (this.now() - started >= this.steadyAfter || this.delay === 0) return this.minDelay
    return Math.min(this.delay * 2, this.maxDelay)
  }

  again(started) {
    this.delay = this.nextDelay(started)
    this.timer = this.setTimer(() => this.run(), this.delay)
    this.timer?.unref?.()
  }

  // stop ends the program (SIGTERM, on which it shuts down) and starts it
  // no more.
  stop() {
    this.stopped = true
    if (this.timer) this.clearTimer(this.timer)
    this.timer = null
    const child = this.child
    this.child = null
    if (child && child.exitCode === null && child.signalCode === null) {
      try {
        child.kill()
      } catch {
        // already gone
      }
    }
  }
}

const sleep = (ms, signal) =>
  new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason)
    const t = setTimeout(done, ms)
    function done() {
      signal?.removeEventListener("abort", abort)
      resolve()
    }
    function abort() {
      clearTimeout(t)
      reject(signal.reason)
    }
    signal?.addEventListener("abort", abort, { once: true })
  })

// callTool runs one of Lamplighter's tools through the internal listener
// at port: POST /api/tools/{name}, answered with {"result": …} or
// {"error": "…"}. It gives the result as JSON text, as Lamplighter's MCP
// server does, and throws the error, which Magpie returns as the call's
// error. While the listener refuses connections it tries again for
// startWait.
export async function callTool({ port, name, args, signal, fetch = globalThis.fetch, startWait = START_WAIT_MS, retryEvery = 250, now = Date.now }) {
  const body = JSON.stringify(toolArguments(name, args))
  const url = `http://127.0.0.1:${port}/api/tools/${encodeURIComponent(name)}`
  const deadline = now() + startWait
  let res
  for (;;) {
    try {
      res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body, signal })
      break
    } catch (e) {
      if (signal?.aborted) throw new Error(`${name}: Lamplighter did not answer in time`)
      if (now() >= deadline) throw new Error(`Lamplighter is not running on 127.0.0.1:${port}: ${e?.message ?? e}`)
      await sleep(retryEvery, signal).catch(() => {})
    }
  }
  const raw = await res.text()
  let data = null
  try {
    data = JSON.parse(raw)
  } catch {
    // not JSON: reported below
  }
  if (res.ok && data && typeof data === "object" && typeof data.error === "string") throw new Error(data.error)
  if (!res.ok || !data || typeof data !== "object" || !("result" in data)) {
    const why = typeof data?.error === "string" ? data.error : raw.trim().slice(0, 300)
    throw new Error(`Lamplighter answered HTTP ${res.status}${why ? `: ${why}` : ""}`)
  }
  return JSON.stringify(data.result)
}
