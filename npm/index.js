// The magpie-lamplighter plugin for Magpie's plugin host (Bun). It starts
// the Lamplighter program shipped in the package for this platform, on the
// internal listener 127.0.0.1:<port>, and gives Magpie's agents its tools.
// The page (npm/page) goes to the same listener. See docs/architecture.md,
// "插件外壳".
//
// The host calls the default export's server with the plugin's options
// from plugins.json; only it, so the helpers in lamplighter.js are not
// taken for plugin functions.

import path from "node:path"
import { fileURLToPath } from "node:url"
import { Supervisor, TOOL_TIMEOUT_MS, binaryPath, callTool, childEnv, makeExecutable, portOf } from "./lamplighter.js"
import { TOOLS } from "./tools.js"

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), "..")
const log = (line) => process.stderr.write(`magpie-lamplighter: ${line}\n`)

// One program for the host, however often the host loads the plugin.
let running = null

function start(port) {
  if (running) return running
  const file = binaryPath(root)
  if (!file) {
    running = { port, error: `Lamplighter has no program for ${process.platform}/${process.arch}` }
    log(running.error)
    return running
  }
  try {
    makeExecutable(file)
  } catch (e) {
    log(`cannot make ${file} executable: ${e?.message ?? e}`)
  }
  // The host runs in Magpie's configuration directory. The gateway at
  // MAGPIE_ADDR takes loopback requests without a key.
  const supervisor = new Supervisor({ file, args: ["-listen", `127.0.0.1:${port}`], env: childEnv(process.env, process.cwd()), log })
  supervisor.start()
  // The program ends with the host: Magpie also ends the host's whole
  // process tree when it stops the host.
  process.once("exit", () => supervisor.stop())
  running = { port, supervisor }
  return running
}

function tools(state) {
  const out = {}
  for (const [name, def] of Object.entries(TOOLS)) {
    out[name] = {
      description: def.description,
      // a JSON Schema, which the host lists as it is and does not check:
      // the internal listener checks the arguments
      args: { type: "object", properties: def.properties, additionalProperties: false },
      async execute(args, ctx) {
        if (state.error) throw new Error(state.error)
        const signal = AbortSignal.any([AbortSignal.timeout(TOOL_TIMEOUT_MS), ...(ctx?.abort ? [ctx.abort] : [])])
        return callTool({ port: state.port, name, args, signal })
      },
    }
  }
  return out
}

async function server(_input, options) {
  const state = start(portOf(options?.port))
  return { tool: tools(state) }
}

export default { id: "magpie-lamplighter", server }
