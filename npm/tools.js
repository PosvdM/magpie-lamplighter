// The tools the plugin gives Magpie's agents: one for each tool of
// Lamplighter's own MCP server (toolDefinitions in internal/mcp/server.go),
// with the same names, arguments and descriptions. Each one runs through the
// internal listener's POST /api/tools/{name}. npm/test/tools.test.mjs checks
// the names and arguments against internal/mcp.

const ACCOUNT = "Exact returned account label (unique), base label before #n, original first note line, or full email; omitted means all. Shared base labels or note names select multiple accounts, which refresh rejects. Exact returned labels take priority over other matches."

const NOT_READY = " Returns ready=false, certain=false and state=starting with retry_after_seconds=30 instead of data until the engine runs and completes its first full poll; a credential-list failure counts as ready. A disabled plugin returns ready=false, certain=false and state=disabled instead of data. These are successful tool results, not errors."

const text = (description) => ({ type: "string", description })

export const TOOLS = {
  list_quotas: {
    description: "Read current quota windows grouped by provider and quota pool, weighted remaining totals and earliest account exhaustion. Per-account fields include remaining percent, reset/start state, percentage points consumed per hour, estimated exhaustion and pre-reset risk, seven-day even-pace budget, freshness/source/error/credential state (credential_invalid, account_suspended), next ignition, plan and weight. Time objects contain ISO 8601 with timezone and relative time. Null means unknown. Forecasts are estimates. Model filtering returns conservative candidate pools with certain=false because Magpie's aliases, routing rules and live account selection are not visible to this plugin. Accounts use the first note line with embedded emails masked, else *** plus the email user name's last two characters (or all characters if shorter; e.g. ***rk), else account-number. Duplicate labels receive unique #n suffixes; full emails are never returned." + NOT_READY,
    properties: {
      provider: text("Provider ID, e.g. codex or claude; omitted means all."),
      account: text(ACCOUNT),
      model: text("Magpie model ID or alias. Returns conservative candidates, never a definitive routing claim."),
    },
  },
  get_quota_history: {
    description: "Read downsampled quota history, including retired windows. At most 2000 points total and 200 per series. provider is a provider ID; account is an exact account label or email. window accepts 5h, 7d or a window ID. range accepts 1h, 3h, 6h, 12h, 24h, 26h, 4d, 8d, 15d, 1mo, 36d (default 24h). Each point has timestamp, relative time, remaining percent and active/passive source." + NOT_READY,
    properties: {
      provider: text("Provider ID, e.g. codex or claude; omitted means all."),
      account: text(ACCOUNT),
      window: text("Window kind (5h, 7d) or exact window ID."),
      range: text("Named history range; default 24h."),
    },
  },
  refresh_quotas: {
    description: "Request an active quota refresh for all accounts or one exact account label or email. Requires a running engine, but does not wait for its first full poll. A non-running engine returns ready=false, state=starting and retry_after_seconds=30; a disabled plugin returns state=disabled. These responses do not consume the refresh limit. Running-engine attempts are globally limited to once per 60 seconds, including refresh failures. Ambiguous account filters are rejected. Returns retry_after_seconds when throttled. Wait is bounded to 180 seconds; timeout does not imply upstream work was canceled. Read list_quotas afterward for per-account freshness and errors.",
    properties: {
      account: text(ACCOUNT),
    },
  },
  get_quota_events: {
    description: "Read recent quota reset, exhaustion, ignition success/failure and credential problem events, newest first. credential_invalid events carry reason auth, suspended or rate_limited and source ignite or query; credential events of an account without quota groups yet have an empty group. since is an RFC3339 timestamp with timezone; limit is an integer, 1-100 (default 50). In-memory retention is at most 200 events of all kinds. Account filters elsewhere use exact account labels or email, never credential paths." + NOT_READY,
    properties: {
      since: text("RFC3339 timestamp with timezone; omitted means all retained events."),
      limit: { type: "integer", minimum: 1, maximum: 100, description: "Maximum events, default 50." },
    },
  },
}

// toolArguments turns a tool call's arguments into the string map that
// /api/tools/{name} takes: absent and null ones left out, limit, an
// integer, as its decimal digits. An argument the tool does not take, or
// one other than limit that is no string, is refused, as Lamplighter's MCP
// server refuses it: /api/tools/{name} does not check the names.
export function toolArguments(name, args) {
  const out = {}
  const known = TOOLS[name]?.properties ?? {}
  for (const [key, value] of Object.entries(args ?? {})) {
    if (value === undefined || value === null) continue
    if (!Object.hasOwn(known, key)) throw new Error(`Unknown argument: ${key}`)
    if (key === "limit" && Number.isInteger(value)) out[key] = String(value)
    else if (typeof value === "string") out[key] = value
    else throw new Error(key === "limit" ? "limit must be an integer" : `${name}: arguments must be strings except limit`)
  }
  return out
}
