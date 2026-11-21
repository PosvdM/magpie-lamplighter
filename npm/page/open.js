// The plugin's page in Magpie's window only goes to Lamplighter's own page
// on the internal listener. Through `magpie web` the bridge gives the
// listener's address through Magpie (magpie.service); in the app's window
// it gives none, and the page goes to 127.0.0.1 on the plugin's port
// option, else the default port. See docs/architecture.md, "插件外壳".
;(() => {
  const DEFAULT_PORT = 8317
  const READY_WAIT = 3000

  const portOf = (v) => {
    const n = typeof v === "number" ? v : typeof v === "string" && /^\s*\d+\s*$/.test(v) ? Number(v) : NaN
    return Number.isInteger(n) && n >= 1 && n <= 65535 ? n : DEFAULT_PORT
  }
  const within = (p, ms) => Promise.race([p, new Promise((_, reject) => setTimeout(() => reject(new Error("timeout")), ms))])

  async function target(magpie) {
    if (!magpie) return `http://127.0.0.1:${DEFAULT_PORT}/`
    await within(magpie.ready, READY_WAIT)
    if (magpie.service) return magpie.service
    const options = await within(magpie.options.get(), READY_WAIT).catch(() => null)
    return `http://127.0.0.1:${portOf(options?.port)}/`
  }

  target(globalThis.magpie)
    .catch(() => `http://127.0.0.1:${DEFAULT_PORT}/`)
    .then((url) => location.replace(url))
})()
