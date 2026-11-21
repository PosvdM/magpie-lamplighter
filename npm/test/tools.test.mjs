import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import test from "node:test"
import { fileURLToPath } from "node:url"
import { TOOLS, toolArguments } from "../tools.js"

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "..")
const server = fs.readFileSync(path.join(root, "internal", "mcp", "server.go"), "utf8")

// goTools reads the tool names and their arguments from internal/mcp: the
// list toolDefinitions walks and the switch of argumentNames.
function goTools() {
  const names = JSON.parse(`[${server.match(/for _, name := range \[\]string\{([^}]*)\}/)[1]}]`)
  const body = server.match(/func argumentNames\(name string\) \[\]string \{([\s\S]*?)\n\}/)[1]
  const args = {}
  let fallback
  for (const m of body.matchAll(/(case ((?:"[a-z_]+",?\s*)+)|default):\s*return \[\]string\{([^}]*)\}/g)) {
    const list = JSON.parse(`[${m[3]}]`)
    if (m[2]) for (const n of JSON.parse(`[${m[2]}]`)) args[n] = list
    else fallback = list
  }
  return Object.fromEntries(names.map((n) => [n, args[n] ?? fallback]))
}

test("the tools are internal/mcp's, with the same names and arguments", () => {
  const go = goTools()
  assert.deepEqual(Object.keys(TOOLS), Object.keys(go))
  for (const [name, args] of Object.entries(go)) assert.deepEqual(Object.keys(TOOLS[name].properties), args, name)
})

test("limit is an integer from 1 to 100, the other arguments strings", () => {
  for (const def of Object.values(TOOLS)) {
    for (const [arg, schema] of Object.entries(def.properties)) {
      if (arg === "limit") assert.deepEqual([schema.type, schema.minimum, schema.maximum], ["integer", 1, 100])
      else assert.equal(schema.type, "string")
      assert.ok(schema.description)
    }
  }
})

test("each description is internal/mcp's", () => {
  // the first sentence of each description is in server.go
  for (const [name, def] of Object.entries(TOOLS)) {
    const first = def.description.split(". ")[0]
    assert.ok(server.includes(first), `${name}: ${first}`)
  }
})

test("toolArguments keeps strings, writes limit as digits and leaves out absent ones", () => {
  assert.deepEqual(toolArguments("get_quota_events", { since: "x", limit: 10, other: null }), { since: "x", limit: "10" })
  assert.deepEqual(toolArguments("get_quota_events", { limit: "10" }), { limit: "10" })
  assert.deepEqual(toolArguments("list_quotas", undefined), {})
  assert.throws(() => toolArguments("list_quotas", { account: ["a"] }), /must be strings/)
  assert.throws(() => toolArguments("list_quotas", { limit: 5 }), /^Error: Unknown argument: limit$/)
  assert.throws(() => toolArguments("refresh_quotas", { provider: "codex" }), /Unknown argument: provider/)
})
