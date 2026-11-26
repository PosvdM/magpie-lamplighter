const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const source = fs.readFileSync(path.join(__dirname, "page.js"), "utf8");
// Load the actual pure functions without starting the DOM or polling timers.
function context(names, globals) {
  const code = names.map((name) => {
    const match = source.match(new RegExp("  function " + name + "\\([^]*?\\n  }"));
    assert.ok(match, name);
    return match[0];
  }).join("\n");
  const ctx = vm.createContext(globals || {});
  vm.runInContext(code, ctx);
  return ctx;
}

// CSS.supports("color", v) of a browser, for the color values the tests use.
const fakeCSS = { supports: (prop, v) => prop === "color" && /^(#[0-9a-f]{3,8}|(rgba?|hsla?|oklch|color-mix)\(.*\)|[a-z]+)$/i.test(v) };

test("a save counts as applied only by its own config token or a later one", () => {
  const ctx = context(["tokenApplied", "checkApplied"], { status: { config: { bark_url: "已设置" } } });
  const tokens = ["a1", "b2", "c3"];
  assert.equal(ctx.tokenApplied(tokens, "b2", "a1"), false);
  assert.equal(ctx.tokenApplied(tokens, "b2", "b2"), true);
  assert.equal(ctx.tokenApplied(tokens, "b2", "c3"), true);
  assert.equal(ctx.tokenApplied(tokens, "b2", ""), false);
  assert.equal(ctx.tokenApplied(tokens, "b2", undefined), false);
  // A token saved elsewhere, or a redacted value in the status, proves nothing.
  assert.equal(ctx.tokenApplied(tokens, "b2", "other"), false);
  assert.equal(ctx.tokenApplied(tokens, "zz", "c3"), false);

  const settled = [];
  ctx.savedTokens = tokens;
  const wait = (token) => {
    const w = { token, done: (v) => { settled.push([token, v]); ctx.applyWaits = ctx.applyWaits.filter((x) => x !== w); } };
    return w;
  };
  ctx.applyWaits = [wait("b2"), wait("c3")];
  ctx.status.config_applied_token = "b2";
  ctx.checkApplied();
  assert.deepEqual(settled, [["b2", true]]);
  assert.equal(ctx.applyWaits.length, 1);
  ctx.status.config_applied_token = "c3";
  ctx.checkApplied();
  assert.deepEqual(settled.at(-1), ["c3", true]);
  assert.equal(ctx.applyWaits.length, 0);
  assert.doesNotMatch(source, /function sameConfig|mcp_key_set/);
});

test("config tokens are 16 hexadecimal digits", () => {
  const ctx = context(["newConfigToken"], { crypto: require("node:crypto").webcrypto, Uint8Array });
  const a = ctx.newConfigToken(), b = ctx.newConfigToken();
  assert.match(a, /^[0-9a-f]{16}$/);
  assert.notEqual(a, b);
});

test("account names are saved in account_names by pin and clearing removes the entry", async () => {
  const writes = [];
  const ctx = context(["saveAccountName", "rereadConfig"], {
    rawConfig: { account_names: { "codex/b": "B" } }, status: { accounts: [{ auth_index: "codex/a" }] },
    API: "api",
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, api: async () => ({}), renderStatus: () => {}, loadHistory: () => {},
  });
  await ctx.saveAccountName({ auth_index: "codex/a" }, "A");
  assert.deepEqual(writes.pop(), { account_names: { "codex/b": "B", "codex/a": "A" } });
  assert.equal(ctx.status.accounts[0].note, "A");
  await ctx.saveAccountName({ auth_index: "codex/a" }, "");
  assert.deepEqual(writes.pop(), { account_names: { "codex/b": "B" } });
  ctx.rawConfig.account_names = { "codex/a": "A" };
  await ctx.saveAccountName({ auth_index: "codex/a" }, "");
  assert.deepEqual(writes.pop(), { account_names: null });
});

test("drag targets use layout midpoints rather than animated rectangles", () => {
  const ctx = context(["accountDropIndex"]);
  const rows = [0, 100, 200].map((offsetTop) => ({
    offsetTop, offsetHeight: 80,
    getBoundingClientRect: () => { throw new Error("animated geometry must not be read"); },
  }));
  assert.equal(ctx.accountDropIndex(rows, 49, 10), 0);
  assert.equal(ctx.accountDropIndex(rows, 51, 10), 1);
  assert.equal(ctx.accountDropIndex(rows, 150, 10), 1);
  assert.equal(ctx.accountDropIndex(rows, 151, 10), 2);
  assert.equal(ctx.accountDropIndex(rows, 999, 10), 3);
});

test("queued inline weights merge changes and an empty field removes only its own override", async () => {
  const states = [], writes = [];
  const ctx = context(["saveAccountField", "accountFieldHost", "fmtWeight", "rereadConfig"], {
    rawConfig: {}, saveQueue: Promise.resolve(), history: null,
    saveHost: () => ({}), saveEntry: (_, input) => ({ input }),
    translate: (key) => key, setSaveState: (...args) => states.push(args),
    claimState: () => () => {}, showApplied: () => {},
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, renderAccounts: () => {},
  });
  const input = (value) => ({ value, parentNode: { firstChild: {} }, accountSaved: "", accountEdit: 1, accountDirty: true, validity: { badInput: false }, hasAttribute: () => false, getAttribute: () => "weight" });
  const a = input("2.5"), b = input("7");
  ctx.saveAccountField(a, { account: { name: "a" } });
  ctx.saveAccountField(b, { account: { name: "b" } });
  await ctx.saveQueue;
  assert.deepEqual(plain(ctx.rawConfig.account_weights), { a: 2.5, b: 7 });
  a.value = ""; a.accountEdit++;
  ctx.saveAccountField(a, { account: { name: "a" } });
  await ctx.saveQueue;
  assert.deepEqual(plain(ctx.rawConfig.account_weights), { b: 7 });
  for (const bad of ["0", "-1", "Infinity", "NaN"]) {
    ctx.saveAccountField(input(bad), { account: { name: "a" } });
  }
  const invalidEmpty = input(""); invalidEmpty.validity.badInput = true;
  ctx.saveAccountField(invalidEmpty, { account: { name: "a" } });
  await ctx.saveQueue;
  assert.equal(writes.length, 3);
  assert.equal(states.length, 5);
});

test("an earlier account name save does not overwrite a newer unsaved edit", async () => {
  const ctx = context(["saveAccountField", "accountFieldHost", "fmtWeight", "rereadConfig"], {
    saveQueue: Promise.resolve(), history: null, saveHost: () => ({}), saveEntry: (_, input) => ({ input }),
    claimState: () => () => {}, translate: (key) => key, renderAccounts: () => {},
    saveAccountName: async () => {},
  });
  const input = { value: "first", parentNode: { firstChild: {} }, accountSaved: "old", accountEdit: 1, accountDirty: true, getAttribute: () => "name" };
  ctx.saveAccountField(input, { account: { name: "a", note: "old\nrest" } });
  input.value = "new unsaved draft"; input.accountEdit++;
  await ctx.saveQueue;
  assert.equal(input.value, "new unsaved draft");
  assert.equal(input.accountDirty, true);
  assert.equal(input.accountPending, 0);
});

// Minimal DOM doubles exercise the actual state aggregation and save queue.
function saveNode(tagName, attrs = {}) {
  return {
    tagName, id: attrs.id || "", name: attrs.name || "", children: [], attrs: { ...attrs },
    value: "", validity: { badInput: false }, accountSaved: "", accountEdit: 1, accountDirty: true,
    get textContent() { return (this.text || "") + this.children.map(n => n.textContent).join(""); },
    set textContent(value) { this.text = value; this.children = []; },
    getAttribute(name) { return this.attrs[name] ?? null; },
    hasAttribute(name) { return name in this.attrs; },
    setAttribute(name, value) { this.attrs[name] = value; },
    removeAttribute(name) { delete this.attrs[name]; },
    appendChild(node) { this.children.push(node); return node; },
    querySelector(selector) { return selector === ".save-state" ? this.children[0] : null; },
    closest() { return null; },
  };
}

const saveFunctions = ["saveHost", "saveControls", "saveItemKey", "saveEntry", "renderSaveState",
  "updateSaveEntry", "setSaveState", "claimState", "editSaveState"];
function stateContext(names = [], globals = {}) {
  return context([...saveFunctions, ...names], {
    saveSerial: 0,
    translate: (key, ...args) => {
      if (key === "save_field_failed") return "✕ " + args[0] + " save failed";
      if (key === "save_failure_reason") return ": " + args[0];
      if (key === "save_failed_compact") return "✕ Save failed";
      if (key === "save_more_failed") return " (" + args[0] + " more failed)";
      return key;
    },
    saveText: text => text,
    saveItemLabel: input => input.id === "account-order-state" ? "Order" : input.getAttribute("data-account-field") || input.name,
    el: (tag, attrs) => saveNode(tag.toUpperCase(), attrs),
    clear: node => { node.textContent = ""; },
    maskedNodes: text => [{ textContent: text }],
    ...globals,
  });
}

function accountSaveContext(fail) {
  const heading = saveNode("H2", { id: "account-order-state" });
  const section = { tagName: "SECTION", querySelector: selector => { assert.equal(selector, ".card-title"); return heading; } };
  const row = { account: { name: "a", note: "old" }, getAttribute: () => "a" };
  const ctx = stateContext(["saveAccountField", "accountFieldHost", "fmtWeight", "saveAccountOrder", "rereadConfig"], {
    fail, rawConfig: {}, saveQueue: Promise.resolve(), history: null, pendingOrder: null,
    $: () => heading, toast: () => {},
    showApplied: say => { say("waiting", "waiting"); say("saved", "applied"); },
    saveAccountName: async () => { if (ctx.fail === "name" || ctx.fail === "unauthorized") throw new Error(ctx.fail); },
    patchConfig: async patch => {
      if (ctx.fail === "weight" && patch.account_weights || ctx.fail === "order" && patch.account_order) throw new Error("failed");
      Object.assign(ctx.rawConfig, patch);
      return "token";
    },
    loadConfig: async () => {}, sortAccounts: () => {}, renderAccounts: () => {},
  });
  const field = (kind, value) => {
    const input = saveNode("INPUT", { "data-account-field": kind });
    input.value = value;
    input.closest = selector => selector === ".account-setting" ? row : selector === ".settings-section" ? section : null;
    return input;
  };
  return { ctx, heading, row, field, name: field("name", "unsaved name"), weight: field("weight", "2.5") };
}

test("a failed name save stays in the heading after successful weight and order saves", async () => {
  const { ctx, heading, row, name, weight } = accountSaveContext("name");
  ctx.saveAccountField(name, row);
  ctx.saveAccountField(weight, row);
  ctx.saveAccountOrder(["a"]);
  await ctx.saveQueue;
  assert.equal(heading.children.length, 1);
  assert.equal(heading.children[0].getAttribute("data-state"), "failed");
  assert.match(heading.textContent, /name save failed/);
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(name.value, "unsaved name");
  assert.equal(name.accountDirty, true);
  assert.equal(name.accountPending, 0);
  assert.equal(weight.accountDirty, false);
  ctx.fail = null;
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
  assert.doesNotMatch(heading.textContent, /failed/);
});

test("weight and order failures survive a successful name save", async () => {
  for (const fail of ["weight", "order"]) {
    const { ctx, heading, row, name, weight } = accountSaveContext(fail);
    if (fail === "weight") ctx.saveAccountField(weight, row);
    else ctx.saveAccountOrder(["a"]);
    ctx.saveAccountField(name, row);
    await ctx.saveQueue;
    assert.equal(heading.children[0].getAttribute("data-state"), "failed");
    assert.match(heading.textContent, fail === "weight" ? /weight save failed/ : /Order save failed/);
    if (fail === "weight") {
      assert.equal(weight.getAttribute("aria-invalid"), "true");
      assert.equal(weight.value, "2.5");
      assert.equal(weight.accountDirty, true);
    }
  }
});

test("reverting a failed account field to its saved value still retries and clears the failure", async () => {
  const { ctx, heading, row, weight } = accountSaveContext("weight");
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  ctx.fail = null;
  weight.value = weight.accountSaved;
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
  assert.equal(ctx.rawConfig.account_weights, null);
});

test("multiple failures show the latest reason and count, with every failed control marked", async () => {
  const { ctx, heading, row, name, weight } = accountSaveContext("unauthorized");
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  weight.value = "0";
  ctx.saveAccountField(weight, row);
  assert.match(heading.textContent, /weight save failed: weight_invalid \(1 more failed\)/);
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  ctx.editSaveState(heading, name);
  assert.match(heading.textContent, /1 more failed/);
  ctx.fail = null;
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  assert.doesNotMatch(heading.textContent, /more failed/);
  weight.value = "3";
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
});

test("all settings resolve to section headings or their own advanced legends", () => {
  const ctx = stateContext();
  for (const advanced of [false, true]) {
    for (const name of ["bark_url", "ignition.start_hour", "providers.codex.monitor", "remember_chart_filters", "mcp_enabled"]) {
      const heading = saveNode(advanced ? "LEGEND" : "H2");
      const group = { tagName: advanced ? "FIELDSET" : "SECTION", querySelector: selector => {
        assert.equal(selector, advanced ? "legend" : ".card-title"); return heading;
      } };
      const input = saveNode("INPUT", { name });
      input.closest = selector => selector === (advanced ? "fieldset" : ".settings-section") ? group : null;
      assert.equal(ctx.saveHost(input), heading);
      ctx.setSaveState(heading, "failed", "reason", input);
      assert.equal(heading.children.length, 1);
      assert.equal(heading.children[0].attrs.class, "save-state");
      assert.equal(input.children.length, 0);
      assert.equal(input.getAttribute("aria-invalid"), "true");
    }
  }
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /<form id="settings">\s*<section[^>]+>\s*<h2[^>]+id="account-order-state"/);
  assert.doesNotMatch(html, /data-save-in/);
});

test("ordinary settings keep A's failure through B's save and reject stale replies after editing", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  const a = saveNode("INPUT", { name: "a" }), b = saveNode("INPUT", { name: "b" });
  const sayA = ctx.claimState(heading, a);
  sayA("failed", "A failed");
  const sayB = ctx.claimState(heading, b);
  sayB("saved", "B saved");
  assert.match(heading.textContent, /a save failed: A failed/);
  assert.equal(a.getAttribute("aria-invalid"), "true");
  ctx.editSaveState(heading, a);
  assert.equal(sayA("saved", "late reply"), false);
  assert.match(heading.textContent, /A failed/);
  const retry = ctx.claimState(heading, a);
  assert.match(heading.textContent, /A failed/);
  retry("waiting", "saved, waiting");
  assert.equal(a.hasAttribute("aria-invalid"), false);
  retry("saved", "applied");
  assert.equal(heading.textContent, "applied");
});

test("an older confirmation cannot replace a more recent save's progress", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  const a = saveNode("INPUT", { name: "a" }), b = saveNode("INPUT", { name: "b" });
  const sayA = ctx.claimState(heading, a), sayB = ctx.claimState(heading, b);
  sayB("waiting", "B waiting");
  sayA("saved", "A applied");
  assert.equal(heading.textContent, "B waiting");
});

test("account cards keep only identity in the heading and place actions after the footer plan", () => {
  const cards = saveNode("DIV");
  const element = (tag, attrs = {}, children = []) => {
    const node = saveNode(tag.toUpperCase(), attrs);
    if (attrs.text) node.textContent = attrs.text;
    children.filter(Boolean).forEach(child => node.appendChild(child));
    return node;
  };
  const ctx = context(["renderAccounts", "hiddenGroups", "groupHidden"], {
    accountDrag: null, accountsBehind: false, SENSITIVE: "account-sensitive",
    status: { accounts: [
      { name: "a", provider: "codex", title: "GPT#first", email: "first@example.com", plan: "Pro", groups: [] },
      { name: "b", provider: "codex", title: "GPT#second", email: "second@example.com", groups: [] },
    ] },
    rawConfig: {}, masked: false, renderAccountSettings: () => {}, accountWeight: () => 2,
    fmtWeight: String, parseTime: () => null, $: () => cards,
    clear: node => { node.textContent = ""; }, el: element,
    translate: (key, value) => value === undefined ? key : key + ": " + value,
    maskedNodes: text => [element("span", { class: "account-sensitive", text })],
    snapshotButton: label => element("button", { class: "snapshot no-snapshot", "aria-label": label }),
  });
  ctx.renderAccounts();
  assert.equal(cards.children.length, 2);
  cards.children.forEach((card, i) => {
    const heading = card.children[0], footer = card.children.at(-1);
    assert.equal(heading.children.length, 2);
    assert.ok(heading.children.every(node => node.tagName === "SPAN"));
    assert.match(heading.children[0].attrs.class, /type-badge/);
    assert.match(heading.children[1].attrs.class, /account-email account-sensitive/);
    assert.match(heading.children[0].children.at(-1).attrs.class, /account-weight/);
    assert.equal(footer.attrs.class, "account-head cell-note");
    assert.equal(footer.children[0].textContent, i ? "" : "plan: Pro");
    assert.equal(footer.children[1].textContent, "refresh");
    assert.equal(footer.children[2].attrs["aria-label"], "snapshot_card");
    footer.children.slice(1).forEach(button => {
      assert.equal(button.tagName, "BUTTON");
      assert.match(button.attrs.class, /no-snapshot/);
    });
  });
  assert.match(source, /copy.querySelectorAll\("\.no-snapshot,/);
});

test("cards leave out hidden groups and groups without windows", () => {
  const cards = saveNode("DIV");
  const element = (tag, attrs = {}, children = []) => {
    const node = saveNode(tag.toUpperCase(), attrs);
    if (attrs.text) node.textContent = attrs.text;
    children.filter(Boolean).forEach(child => node.appendChild(child));
    return node;
  };
  const win = [{ id: "five-hour", kind: "5h" }];
  const retired = { key: "antigravity:1:old", label: "", source_label: "", windows: null };
  const ctx = context(["renderAccounts", "hiddenGroups", "hiddenList", "groupHidden"], {
    accountDrag: null, accountsBehind: false, SENSITIVE: "account-sensitive",
    status: { config: { hidden_groups: ["antigravity|Claude / GPT"] }, accounts: [
      { name: "a", provider: "antigravity", title: "Antigravity", groups: [
        { key: "antigravity:1:gemini", label: "Gemini", source_label: "Gemini", windows: win },
        { key: "antigravity:1:cg", label: "Claude / GPT", source_label: "Claude / GPT", windows: win },
        retired,
      ] },
      // Only a hidden group and a retired one: no card.
      { name: "b", provider: "antigravity", title: "Antigravity", groups: [
        { key: "antigravity:2:cg", label: "Claude / GPT", source_label: "Claude / GPT", windows: win },
        retired,
      ] },
      // Only a retired group: the card stays, without groups.
      { name: "c", provider: "antigravity", title: "Antigravity", error: "failed", groups: [retired] },
    ] },
    rawConfig: {}, masked: false, renderAccountSettings: () => {}, accountWeight: () => 1,
    fmtWeight: String, parseTime: () => null, $: () => cards,
    clear: node => { node.textContent = ""; }, el: element,
    translate: (key, value) => value === undefined ? key : key + ": " + value,
    maskedNodes: text => [element("span", { text })], providerTitle: () => "Antigravity",
    quotaRow: () => element("div", { class: "quota-row" }), freshness: () => null,
    snapshotButton: label => element("button", { "aria-label": label }),
  });
  ctx.renderAccounts();
  assert.deepEqual(cards.children.map(card => card.attrs["data-account"]), ["a", "c"]);
  const blocks = card => card.children.filter(node => node.attrs.class === "quota-group");
  assert.deepEqual(blocks(cards.children[0]).map(block => block.children[0].textContent), ["Gemini"]);
  assert.equal(blocks(cards.children[1]).length, 0);
  assert.deepEqual(cards.children[1].children.filter(node => node.attrs.class === "error-text").map(node => node.textContent), ["failed"]);
});

test("ignition group choices skip empty names from accounts and the config", () => {
  const ctx = context(["groupChoices"], {
    status: { accounts: [{ provider: "antigravity", groups: [
      { source_label: "Gemini" }, { source_label: "" }, { source_label: "Extra" },
    ] }] },
  });
  assert.deepEqual(plain(ctx.groupChoices({ id: "antigravity", groups: ["Gemini", "Claude / GPT"] }, ["", "Old"])),
    ["Gemini", "Claude / GPT", "Extra", "Old"]);
});

function settingsSaveContext(radio = false) {
  const a = saveNode("INPUT", { name: radio ? "recovery_notify.five_hour" : "ignition.start_hour" });
  const b = saveNode("INPUT", { name: radio ? "recovery_notify.five_hour" : "ignition.end_hour" });
  const inputs = [a, b], applied = [], refilled = [];
  inputs.forEach((input, i) => { input.type = "number"; input.value = String(i ? 22 : 7); input.checkValidity = () => true; });
  if (radio) inputs.forEach((input, i) => {
    input.type = "radio"; input.value = i ? "all" : "off"; input.checked = !i;
    input.closest = selector => selector === ".thresholds, .mode-switch" ? { querySelectorAll: () => inputs } : null;
  });
  const ctx = context(["saveField", "captureIntent", "buildPatch", "refillSaved", "noteEdit", "inputTop", "topKey", "setPath", "saveControls", "rereadConfig"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(),
    rawConfig: { ignition: {} }, status: { config: {} },
    saveHost: () => ({}), claimState: () => () => true,
    patchConfig: async patch => { Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {},
    showApplied: () => new Promise(resolve => applied.push(resolve)),
    fillSettings: keys => refilled.push(plain(keys)), renderIgnitionOff: () => {},
  });
  const edit = (input, value) => { input.value = value; input.settingsDirty = true; ctx.noteEdit(input); };
  return { ctx, a, b, applied, refilled, edit };
}

test("consecutive saves within one top-level key clear both submitted drafts independently", async () => {
  const { ctx, a, b, applied, refilled, edit } = settingsSaveContext();
  edit(a, "8"); ctx.saveField({ target: a });
  edit(b, "23"); ctx.saveField({ target: b });
  await ctx.saveQueue;
  assert.equal(applied.length, 2);
  applied[0](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, true);
  applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, false);
  assert.equal(refilled.length, 2);
});

test("confirmation preserves newer drafts and independently failed controls of the same key", async () => {
  const { ctx, a, b, applied, edit } = settingsSaveContext();
  edit(a, "8"); ctx.saveField({ target: a });
  edit(b, "23"); ctx.saveField({ target: b });
  await ctx.saveQueue;
  edit(a, "9");
  applied[0](true); applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, true);
  assert.equal(a.value, "9");
  assert.equal(b.settingsDirty, false);
  b.settingsDirty = true;
  b.setAttribute("aria-invalid", "true");
  edit(a, "10"); ctx.saveField({ target: a });
  await ctx.saveQueue;
  applied[2](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, true);
  assert.equal(b.getAttribute("aria-invalid"), "true");
});

test("a radio selection owns the generations and submitted state of the whole group", async () => {
  const { ctx, a, b, applied } = settingsSaveContext(true);
  a.settingsDirty = true; ctx.noteEdit(a); ctx.saveField({ target: a });
  a.checked = false; b.checked = true;
  b.settingsDirty = true; ctx.noteEdit(b); ctx.saveField({ target: b });
  await ctx.saveQueue;
  applied[0](true); await Promise.resolve();
  assert.equal(a.settingsDirty, true);
  assert.equal(b.settingsDirty, true);
  applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, false);
  assert.equal(a.checked, false);
  assert.equal(b.checked, true);
});

test("fields skipped by patch construction keep their drafts after confirmation", () => {
  const { ctx, a, b, edit } = settingsSaveContext();
  edit(a, ""); edit(b, "23");
  const intent = ctx.captureIntent(["ignition"]);
  ctx.buildPatch("ignition", intent);
  assert.equal(intent.submitted.includes(a), false);
  ctx.refillSaved(intent);
  assert.equal(a.settingsDirty, true);
  assert.equal(b.settingsDirty, false);
});

test("recreated account inputs inherit drafts and failures and receive later save results", async () => {
  const { ctx, heading, row, field, name } = accountSaveContext("name");
  ctx.saveAccountField(name, row); await ctx.saveQueue;
  const replacement = field("name", "server value");
  ctx.saveEntry(heading, replacement);
  const entry = heading.saveEntries.get("a:name");
  assert.equal(entry.input, replacement);
  assert.equal(entry.controls[0], replacement);
  assert.equal(replacement.value, "unsaved name");
  assert.equal(replacement.getAttribute("aria-invalid"), "true");
  ctx.saveAccountField(replacement, row); await ctx.saveQueue;
  assert.equal(replacement.getAttribute("aria-invalid"), "true");
  ctx.fail = null;
  ctx.saveAccountField(replacement, row); await ctx.saveQueue;
  assert.equal(replacement.hasAttribute("aria-invalid"), false);
  assert.equal(replacement.accountDirty, false);
});

test("pending account saves settle against the current binding after the row is recreated", async () => {
  const { ctx, heading, row, field, name } = accountSaveContext();
  let finish;
  ctx.saveAccountName = () => new Promise(resolve => { finish = resolve; });
  ctx.saveAccountField(name, row);
  await Promise.resolve();
  const replacement = field("name", "server value");
  ctx.saveEntry(heading, replacement);
  assert.equal(replacement.accountPending, 1);
  finish(); await ctx.saveQueue;
  assert.equal(replacement.value, "unsaved name");
  assert.equal(replacement.accountPending, 0);
  assert.equal(replacement.accountDirty, false);
});

test("failed states separate the compact summary, field summary, reason and count", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  ctx.setSaveState(heading, "failed", "first reason", saveNode("INPUT", { name: "first" }));
  ctx.setSaveState(heading, "failed", "second reason", saveNode("INPUT", { name: "long field name" }));
  const parts = Object.fromEntries(heading.children[0].children.map(n => [n.attrs["data-save-part"], n.textContent]));
  assert.equal(parts.compact, "✕ Save failed");
  assert.equal(parts.summary, "✕ long field name save failed");
  assert.equal(parts.reason, ": second reason");
  assert.equal(parts.count, " (1 more failed)");
  const css = fs.readFileSync(path.join(__dirname, "page.css"), "utf8");
  assert.match(css, /@media \(max-width: 640px\)/);
  assert.match(css, /\.save-state\[data-state="failed"\] > \[data-save-part="summary"\], \.save-state > \[data-save-part="reason"\] { display: none; }/);
});

test("saved messages and dynamic failure reasons follow language changes", () => {
  const code = source.slice(source.indexOf("  var I18N ="), source.indexOf("\n  function translate("));
  assert.match(code, /save_field_failed/);
  const globals = vm.createContext({});
  vm.runInContext(code, globals);
  globals.translate = (key, value) => globals.I18N.en[key].replace("{0}", value);
  const ctx = context(["saveText"], globals);
  assert.equal(ctx.saveText(globals.I18N.zh.save_applied), globals.I18N.en.save_applied);
  assert.equal(ctx.saveText(globals.I18N.zh.weight_invalid), globals.I18N.en.weight_invalid);
  assert.equal(ctx.saveText(globals.I18N.zh.mcp_error.replace("{0}", "bind failed")), globals.I18N.en.mcp_error.replace("{0}", "bind failed"));
  assert.equal(ctx.saveText("HTTP 500 upstream error"), "HTTP 500 upstream error");
});

const HOUR = 3600, DAY = 86400, WEEK = 7 * DAY;
function pace() { return context(["paceSegments", "paceAt", "pooledPace"], { RESET_DRIFT: 60 }); }
// Arrays from the VM context have another prototype.
const plain = (value) => JSON.parse(JSON.stringify(value));

test("an early reset cuts the even pace where the new reset appears", () => {
  const ctx = pace();
  const t0 = 1000 * DAY, x1 = t0 + 5 * DAY, early = t0 + 3 * DAY, x2 = early + WEEK;
  const segs = ctx.paceSegments([[t0 - 3 * DAY, x1], [early, x2]], WEEK, x2, early + DAY);
  assert.equal(segs.length, 2);
  // The old line starts where its window began and stops at the early reset
  // at the share an even pace leaves there, not at 0%.
  assert.deepEqual(plain(segs[0]), [x1 - WEEK, early, x1, false]);
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early - 1) - 100 * 2 / 7) < 0.01);
  // From the early reset on, the new window runs from 100% over a full week.
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early) - 100) < 1e-9);
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early + DAY) - 100 * 6 / 7) < 1e-9);
  assert.equal(ctx.paceAt(segs, WEEK, x2 + 1), null);
});

test("reset times that drift by seconds keep one continuous line", () => {
  const ctx = pace();
  const x = 2000 * DAY, t0 = x - 4 * HOUR, t1 = x - 3 * HOUR;
  const segs = ctx.paceSegments([[t0, x], [t1, x + 2]], 5 * HOUR, x + 30, t1 + 600);
  // The status reset within RESET_DRIFT of the history adds no segment.
  assert.equal(segs.length, 2);
  const before = ctx.paceAt(segs, 5 * HOUR, t1 - 1e-6), after = ctx.paceAt(segs, 5 * HOUR, t1);
  assert.ok(Math.abs(before - after) < 0.05, before + " " + after);
  assert.ok(Math.abs(after - 60) < 0.05);
});

test("no even pace while the reset is unknown, and a new status reset applies from now", () => {
  const ctx = pace();
  const L = 5 * HOUR, x = 3000 * DAY, now = x + 2 * HOUR;
  const segs = ctx.paceSegments([[x - 4 * HOUR, x], [x - HOUR, 0]], L, x + 4 * HOUR, now);
  assert.equal(ctx.paceAt(segs, L, x - 2 * HOUR) > 0, true);
  assert.equal(ctx.paceAt(segs, L, x - 30 * 60), null);
  // From the moment the reset is unknown, not only after it.
  assert.equal(ctx.paceAt(segs, L, x - HOUR), null);
  assert.ok(ctx.paceAt(segs, L, x - HOUR - 1) > 0);
  assert.equal(ctx.paceAt(segs, L, now - 60), null);
  assert.ok(Math.abs(ctx.paceAt(segs, L, now) - 40) < 1e-9);
  assert.ok(Math.abs(ctx.paceAt(segs, L, x + 3 * HOUR) - 20) < 1e-9);
  assert.deepEqual(plain(ctx.paceSegments([], L, 0, now)), []);
  assert.deepEqual(plain(ctx.paceSegments(undefined, L, null, now)), []);
  assert.deepEqual(plain(ctx.paceSegments([[now - HOUR, now + HOUR]], 0, null, now)), []);
});

test("a total has an even pace only while every account has one", () => {
  const ctx = pace();
  const L = WEEK, now = 4000 * DAY;
  const a = { weight: 1, segments: ctx.paceSegments([[now - 2 * DAY, now + 5 * DAY]], L, null, now) };
  const b = { weight: 3, segments: ctx.paceSegments([[now - DAY, now + 6 * DAY]], L, null, now) };
  assert.ok(Math.abs(ctx.pooledPace([a, b], L, now) - (100 * 5 / 7 + 3 * 100 * 6 / 7) / 4) < 1e-9);
  // Before b's line starts, the total is left empty rather than a's alone.
  assert.equal(ctx.pooledPace([a, b], L, now - 1.5 * DAY), null);
  assert.equal(ctx.pooledPace([], L, now), null);
});

test("normal saves use the backend URL defaults and generate a key when enabling MCP", () => {
  let inputs = [];
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: {}, status: { config: {} },
    $: () => ({ elements: inputs }),
    newMcpKey: () => "llmcp_generated",
  });
  function patch(name, value, type) {
    inputs = [{ name, value, type: type || "text", checked: value, getAttribute: () => null }];
    return JSON.parse(JSON.stringify(ctx.buildPatch(name)));
  }
  assert.deepEqual(patch("public_listen", ""), { public_listen: "127.0.0.1:8318" });
  assert.deepEqual(patch("bark_url", "https://api.day.app/test/"), { bark_url: "https://api.day.app/test" });
  ctx.rawConfig.mcp_key = "  ";
  assert.deepEqual(patch("mcp_enabled", true, "checkbox"), { mcp_enabled: true, mcp_key: "llmcp_generated" });
});

test("reset reminder hours accept commas and spaces and save sorted without duplicates", () => {
  const ctx = context(["parseHours", "parseValue", "formatValue"], {
    REMINDER_HOURS: { five_hour: { min: 0.5, max: 4.5, invalid: "hours_invalid_5h" }, seven_day: { min: 1, max: 167, invalid: "hours_invalid_7d" } },
    REMINDER_HOURS_MAX: 3,
    translate: (key) => key,
  });
  const parse = (text, name) => plain(ctx.parseValue("hours", text, "reset_reminder_hours." + name));
  const fails = (text, name, key) => assert.throws(() => ctx.parseValue("hours", text, "reset_reminder_hours." + name), { message: key });
  assert.deepEqual(parse("10, 24", "seven_day"), [24, 10]);
  assert.deepEqual(parse(" 1，0.5 2 ", "five_hour"), [2, 1, 0.5]);
  assert.deepEqual(parse("24,24, 24.0", "seven_day"), [24]);
  assert.deepEqual(parse("4.5,.5", "five_hour"), [4.5, 0.5]);
  fails("", "five_hour", "hours_empty");
  fails(" , ，", "seven_day", "hours_empty");
  fails("5", "five_hour", "hours_invalid_5h");
  fails("0.4", "five_hour", "hours_invalid_5h");
  fails("168", "seven_day", "hours_invalid_7d");
  fails("1h", "seven_day", "hours_invalid_7d");
  fails("-2", "seven_day", "hours_invalid_7d");
  fails("1e2", "seven_day", "hours_invalid_7d");
  fails("1, 2, 3, 4", "seven_day", "hours_too_many");
  assert.equal(ctx.formatValue("hours", [24, 10]), "24, 10");
  assert.equal(ctx.formatValue("hours", undefined), "");
});

test("reset reminder hours save both windows, keeping the effective value of one that does not parse", () => {
  let inputs = [];
  const ctx = context(["topKey", "setPath", "getPath", "buildPatch", "parseHours", "parseValue"], {
    rawConfig: {}, status: { config: { reset_reminder_hours: { five_hour: [1], seven_day: [24] } } },
    $: () => ({ elements: inputs }),
    REMINDER_HOURS: { five_hour: { min: 0.5, max: 4.5, invalid: "hours_invalid_5h" }, seven_day: { min: 1, max: 167, invalid: "hours_invalid_7d" } },
    REMINDER_HOURS_MAX: 3,
    translate: (key) => key,
  });
  const field = (name, value) => ({ name: "reset_reminder_hours." + name, value, type: "text", getAttribute: (a) => a === "data-format" ? "hours" : null });
  inputs = [field("five_hour", "1"), field("seven_day", "10 24")];
  assert.deepEqual(plain(ctx.buildPatch("reset_reminder_hours")), { reset_reminder_hours: { five_hour: [1], seven_day: [24, 10] } });
  inputs = [field("five_hour", "x"), field("seven_day", "48")];
  assert.deepEqual(plain(ctx.buildPatch("reset_reminder_hours")), { reset_reminder_hours: { five_hour: [1], seven_day: [48] } });
  ctx.rawConfig.reset_reminder_hours = { five_hour: [2], seven_day: [24] };
  assert.deepEqual(plain(ctx.buildPatch("reset_reminder_hours")), { reset_reminder_hours: { five_hour: [2], seven_day: [48] } });
});

test("threshold validation accepts equal levels and rejects reversed levels", () => {
  const elements = {};
  const ctx = context(["thresholdsInOrder"], {
    THRESHOLDS: ["notice_threshold", "low_threshold", "critical_threshold"],
    $: () => ({ elements }),
  });
  function valid(values) {
    ctx.THRESHOLDS.forEach((name, i) => { elements[name] = { value: String(values[i]) }; });
    return ctx.thresholdsInOrder();
  }
  assert.equal(valid([20, 20, 10]), true);
  assert.equal(valid([0, 0, 0]), true);
  assert.equal(valid([10, 20, 10]), false);
});

test("five color levels use raw percentages before display rounding", () => {
  const ctx = context(["quotaThresholds", "quotaLevel"], { LEVEL_EPSILON: 1e-6, status: { config: { notice_threshold: 50, low_threshold: 20 } } });
  assert.equal(ctx.quotaLevel(100), "full");
  assert.equal(ctx.quotaLevel(99.6), "high");
  assert.equal(ctx.quotaLevel(50), "medium");
  assert.equal(ctx.quotaLevel(20), "low");
  assert.equal(ctx.quotaLevel(0), "empty");
});

test("window translations have no duplicate definitions", () => {
  assert.equal((source.match(/window_5h:/g) || []).length, 2);
  assert.equal((source.match(/window_7d:/g) || []).length, 2);
  assert.match(source, /window_5h: "5-hour window"/);
});

test("a queued save keeps the value the user chose, whatever the field shows later", () => {
  const input = { name: "mcp_enabled", value: "on", type: "checkbox", checked: false, getAttribute: () => null };
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: { mcp_key: "llmcp_x" }, status: { config: {} },
    $: () => ({ elements: [input] }),
    newMcpKey: () => "llmcp_generated",
  });
  // The user turned MCP off; a late confirmation of an earlier save then
  // refilled the switch as on.
  const intent = { inputs: [input], states: [{ value: "on", checked: false }] };
  input.checked = true;
  assert.deepEqual(plain(ctx.buildPatch("mcp_enabled", intent)), { mcp_enabled: false });
  assert.deepEqual(plain(ctx.buildPatch("mcp_enabled")), { mcp_enabled: true });
});

test("the confirmation deadline ends a wait even when no status arrives", async () => {
  const ctx = context(["tokenApplied", "whenApplied", "checkApplied"], {
    APPLY_TIMEOUT: 40, setTimeout, clearTimeout, Promise,
    savedTokens: ["a1", "b2"], applyWaits: [], status: { config_applied_token: "old" },
    // The status requests hang: nothing calls checkApplied.
    pollApplied: () => {},
  });
  const started = Date.now();
  assert.equal(await ctx.whenApplied("a1"), false);
  assert.ok(Date.now() - started < 1000);
  assert.equal(ctx.applyWaits.length, 0);
  // The token reported after the deadline settles nothing.
  ctx.status.config_applied_token = "a1";
  ctx.checkApplied();
  assert.equal(ctx.applyWaits.length, 0);

  ctx.status.config_applied_token = "old";
  const pending = ctx.whenApplied("b2");
  assert.equal(ctx.applyWaits.length, 1);
  ctx.status.config_applied_token = "b2";
  ctx.checkApplied();
  assert.equal(await pending, true);
  assert.equal(ctx.applyWaits.length, 0);
  assert.equal(await ctx.whenApplied("a1"), true);
});

test("a current window whose reset became unknown ends the even pace now", () => {
  const ctx = pace();
  const L = WEEK, now = 5000 * DAY, x = now + 3 * DAY;
  const history = [[now - 2 * DAY, x]];
  // Reset unknown in the status: the line stops now, including the future.
  let segs = ctx.paceSegments(history, L, 0, now);
  assert.ok(ctx.paceAt(segs, L, now - 1) > 0);
  assert.equal(ctx.paceAt(segs, L, now), null);
  assert.equal(ctx.paceAt(segs, L, now + HOUR), null);
  assert.equal(ctx.paceAt(segs, L, x - 1), null);
  // Without the window in the status, the history holds.
  segs = ctx.paceSegments(history, L, null, now);
  assert.ok(ctx.paceAt(segs, L, now + HOUR) > 0);
  segs = ctx.paceSegments(history, L, undefined, now);
  assert.ok(ctx.paceAt(segs, L, now + HOUR) > 0);
  // Already unknown in the history: nothing more to add.
  assert.deepEqual(plain(ctx.paceSegments([[now - DAY, 0]], L, 0, now)), []);
  // A total breaks with the account whose reset became unknown.
  const a = { weight: 1, segments: ctx.paceSegments(history, L, 0, now) };
  const b = { weight: 1, segments: ctx.paceSegments([[now - DAY, now + 6 * DAY]], L, now + 6 * DAY, now) };
  assert.ok(ctx.pooledPace([a, b], L, now - 1) > 0);
  assert.equal(ctx.pooledPace([a, b], L, now), null);
  assert.equal(ctx.pooledPace([a, b], L, now + HOUR), null);
});

// ---- Events ----

// A var statement of page.js, such as the NOTIFY_TRIGGERS table.
function pageVar(name) {
  const match = source.match(new RegExp("\\n  var " + name + " = [^]*?;\\n"));
  assert.ok(match, name);
  return match[0];
}

const EVENT_FUNCTIONS = ["groupLabel", "retryText", "cutText", "errorSummary", "notifyKind", "eventWindow", "sameSubject",
  "eventSeconds", "eventRows", "notifyTitle", "eventView", "eventParts", "eventMasked", "pushMarks", "parseTime",
  "sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName", "groupTitle", "eventTitle"];

// Elements and text nodes as plain objects; translate shows the key and
// its arguments.
function eventContext(globals = {}) {
  const ctx = context(EVENT_FUNCTIONS, {
    status: { accounts: [] }, masked: false, SENSITIVE: "account-sensitive",
    providerTitle: (id) => ({ antigravity: "Antigravity", claude: "Claude", codex: "GPT" })[id] || id,
    translate: (key, ...args) => args.length ? key + "(" + args.join("|") + ")" : key,
    fmtDateTime: (t) => t ? t.toISOString().slice(5, 16) : "—",
    pct: (v) => v == null || isNaN(v) ? "—" : Math.round(v) + "%",
    el: (tag, attrs = {}, children = []) => ({ tag, ...attrs, children }),
    document: { createTextNode: (text) => ({ text }) },
    maskedNodes: (text, extra) => {
      // Like page.js: each listed name in its own sensitive span.
      let rest = String(text), out = [];
      const names = (extra || []).filter((n) => n && n.trim()).sort((a, b) => b.length - a.length);
      while (rest) {
        let at = -1, hit = "";
        for (const n of names) { const i = rest.indexOf(n); if (i >= 0 && (at < 0 || i < at)) { at = i; hit = n; } }
        if (at < 0) { out.push({ text: rest }); break; }
        if (at) out.push({ text: rest.slice(0, at) });
        out.push({ tag: "span", class: "account-sensitive", text: hit });
        rest = rest.slice(at + hit.length);
      }
      return out;
    },
    ...globals,
  });
  vm.runInContext(["EVENT_TYPES", "NOTIFY_TRIGGERS", "NOTIFY_LAG", "PAUSE_PAIR", "ERROR_CHARS", "ACCOUNT_EVENTS", "ACCOUNT_NOTIFY_KINDS", "SERVICE_SHORT"].map(pageVar).join("\n"), ctx);
  return ctx;
}

const T0 = Date.UTC(2026, 10, 16, 0, 0, 0) / 1000;
function event(kind, at, fields = {}) {
  return { time: new Date((T0 + at) * 1000).toISOString(), kind, level: "info", ...fields };
}

test("a notification joins the row of the event it reports", () => {
  const ctx = eventContext();
  const g = { group: "codex:a", label: "GPT" };
  const events = [
    event("notify", 70, { ...g, params: { title: "✅ GPT · 7d 已恢复", notify_kind: "recovery", windows: "seven-day" } }),
    event("reset", 60, { ...g, params: { window: "seven-day", kind: "7d", label: "7d", remaining: "100.00" } }),
    event("notify_failed", 40, { ...g, level: "warn", params: { title: "⚠️ GPT 点火已暂停", notify_kind: "circuit", error: "bark: 500" } }),
    event("notify", 39, { ...g, params: { title: "⚠️ GPT 点火已暂停", notify_kind: "circuit" } }),
    event("ignite_paused", 30, { ...g, level: "error", params: { until: "2026-11-17T00:00:00Z", error: "boom", summary: "boom" } }),
    // No event before it: a quota alert stays a row of its own.
    event("notify", 20, { ...g, params: { title: "🟡 GPT · 5h 45%", notify_kind: "quota" } }),
    // Too late for the reset of another group.
    event("notify", 10, { group: "codex:b", label: "GPT 2", params: { title: "✅ GPT 2 · 7d 已恢复", notify_kind: "recovery" } }),
  ];
  const built = ctx.eventRows(events, false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.kind + "@" + (r.event.params.title || ""))), [
    "reset@", "ignite_paused@", "notify@🟡 GPT · 5h 45%", "notify@✅ GPT 2 · 7d 已恢复"]);
  assert.deepEqual(plain(built.rows[0].pushes.map((ev) => ev.kind)), ["notify"]);
  assert.deepEqual(plain(built.rows[1].pushes.map((ev) => ev.kind).sort()), ["notify", "notify_failed"]);
  const marks = ctx.pushMarks(built.rows[1].pushes);
  assert.deepEqual(plain(marks.map((m) => m.text)), ["ev_notify", "ev_notify_failed"]);
  assert.match(marks[1].title, /bark: 500/);
  assert.equal(marks[1].class, "event-mark failed");
  // A standalone quota alert has its own badge, without the repeated label.
  const view = ctx.eventView(built.rows[2].event, built.rows[2].notifyKind);
  assert.equal(view.badge, "ev_notify_quota");
  assert.equal(view.detail, "🟡 5h 45%");
  // An unmatched recovery falls back to the plain badge.
  assert.equal(ctx.eventView(built.rows[3].event, built.rows[3].notifyKind).badge, "ev_notify");
});

test("matching needs the same group, the order, the time limit and the windows", () => {
  const ctx = eventContext();
  const g = { group: "k", label: "Gemini" };
  const reset = (at, window) => event("reset", at, { ...g, params: { window, remaining: "100.00" } });
  const recovery = (at, windows) => event("notify", at, { ...g, params: { title: "✅ Gemini · 7d 已恢复", notify_kind: "recovery", windows } });
  // Sent before the event, or more than 70 minutes after it, as retries
  // can be: no match.
  assert.equal(ctx.eventRows([reset(100, "gemini-weekly"), recovery(99, "gemini-weekly")], true).rows.length, 2);
  assert.equal(ctx.eventRows([recovery(4301, "gemini-weekly"), reset(100, "gemini-weekly")], true).rows.length, 2);
  assert.equal(ctx.eventRows([recovery(4300, "gemini-weekly"), reset(100, "gemini-weekly")], true).rows.length, 1);
  // The window must be one the notification names.
  let built = ctx.eventRows([recovery(110, "gemini-weekly"), reset(100, "gemini-5h")], true);
  assert.equal(built.rows.length, 2);
  // A notification for two windows marks both resets.
  built = ctx.eventRows([recovery(110, "gemini-5h,gemini-weekly"), reset(100, "gemini-5h"), reset(100, "gemini-weekly")], true);
  assert.equal(built.rows.length, 2);
  assert.ok(built.rows.every((r) => r.pushes.length === 1));
  // Hidden 5-hour resets take their notifications with them.
  built = ctx.eventRows([recovery(110, "gemini-5h"), reset(100, "gemini-5h"), reset(50, "gemini-weekly")], false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.params.window)), ["gemini-weekly"]);
  assert.equal(built.hidden, 1);
  assert.equal(built.fiveHour, 1);
});

test("older notifications are told by their titles", () => {
  const ctx = eventContext();
  const kind = (title, kindName = "notify") => ctx.notifyKind({ kind: kindName, params: { title } });
  assert.equal(kind("✅ GPT · 7d 已恢复"), "recovery");
  assert.equal(kind("✅ GPT · 7d recovered"), "recovery");
  assert.equal(kind("⚠️ Gemini 点火已暂停"), "circuit");
  assert.equal(kind("⚠️ Gemini ignition paused"), "circuit");
  assert.equal(kind("🎫 GPT · 套餐 plus → free"), "plan");
  assert.equal(kind("🟡 GPT · 5h 45%"), "quota");
  assert.equal(kind("🔴 GPT · 5h 5%"), "quota");
  assert.equal(kind("⏰ GPT · 5h 重置提醒"), "reminder");
  // Codex reset signals also start with ✅ but report no reset of an account.
  assert.equal(kind("✅ Codex 全局重置已完成"), "codex_reset");
  assert.equal(kind("🕯️ Lamplighter 测试通知"), "");
  // Failed notifications recorded without params.
  assert.equal(ctx.notifyKind({ kind: "notify_failed", detail: "✅ GPT · 7d 已恢复：timeout" }), "recovery");
  assert.equal(ctx.notifyKind({ kind: "notify", params: { title: "x", notify_kind: "suspended" } }), "suspended");
});

test("resets and exhaustion name the window by its short label", () => {
  const ctx = eventContext();
  assert.deepEqual(plain(ctx.eventWindow({ window: "five-hour" })), { kind: "5h", label: "5h" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "gemini-5h" })), { kind: "5h", label: "5h" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day" })), { kind: "7d", label: "7d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "3p-weekly" })), { kind: "7d", label: "7d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day-opus" })), { kind: "", label: "seven-day-opus" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day-opus", kind: "", label: "Opus 7d" })), { kind: "", label: "Opus 7d" });
  let view = ctx.eventView(event("reset", 0, { label: "GPT", params: { window: "seven-day", remaining: "100.00", reset: "2026-11-23T00:00:00Z" } }));
  assert.equal(view.badge, "ev_reset");
  assert.equal(view.style, "success");
  assert.equal(view.detail, "ev_window_next(7d|100%|11-23T00:00)");
  view = ctx.eventView(event("exhausted", 0, { label: "GPT", params: { window: "five-hour", kind: "5h", label: "5h", remaining: "0.00", reset: "2026-11-16T05:00:00Z" } }));
  assert.equal(view.badge, "ev_exhausted");
  assert.equal(view.detail, "ev_window_due(5h|0%|11-16T05:00)");
  view = ctx.eventView(event("reset", 0, { label: "GPT", params: { window: "seven-day", remaining: "100.00" } }));
  assert.equal(view.detail, "ev_window_left(7d|100%)");
  // Exhaustion of a 5-hour window always shows.
  const built = ctx.eventRows([event("exhausted", 0, { params: { window: "five-hour", remaining: "0" } })], false);
  assert.equal(built.rows.length, 1);
});

test("error texts shrink to one line and stay masked", () => {
  const ctx = eventContext();
  const google = 'host_call_failed: {\n  "error": {\n    "code": 403,\n    "message": "The caller does not have permission for alice@example.com",\n' +
    '    "status": "PERMISSION_DENIED",\n    "details": [{ "@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "TOS_VIOLATION" }]\n  }\n} (HTTP 403)';
  assert.equal(ctx.errorSummary(google), "host_call_failed: HTTP 403 · TOS_VIOLATION · The caller does not have permission for alice@example.com");
  assert.equal(ctx.errorSummary('{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}} (HTTP 429)'),
    "HTTP 429 · rate_limit_error · Rate limit reached");
  assert.equal(ctx.errorSummary('刷新失败：{"error":"invalid_grant","error_description":"Token has been expired"}'), "刷新失败：invalid_grant · Token has been expired");
  assert.equal(ctx.errorSummary("dial tcp: timeout\nmore lines"), "dial tcp: timeout");
  assert.equal(ctx.errorSummary("{not json\nsecond"), "{not json");
  const long = ctx.errorSummary("x".repeat(400));
  assert.equal(Array.from(long).length, 160);
  assert.ok(long.endsWith("…"));

  const ev = event("ignite_failed", 0, { group: "g", label: "alice", params: { retry_seconds: "300", error: google } });
  const view = ctx.eventView(ev);
  assert.ok(!view.detail.includes("\n"));
  assert.ok(view.detail.includes("TOS_VIOLATION"));
  assert.ok(view.tip.includes('"code": 403'));
  // The summary from the backend wins over the page's own.
  assert.ok(ctx.eventView({ ...ev, params: { ...ev.params, summary: "账号被封禁" } }).detail.includes("账号被封禁"));
  // Masked, neither the label nor the error leaves a plain text node.
  const nodes = ctx.eventMasked(ev, view);
  const plainText = (list) => list.filter((n) => n.class !== "account-sensitive").map((n) => n.text || "").join("");
  assert.equal(plainText(nodes.label), "");
  assert.ok(!plainText(nodes.detail).includes("alice"), plainText(nodes.detail));
  assert.ok(!plainText(nodes.detail).includes("TOS_VIOLATION"));
  // The tooltips of notification marks are left out while masked.
  ctx.masked = true;
  const marks = ctx.pushMarks([event("notify_failed", 0, { params: { title: "⚠️ alice 点火已暂停", error: "x" } })]);
  assert.equal(marks[0].title, null);
});

test("credential events show the reason, a summary and an appeal link", () => {
  const ctx = eventContext();
  const invalid = (params) => ctx.eventView(event("credential_invalid", 0, { level: "error", label: "Gemini", params }));
  let view = invalid({ reason: "suspended", status: "403", code: "TOS_VIOLATION", summary: "账号因违反服务条款被停用", appeal_url: "https://support.example.com/appeal", source: "ignite" });
  assert.equal(view.badge, "ev_credential_suspended");
  assert.equal(view.style, "failure");
  // Found by ignition: the row says ignition is paused.
  assert.equal(view.detail, "ev_ignite_paused_note(账号因违反服务条款被停用)");
  assert.equal(view.link, "https://support.example.com/appeal");
  // The tooltip shows the status, code and summary, and the appeal address.
  assert.equal(view.tip, "HTTP 403 · TOS_VIOLATION · 账号因违反服务条款被停用\nhttps://support.example.com/appeal");
  assert.ok(view.secrets.includes("账号因违反服务条款被停用"));
  view = invalid({ reason: "auth", summary: "", status: "401", appeal_url: "javascript:alert(1)" });
  assert.equal(view.badge, "ev_credential_invalid");
  assert.equal(view.detail, "HTTP 401");
  assert.equal(view.link, "");
  view = invalid({ reason: "rate_limited", summary: "请求过多", until: "2026-11-16T08:00:00Z" });
  assert.equal(view.badge, "ev_credential_rate_limited");
  assert.equal(view.detail, "ev_limited_until(请求过多|11-16T08:00)");
  view = ctx.eventView(event("credential_restored", 0, { label: "Gemini", params: { source: "query" } }));
  assert.equal(view.badge, "ev_credential_restored");
  assert.equal(view.style, "success");
  assert.equal(view.detail, "ev_source_query");
  // A credential notification joins its event.
  const built = ctx.eventRows([
    event("notify", 5, { group: "k", label: "Gemini", params: { title: "⚠️ Gemini", notify_kind: "suspended" } }),
    event("credential_invalid", 0, { group: "k", label: "Gemini", params: { reason: "suspended" } }),
    event("notify", 5, { group: "j", label: "Claude", params: { title: "✅ Claude", notify_kind: "credential_ok" } }),
    event("credential_restored", 0, { group: "j", label: "Claude", params: { source: "query" } }),
  ], false);
  assert.deepEqual(plain(built.rows.map((r) => [r.event.kind, r.pushes.length])), [["credential_invalid", 1], ["credential_restored", 1]]);
});

// ---- Even pace extended back to the cycle start ----

function extension() {
  return context(["paceSegments", "paceAt", "pooledPace", "paceExtensions", "extendedPace"], { RESET_DRIFT: 60, RESET_JUMP: 5 });
}

// Synthetic samples shaped like a 7-day window whose quota came back to
// 100% at rise and whose scheduled reset moved back to an earlier time at
// move: [time, remaining].
function movedReset() {
  const rise = 6000 * DAY, x1 = rise + WEEK + 4 * 60 + 14, x2 = rise + 6 * DAY - 52 * 60, move = rise + 16.5 * HOUR;
  const points = [[rise - 2 * HOUR, 22], [rise - HOUR, 21], [rise, 100], [rise + 6 * HOUR, 91], [move - 2 * HOUR, 65], [move, 21], [move + HOUR, 20]];
  // The history records a change of the reset only beyond 60 s; the
  // reset wavered between x1 and x1 + 1 meanwhile.
  const resets = [[rise - 3 * DAY, x2], [rise, rise + WEEK], [rise + 4 * 60, x1], [move, x2]];
  return { rise, x1, x2, move, points, resets };
}

test("a reset that moves within its cycle extends the new line back to the cycle start", () => {
  const ctx = extension();
  const d = movedReset();
  const segs = ctx.paceSegments(d.resets, WEEK, d.x2, d.move + 2 * HOUR);
  const ext = ctx.paceExtensions(segs, d.points, WEEK);
  assert.deepEqual(plain(ext), [[d.rise, d.move, d.x2]]);
  // The old line stays: just before the move it follows the reset x1.
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, d.move - 1) - 100 * (d.x1 - d.move + 1) / WEEK) < 1e-6);
  // The extension runs the line of x2 back to the rise.
  const part = { weight: 1, segments: segs, extensions: ext };
  assert.ok(Math.abs(ctx.extendedPace([part], WEEK, d.rise) - 100 * (d.x2 - d.rise) / WEEK) < 1e-9);
  assert.ok(Math.abs(ctx.extendedPace([part], WEEK, d.move) - ctx.paceAt(segs, WEEK, d.move)) < 1e-9);
  // Outside the extension there is none.
  assert.equal(ctx.extendedPace([part], WEEK, d.rise - 1), null);
  assert.equal(ctx.extendedPace([part], WEEK, d.move + 1), null);
});

test("resets that waver by a second extend nothing", () => {
  const ctx = extension();
  const d = movedReset();
  // The same cycle recorded with every one-second change, as a backend
  // without the 60 s tolerance would: x1, x1 + 1, x1, x1 + 1.
  const resets = [[d.rise, d.rise + WEEK], [d.rise + 4 * 60, d.x1], [d.rise + 6 * HOUR, d.x1 + 1], [d.rise + 8 * HOUR, d.x1], [d.rise + 9 * HOUR, d.x1 + 1]];
  const segs = ctx.paceSegments(resets, WEEK, d.x1, d.rise + 10 * HOUR);
  assert.deepEqual(plain(ctx.paceExtensions(segs, d.points, WEEK)), []);
  // The status reset that wavers adds no segment either.
  assert.equal(ctx.paceSegments(resets, WEEK, d.x1 + 1, d.rise + 10 * HOUR).length, segs.length);
  // A line that starts with its cycle, or an early reset, extends nothing.
  const early = d.rise + 2 * DAY;
  assert.deepEqual(plain(ctx.paceExtensions(ctx.paceSegments([[d.rise, d.x1], [early, early + WEEK]], WEEK, null, early + HOUR),
    [[d.rise, 100], [early - HOUR, 40], [early, 100]], WEEK)), []);
  // Without a rise in the samples, the start of the cycle is not known.
  assert.deepEqual(plain(ctx.paceExtensions(ctx.paceSegments([[d.move, d.x2]], WEEK, null, d.move + HOUR), [[d.move, 21]], WEEK)), []);
});

test("a total extends where an account does, with the other accounts' lines", () => {
  const ctx = extension();
  const d = movedReset();
  const segsA = ctx.paceSegments(d.resets, WEEK, d.x2, d.move + HOUR);
  const a = { weight: 1, segments: segsA, extensions: ctx.paceExtensions(segsA, d.points, WEEK) };
  const xb = d.rise + 3 * DAY;
  const segsB = ctx.paceSegments([[xb - WEEK, xb]], WEEK, xb, d.move + HOUR);
  const b = { weight: 3, segments: segsB, extensions: ctx.paceExtensions(segsB, [], WEEK) };
  const t = d.rise + HOUR;
  const want = (100 * (d.x2 - t) / WEEK + 3 * 100 * (xb - t) / WEEK) / 4;
  assert.ok(Math.abs(ctx.extendedPace([a, b], WEEK, t) - want) < 1e-9);
  assert.equal(ctx.extendedPace([b], WEEK, t), null);
});

test("the detail heading moves left of the plot only where it fits", () => {
  const ctx = context(["placeDetailHead"], { SIDE_GAP: 46 });
  const wrap = () => {
    const classes = new Set(), style = {};
    return { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) }, style: { setProperty: (k, v) => { style[k] = v; } }, classes, css: style };
  };
  const box = { clientWidth: 1000, offsetHeight: 206 };
  let w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 180 }, box, { labelW: 190, width: 1000 });
  assert.ok(w.classes.has("side"));
  assert.equal(w.css["--detail-side"], "144px");
  // Too narrow: the blank of a chart below 640 pixels.
  w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 100 }, { clientWidth: 600, offsetHeight: 206 }, { labelW: 132, width: 600 });
  assert.ok(!w.classes.has("side"));
  // Too tall, such as a long name over several lines.
  w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 240 }, box, { labelW: 190, width: 1000 });
  assert.ok(!w.classes.has("side"));
});

test("manual ignition failures show no retry, and credential pauses no end time", () => {
  const ctx = eventContext();
  const json = '{"error":{"code":401,"message":"token expired","status":"UNAUTHENTICATED"}}';
  let view = ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { manual: "1", error: json, summary: "凭证已失效" } }));
  assert.equal(view.detail, "凭证已失效");
  assert.equal(view.tip, json);
  view = ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { manual: "1", error: json } }));
  assert.equal(view.detail, "HTTP 401 · UNAUTHENTICATED · token expired");
  assert.ok(view.secrets.includes(json));
  // Automatic failures keep the retry; older manual failures without params
  // keep their stored text.
  assert.match(ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { retry_seconds: "300", error: "x" } })).detail, /^ev_retry\(/);
  assert.equal(ctx.eventView(event("ignite_failed", 0, { label: "GPT", detail: "点火失败：x" })).detail, "点火失败：x");

  const paused = (params) => ctx.eventView(event("ignite_paused", 0, { level: "error", label: "Gemini", params }));
  assert.equal(paused({ reason: "auth", error: json, summary: "凭证已失效" }).detail, "ev_paused_auth(凭证已失效)");
  view = paused({ reason: "suspended", error: json });
  assert.equal(view.detail, "ev_paused_suspended(HTTP 401 · UNAUTHENTICATED · token expired)");
  assert.equal(view.tip, "ev_paused_suspended(" + json + ")");
  assert.equal(paused({ reason: "rate_limited", until: "2026-11-17T00:00:00Z", error: "x", summary: "请求过多" }).detail, "ev_paused(11-17T00:00|请求过多)");
  assert.equal(paused({ until: "2026-11-17T00:00:00Z", error: "boom" }).detail, "ev_paused(11-17T00:00|boom)");
  // Without an end time or a known reason, no "until —".
  assert.equal(paused({ error: "boom" }).detail, "boom");
});

test("a credential problem shares its row with the ignition pause and the notifications", () => {
  const ctx = eventContext();
  const g = { group: "claude:k", label: "Claude" };
  const until = "2026-11-16T23:00:00Z";
  const events = [
    event("notify", 1800, { ...g, params: { title: "⏳ Claude 被限流", notify_kind: "rate_limited" } }),
    event("notify_failed", 30, { ...g, level: "warn", params: { title: "⚠️ Claude 点火已暂停", notify_kind: "circuit", error: "Bark: 500" } }),
    event("credential_invalid", 0, { ...g, level: "error", params: { reason: "rate_limited", status: "429", summary: "请求过多", until, source: "ignite" } }),
    event("ignite_paused", 0, { ...g, level: "error", params: { until, error: '{"error":{"code":429,"message":"Number of requests has exceeded your rate limit"}}', summary: "请求过多" } }),
    // Another group's pause stays its own row.
    event("ignite_paused", 10, { group: "gemini:k", label: "Gemini", level: "error", params: { until, error: "x" } }),
  ];
  const built = ctx.eventRows(events, false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.kind + ":" + r.event.label)), ["credential_invalid:Claude", "ignite_paused:Gemini"]);
  const row = built.rows[0];
  assert.equal(row.paused.kind, "ignite_paused");
  // The rate limit notification half an hour later and the circuit
  // notification of the pause both join it.
  assert.deepEqual(plain(row.pushes.map((ev) => ev.params.notify_kind).sort()), ["circuit", "rate_limited"]);
  const view = ctx.eventView(row.event, row.notifyKind, row.paused);
  assert.equal(view.badge, "ev_credential_rate_limited");
  assert.equal(view.detail, "ev_ignite_paused_until(请求过多|11-16T23:00)");
  // The tooltip adds the full error of the pause on a line of its own.
  assert.equal(view.tip, 'HTTP 429 · 请求过多\n{"error":{"code":429,"message":"Number of requests has exceeded your rate limit"}}');
  // A pause more than PAUSE_PAIR seconds away is a row of its own.
  assert.equal(ctx.eventRows([events[2], { ...events[3], time: new Date((T0 - 61) * 1000).toISOString() }], false).rows.length, 2);
  // Found by a query, without a pause: the rate limit alone.
  assert.equal(ctx.eventView(event("credential_invalid", 0, { ...g, params: { reason: "rate_limited", summary: "请求过多", until, source: "query" } })).detail,
    "ev_limited_until(请求过多|11-16T23:00)");
  // An account suspended at ignition: one row too.
  const suspended = ctx.eventRows([
    event("credential_invalid", 0, { ...g, params: { reason: "suspended", summary: "停用", source: "ignite" } }),
    event("ignite_paused", 0, { ...g, params: { reason: "suspended", error: "x", summary: "停用" } }),
  ], false);
  assert.equal(suspended.rows.length, 1);
  const merged = ctx.eventView(suspended.rows[0].event, undefined, suspended.rows[0].paused);
  assert.equal(merged.detail, "ev_ignite_paused_note(停用)");
  // With an appeal address, the pause's error follows it; a pause without
  // an error adds nothing.
  const appeal = { reason: "suspended", status: "403", summary: "停用", appeal_url: "https://appeal.example.com/", source: "ignite" };
  const withPause = (error) => ctx.eventView(event("credential_invalid", 0, { ...g, params: appeal }), undefined,
    event("ignite_paused", 0, { ...g, params: error ? { reason: "suspended", error, summary: "停用" } : { reason: "suspended", summary: "停用" } }));
  assert.equal(withPause("raw").tip, "HTTP 403 · 停用\nhttps://appeal.example.com/\nraw");
  assert.equal(withPause("").tip, "HTTP 403 · 停用\nhttps://appeal.example.com/");
});

test("Did Codex Reset notifications join the event recorded after them", () => {
  const ctx = eventContext();
  const signal = (at) => event("codex_reset", at, { label: "Did Codex Reset", params: { count: "1" } });
  const push = (at, fields = {}) => event("notify", at, { params: { title: "✅ Codex 全局重置已完成", notify_kind: "codex_reset" }, ...fields });
  let built = ctx.eventRows([signal(10), push(5), push(0, { params: { title: "📅 Codex 全局重置已排期" } })], false);
  assert.equal(built.rows.length, 1);
  assert.equal(built.rows[0].event.kind, "codex_reset");
  assert.equal(built.rows[0].pushes.length, 2);
  // More than five minutes apart: the notification stays a row, under the
  // label of Did Codex Reset.
  built = ctx.eventRows([signal(400), push(0)], false);
  assert.equal(built.rows.length, 2);
  const view = ctx.eventView(built.rows[1].event, built.rows[1].notifyKind);
  assert.equal(view.label, "Did Codex Reset");
  assert.equal(view.detail, "✅ Codex 全局重置已完成");
});

test("older failure events without params hide their whole detail when masked", () => {
  const ctx = eventContext();
  const plainText = (list) => list.filter((n) => n.class !== "account-sensitive").map((n) => n.text || "").join("");
  // A manual ignition failure from before params, naming an account that
  // has since been removed: no name list can catch it.
  const old = [
    event("ignite_failed", 0, { group: "claude:k", label: "Claude", level: "warn", detail: "手动点火失败：permission denied for gone@example.com" }),
    event("ignite_paused", 0, { group: "claude:k", label: "Claude", level: "error", detail: "暂停到 11/17：gone@example.com" }),
    event("notify_failed", 0, { group: "claude:k", label: "Claude", level: "warn", detail: "Bark：gone@example.com" }),
    event("error", 0, { level: "error", label: "Claude", detail: "额度查询失败：gone@example.com" }),
  ];
  for (const ev of old) {
    const view = ctx.eventView(ev);
    assert.ok(view.whole, ev.kind);
    const nodes = ctx.eventMasked(ev, view);
    assert.equal(plainText(nodes.detail), "", ev.kind);
    assert.ok(nodes.detail.some((n) => n.text.includes("gone@example.com")), ev.kind);
  }
  // An older success keeps its detail, with only the names hidden.
  const ok = event("ignite", 0, { group: "claude:k", label: "Claude", detail: "claude-haiku · 下次重置 11/17" });
  assert.equal(ctx.eventView(ok).whole, false);
  assert.equal(plainText(ctx.eventMasked(ok).detail), "claude-haiku · 下次重置 11/17");
  // An older success without a stored label hides whole too, even when its
  // group gives a label now: the account may have been renamed since.
  const ctx2 = eventContext({ status: { accounts: [{ groups: [{ key: "claude:k", label: "Claude Now" }] }] } });
  const unlabeled = event("ignite", 0, { group: "claude:k", message: "Claude Old 点火成功（claude-haiku），下次重置 11/17" });
  const view = ctx2.eventView(unlabeled);
  assert.equal(view.label, "Claude Now");
  assert.ok(view.whole);
  const nodes = ctx2.eventMasked(unlabeled, view);
  assert.equal(plainText(nodes.detail), "");
  assert.ok(nodes.detail.some((n) => n.text.includes("Claude Old")));
  // Events with params keep the masking by names and error texts.
  assert.equal(ctx.eventView(event("ignite_failed", 0, { label: "Claude", params: { manual: "1", error: "x", summary: "y" } })).whole, false);
});

test("the theme and language follow Magpie, else the browser", () => {
  const ctx = context(["pageTheme", "pageLanguage"]);
  assert.equal(ctx.pageTheme({ dark: true, lang: "en" }, false), "dark");
  assert.equal(ctx.pageTheme({ dark: false, lang: "en" }, true), "light");
  assert.equal(ctx.pageTheme(null, true), "dark");
  assert.equal(ctx.pageTheme({}, false), "light");
  assert.equal(ctx.pageLanguage({ dark: false, lang: "zh-TW" }, "en-US"), "zh");
  assert.equal(ctx.pageLanguage({ dark: false, lang: "ja" }, "zh-CN"), "en");
  assert.equal(ctx.pageLanguage(null, "zh-CN"), "zh");
  assert.equal(ctx.pageLanguage({ dark: true }, "fr"), "en");
  assert.equal(ctx.pageLanguage(null, ""), "en");
});

test("the page speaks Magpie's bridge protocol to its parent only", async () => {
  const posted = [], calls = { theme: 0, language: 0, refresh: 0 };
  const parent = { postMessage: (msg, target) => posted.push([plain(msg), target]) };
  const ctx = context(["inFrame", "magpieCall", "magpieMessage", "magpieTheme", "magpieVisible", "themeTokens", "tokenValue"], {
    window: { parent }, magpie: { ready: false, theme: null, visible: true, calls: {}, next: 0 }, MAGPIE_TOKENS: ["bg"], FONT_TOKENS: {},
    SANDBOXED: true, CSS: fakeCSS,
    status: null, history: null, renderChart: () => {},
    applyTheme: () => calls.theme++, applyLanguage: () => calls.language++,
    loadStatus: () => { calls.refresh++; return Promise.resolve(); }, refreshHistory: () => {},
    contentReady: true, keyAsked: () => false, loadPage: () => { calls.load = (calls.load || 0) + 1; },
  });
  const hello = ctx.magpieCall("hello");
  assert.deepEqual(posted, [[{ magpie: 1, id: 1, method: "hello" }, "*"]]);
  // Another window's answer, or one without the marker, is ignored.
  ctx.magpieMessage({ source: {}, data: { magpie: 1, id: 1, result: { dark: false } } });
  ctx.magpieMessage({ source: parent, data: { id: 1, result: { dark: false } } });
  ctx.magpieMessage({ source: parent, data: { magpie: 1, id: 1, result: { dark: true, lang: "zh", tokens: {}, visible: true } } });
  assert.deepEqual(plain(await hello), { dark: true, lang: "zh", tokens: {}, visible: true });
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "theme", data: { dark: false, lang: "en", tokens: { bg: "#fff" } } } });
  assert.deepEqual(plain(ctx.magpie.theme), { dark: false, lang: "en", tokens: { bg: "#fff" } });
  assert.equal(calls.theme, 1);
  assert.equal(calls.language, 1);
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "visibility", data: { visible: false } } });
  assert.equal(ctx.magpie.visible, false);
  ctx.status = {};
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "visibility", data: { visible: true } } });
  assert.equal(ctx.magpie.visible, true);
  assert.equal(calls.refresh, 1);
  // Back to a page that has not read a valid config yet, it loads at once.
  ctx.contentReady = false;
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "visibility", data: { visible: false } } });
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "visibility", data: { visible: true } } });
  assert.equal(calls.load, 1);
  assert.equal(calls.refresh, 1);
  const copy = ctx.magpieCall("image.copy", { png: "blob" });
  ctx.magpieMessage({ source: parent, data: { magpie: 1, id: 2, error: "A picture is a PNG of 16 MB at most" } });
  await assert.rejects(copy, /16 MB/);
  // The page never reports errors to Magpie, which would replace it.
  assert.doesNotMatch(source, /method: "error"|magpieCall\("error"/);
});

test("Magpie's theme tokens become --magpie-<token> variables on the root", () => {
  const ctx = context(["themeTokens", "applyTokens", "tokenValue"], { CSS: fakeCSS });
  vm.runInContext(pageVar("MAGPIE_TOKENS") + pageVar("FONT_TOKENS"), ctx);
  assert.deepEqual([...ctx.MAGPIE_TOKENS], ["bg", "card", "fg", "fg-2", "muted", "faint", "line", "accent", "accent-soft", "accent-fg", "green", "red", "amber", "font", "code"]);
  // Only the known keys with string values are taken.
  const tokens = ctx.themeTokens({ bg: " #1a1a1e ", "fg-2": "#b9b9c1", accent: 5, red: "", font: '"SF Pro Text", sans-serif', "x; color": "red", line: null });
  assert.deepEqual(plain(tokens), { bg: "#1a1a1e", "fg-2": "#b9b9c1", font: '"SF Pro Text", sans-serif' });
  assert.deepEqual(plain(ctx.themeTokens(null)), {});
  assert.deepEqual(plain(ctx.themeTokens("bg")), {});
  const props = {};
  const style = { setProperty: (k, v) => { props[k] = v; }, removeProperty: (k) => { delete props[k]; } };
  ctx.applyTokens(style, tokens);
  assert.deepEqual(props, { "--magpie-bg": "#1a1a1e", "--magpie-fg-2": "#b9b9c1", "--magpie-font": '"SF Pro Text", sans-serif' });
  // A token missing from a later theme falls back to page.css.
  ctx.applyTokens(style, { bg: "#f4f4f6" });
  assert.deepEqual(props, { "--magpie-bg": "#f4f4f6" });
  ctx.applyTokens(style, null);
  assert.deepEqual(props, {});
});

test("page.css takes its colors and fonts from Magpie's tokens, with Magpie's defaults", () => {
  const css = fs.readFileSync(path.join(__dirname, "page.css"), "utf8");
  for (const token of ["bg", "card", "fg", "fg-2", "muted", "faint", "line", "accent", "accent-soft", "accent-fg", "green", "red", "amber", "font", "code"]) {
    assert.match(css, new RegExp("var\\(--magpie-" + token + ", "), token);
  }
  // Magpie's default light and dark values are the fallbacks.
  assert.match(css, /--bg: var\(--magpie-bg, #f4f4f6\)/);
  assert.match(css, /--bg: var\(--magpie-bg, #1a1a1e\)/);
  assert.doesNotMatch(css, /Management Center|CPA/);
  // The quota levels keep colors of their own, full apart from high.
  const level = (name) => css.match(new RegExp("--quota-" + name + "-color: (#[0-9a-f]{6})"))[1];
  assert.notEqual(level("full"), level("high"));
  assert.equal(new Set(["full", "high", "medium", "low", "empty"].map(level)).size, 5);
});

test("a screenshot keeps the root's Magpie tokens", () => {
  assert.match(source, /var rootStyle = root\.getAttribute\("style"\)/);
  const ctx = context(["xmlAttr"]);
  assert.equal(ctx.xmlAttr('--magpie-font: "A&B", <x>'), "--magpie-font: &quot;A&amp;B&quot;, &lt;x>");
});

test("opened on its own the page asks nothing of a parent", async () => {
  const win = {};
  win.parent = win;
  const ctx = context(["inFrame", "magpieCall"], { window: win, magpie: { calls: {}, next: 0 }, SANDBOXED: true });
  assert.equal(ctx.inFrame(), false);
  await assert.rejects(ctx.magpieCall("hello"), /not in Magpie/);
});

test("a page outside a sandbox ignores the site that frames it", async () => {
  const posted = [], calls = { theme: 0 };
  const parent = { postMessage: (msg) => posted.push(msg) };
  // Magpie always frames the page sandboxed; a page from the public
  // listener framed by another site is not.
  const ctx = context(["inFrame", "magpieCall", "magpieMessage", "magpieHello"], {
    window: { parent, addEventListener: () => { throw new Error("listens"); } },
    magpie: { ready: false, theme: null, visible: true, calls: {}, next: 0 }, SANDBOXED: false,
    magpieTheme: () => calls.theme++, magpieVisible: () => calls.theme++,
  });
  assert.equal(ctx.inFrame(), false);
  ctx.magpieHello();
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "theme", data: { dark: true, tokens: { bg: "#000" } } } });
  ctx.magpieMessage({ source: parent, data: { magpie: 1, event: "visibility", data: { visible: false } } });
  await assert.rejects(ctx.magpieCall("image.copy", { png: "blob" }), /not in Magpie/);
  assert.deepEqual(posted, []);
  assert.equal(calls.theme, 0);
  assert.equal(ctx.magpie.ready, false);
  assert.match(source, /function inFrame\(\) \{\n    if \(!SANDBOXED\) return false;/);
});

test("theme tokens take only colors and font lists that load nothing", () => {
  const ctx = context(["tokenValue"], { CSS: fakeCSS });
  vm.runInContext(pageVar("FONT_TOKENS"), ctx);
  for (const good of ["#1a1a1e", "rgb(28 28 33)", "rgba(0, 0, 0, .5)", "oklch(70% 0.1 250)", "color-mix(in srgb, #fff 20%, #000)", "white"]) {
    assert.equal(ctx.tokenValue("bg", good), good, good);
  }
  for (const bad of ["url(https://evil.example/x)", "URL(x)", "image-set(\"x.png\" 1x)", "-webkit-image-set(url(x) 1x)", "var(--x)", "env(safe-area-inset-top)",
    "attr(data-x)", "red; background: url(x)", "red}", "#fff\\", "<x>", "notacolor(1)", "#fff 1px", "", "  ", 5, null]) {
    assert.equal(ctx.tokenValue("bg", bad), "", String(bad));
  }
  assert.equal(ctx.tokenValue("font", ' -apple-system, "SF Pro Text", "Segoe UI", sans-serif '), '-apple-system, "SF Pro Text", "Segoe UI", sans-serif');
  assert.equal(ctx.tokenValue("code", 'ui-monospace, "SF Mono", Menlo, monospace'), 'ui-monospace, "SF Mono", Menlo, monospace');
  for (const bad of ["url(x.woff)", "a; color: red", "a{}", "a\\66", "var(--x)", "@import", "x".repeat(301)]) {
    assert.equal(ctx.tokenValue("font", bad), "", bad);
  }
  // Without CSS.supports no color is taken.
  ctx.CSS = undefined;
  assert.equal(ctx.tokenValue("bg", "#fff"), "");
});

test("the sandbox drops neither storage errors nor confirmations", () => {
  // Every use of localStorage tolerates the sandbox's SecurityError.
  const lines = source.split("\n");
  lines.forEach((line, i) => {
    if (!/localStorage\./.test(line) || /^\s*\/\//.test(line)) return;
    const guarded = /try \{/.test(line) || /try \{\s*$/.test(lines[i - 1]) || /try \{\s*$/.test(lines[i - 2]);
    assert.ok(guarded, "line " + (i + 1) + ": " + line.trim());
  });
  assert.doesNotMatch(source, /parentRoot|watchParent|cli-proxy|sessionStorage|document\.cookie|\balert\(|\bprompt\(/);
  // confirm() is called only where it can ask.
  assert.deepEqual(lines.filter((line) => /\bconfirm\(/.test(line) && !/^\s*\/\//.test(line)).map((line) => line.trim()), ["if (!SANDBOXED) return confirm(message);"]);

  const toasts = [];
  let now = 1000;
  const ctx = context(["confirmed"], {
    SANDBOXED: true, toast: (text) => toasts.push(text), translate: (key, arg) => key + ":" + arg,
    confirm: () => { throw new Error("no dialogs in the sandbox"); },
    Date: { now: () => now },
  });
  const button = {};
  assert.equal(ctx.confirmed(button, "Ignite?"), false);
  assert.deepEqual(toasts, ["confirm_again:Ignite?"]);
  now += 4000;
  assert.equal(ctx.confirmed(button, "Ignite?"), true);
  assert.equal(ctx.confirmed(button, "Ignite?"), false);
  now += 6000;
  assert.equal(ctx.confirmed(button, "Ignite?"), false);
  ctx.SANDBOXED = false;
  ctx.confirm = (text) => text === "Ignite?";
  assert.equal(ctx.confirmed(button, "Ignite?"), true);
});

test("Magpie's warm-up accounts are named as the page names them", () => {
  const ctx = context(["warmupNames"]);
  const title = (p) => ({ claude: "Claude", codex: "Codex" })[p] || p;
  const accounts = [
    { auth_index: "claude/a", provider: "claude", note: "Work\nsecond line", email: "a@example.com" },
    { auth_index: "codex/b", provider: "codex", email: "b@example.com" },
  ];
  assert.deepEqual(plain(ctx.warmupNames(["claude/a", "codex/b", "claude/gone", "claude/off", "", 3], accounts, { "claude/off": "Spare" }, title)),
    ["Claude Work", "Codex b@example.com", "claude/gone", "Spare"]);
  assert.deepEqual(plain(ctx.warmupNames(undefined, accounts, {}, title)), []);
  assert.deepEqual(plain(ctx.warmupNames(null, accounts, {}, title)), []);
});

test("enabling the page on the public listener saves a new admin key with it", () => {
  let inputs = [];
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey", "adminKey"], {
    rawConfig: {}, status: { config: {} },
    $: () => ({ elements: inputs }),
    newMcpKey: () => "llmcp_generated", newAdminKey: () => "lladm_generated",
  });
  function patch(name, value) {
    inputs = [{ name, value, type: "checkbox", checked: value, getAttribute: () => null }];
    return JSON.parse(JSON.stringify(ctx.buildPatch(name)));
  }
  assert.deepEqual(patch("web_enabled", true), { web_enabled: true, admin_key: "lladm_generated" });
  assert.deepEqual(patch("web_enabled", false), { web_enabled: false });
  ctx.rawConfig.admin_key = "lladm_saved";
  assert.deepEqual(patch("web_enabled", true), { web_enabled: true });
});

test("generated keys are 32 random bytes in base64url after their prefix", () => {
  const ctx = context(["newSecret", "newMcpKey", "newAdminKey"], { crypto: require("node:crypto").webcrypto, Uint8Array, btoa });
  assert.match(ctx.newAdminKey(), /^lladm_[A-Za-z0-9_-]{43}$/);
  assert.match(ctx.newMcpKey(), /^llmcp_[A-Za-z0-9_-]{43}$/);
  assert.notEqual(ctx.newAdminKey(), ctx.newAdminKey());
});

test("the public listener state falls back to the MCP state", () => {
  const ctx = context(["publicState"]);
  const pub = { listen: "0.0.0.0:8318", running: true, error: "", web: true, mcp: false };
  assert.equal(ctx.publicState({ public: pub, mcp: { running: false } }), pub);
  assert.deepEqual(plain(ctx.publicState({ mcp: { enabled: true, listen: "127.0.0.1:8318", running: false, error: "bind" } })),
    { listen: "127.0.0.1:8318", running: false, error: "bind", web: null, mcp: false });
  assert.equal(ctx.publicState({}), null);
  assert.equal(ctx.publicState(null), null);
});

test("a 401 asks for the key unless the page changed the key meanwhile", async () => {
  const asked = [];
  const ctx = context(["api"], {
    key: "", PAGE_TOKEN: "", translate: (k) => k, setTimeout, clearTimeout, askForKey: (message) => asked.push(message),
    fetch: async () => ({ status: 401, ok: false, json: async () => ({ error: "unauthorized" }) }),
  });
  await assert.rejects(ctx.api("GET", "api/status"), /unauthorized/);
  assert.deepEqual(asked, [""]);
  ctx.key = "old";
  await assert.rejects(ctx.api("GET", "api/status"), /unauthorized/);
  assert.deepEqual(asked, ["", "key_invalid"]);
  ctx.fetch = async (path, opts) => {
    assert.equal(opts.headers.Authorization, "Bearer old");
    ctx.key = "new";
    return { status: 401, ok: false, json: async () => ({}) };
  };
  await assert.rejects(ctx.api("GET", "api/status"), /unauthorized/);
  assert.equal(asked.length, 2);
});

test("the key form works without form submission", () => {
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /<button id="key-submit"[^>]+type="button"/);
  assert.match(source, /\$\("key-submit"\)\.addEventListener\("click", submitKey\)/);
  assert.match(source, /\$\("key-input"\)\.addEventListener\("keydown"/);
  // The admin key is generated, shown and copied like the MCP key.
  for (const id of ["admin-key-view", "admin-key-show", "admin-key-copy", "admin-key-new", "public-status"]) assert.match(html, new RegExp('id="' + id + '"'));
  assert.match(html, /name="web_enabled" type="checkbox"/);
});

test("API requests carry the page token only when the page has one", async () => {
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  // The internal listener replaces this exact tag (web.PageWith).
  assert.equal(html.split('<meta name="lamplighter-page" content="">').length, 2);
  const meta = (content) => ({ querySelector: (sel) => {
    assert.equal(sel, 'meta[name="lamplighter-page"]');
    return content === null ? null : { getAttribute: () => content };
  } });
  const token = context(["pageToken"]);
  assert.equal(token.pageToken(meta("tok_123")), "tok_123");
  assert.equal(token.pageToken(meta("")), "");
  assert.equal(token.pageToken(meta(null)), "");

  const sent = [];
  const ctx = context(["api"], {
    key: "", authEpoch: 0, PAGE_TOKEN: "tok_123", translate: (k) => k, setTimeout, clearTimeout, askForKey: () => {},
    fetch: async (p, opts) => { sent.push(opts.headers); return { status: 200, ok: true, json: async () => ({}) }; },
  });
  await ctx.api("GET", "api/status");
  await ctx.api("PATCH", "api/config", { a: 1 });
  assert.equal(sent[0]["X-Lamplighter-Page"], "tok_123");
  assert.equal(sent[1]["X-Lamplighter-Page"], "tok_123");
  ctx.PAGE_TOKEN = "";
  await ctx.api("GET", "api/status");
  assert.equal("X-Lamplighter-Page" in sent[2], false);
  // Requests go out only through api(), and never on a parent's message.
  assert.equal(source.match(/\bfetch\(/g).length, 1);
  const handler = source.match(/  function magpieMessage\([^]*?\n  }/)[0];
  assert.doesNotMatch(handler, /api\(|fetch\(/);
});

// ---- Time zone and ignition settings ----

const pageHtml = () => fs.readFileSync(path.join(__dirname, "page.html"), "utf8");

function timezoneContext(rawConfig, statusConfig) {
  const input = saveNode("INPUT", { name: "timezone" });
  input.type = "text";
  input.accountDirty = undefined;
  const unsaved = { hidden: true };
  const ctx = context(["effectiveConfig", "browserTimezone", "renderTimezone", "fillInput", "getPath"], {
    rawConfig, status: { config: statusConfig || {} }, MCP_LISTEN: "127.0.0.1:8318",
    Intl: { DateTimeFormat: () => ({ resolvedOptions: () => ({ timeZone: "Asia/Shanghai" }) }) },
    document: { activeElement: null },
    $: (id) => id === "settings" ? { elements: { timezone: input } } : id === "timezone-unsaved" ? unsaved : null,
  });
  const fill = () => { ctx.fillInput(input, ctx.effectiveConfig()); ctx.renderTimezone(); };
  return { ctx, input, unsaved, fill };
}

test("without a saved time zone the field shows the browser's, marked as not saved", () => {
  const { ctx, input, unsaved, fill } = timezoneContext({}, { timezone: "" });
  fill();
  assert.equal(input.value, "Asia/Shanghai");
  assert.equal(unsaved.hidden, false);
  // The field being edited, a draft or a failed value is left alone.
  input.value = "";
  ctx.document.activeElement = input;
  ctx.renderTimezone();
  assert.equal(input.value, "");
  ctx.document.activeElement = null;
  input.settingsDirty = true;
  ctx.renderTimezone();
  assert.equal(input.value, "");
  input.settingsDirty = false;
  input.setAttribute("aria-invalid", "true");
  ctx.renderTimezone();
  assert.equal(input.value, "");
  input.removeAttribute("aria-invalid");
  // The saved name shows without the mark, whatever the status reports.
  ctx.rawConfig.timezone = "Europe/Berlin";
  fill();
  assert.equal(input.value, "Europe/Berlin");
  assert.equal(unsaved.hidden, true);
  // A browser that reports no time zone leaves the field empty and unmarked.
  const none = timezoneContext({});
  none.ctx.Intl = { DateTimeFormat: () => { throw new Error("no Intl"); } };
  none.fill();
  assert.equal(none.input.value, "");
  assert.equal(none.unsaved.hidden, true);
  // Filling the field writes nothing: the page has no save in that path.
  const render = source.match(/  function renderTimezone\([^]*?\n  }/)[0];
  assert.doesNotMatch(render, /saveField|patchConfig|api\(/);
});

test("the time zone datalist lists the browser's names, or none to type in", () => {
  const ctx = context(["timezoneOptions"]);
  ctx.Intl = { supportedValuesOf: (kind) => { assert.equal(kind, "timeZone"); return ["Asia/Shanghai", "Europe/Berlin"]; } };
  assert.deepEqual(plain(ctx.timezoneOptions()), ["Asia/Shanghai", "Europe/Berlin", "UTC"]);
  ctx.Intl = { supportedValuesOf: () => ["UTC", "Asia/Tokyo"] };
  assert.deepEqual(plain(ctx.timezoneOptions()), ["UTC", "Asia/Tokyo"]);
  ctx.Intl = {};
  assert.deepEqual(plain(ctx.timezoneOptions()), []);
  ctx.Intl = { supportedValuesOf: () => { throw new RangeError("unsupported"); } };
  assert.deepEqual(plain(ctx.timezoneOptions()), []);
  const html = pageHtml();
  assert.match(html, /<input name="timezone" type="text" list="timezone-options"/);
  assert.match(html, /<datalist id="timezone-options"><\/datalist>/);
  assert.match(source, /timezoneOptions\(\)\.forEach\(function \(name\) \{ \$\("timezone-options"\)\.appendChild\(el\("option", \{ value: name \}\)\); \}\);/);
});

function timezoneSaveContext(reply) {
  const heading = saveNode("H2");
  const section = { tagName: "SECTION", querySelector: (selector) => { assert.equal(selector, ".card-title"); return heading; } };
  const input = saveNode("INPUT", { name: "timezone" });
  input.type = "text";
  input.closest = (selector) => selector === ".settings-section" ? section : null;
  const sent = [], refilled = [];
  const ctx = stateContext(["saveField", "captureIntent", "buildPatch", "inputTop", "topKey", "setPath", "getPath", "noteEdit", "patchConfig", "keepSaved", "api", "rereadConfig"], {
    rawConfig: {}, status: { config: {} }, saveQueue: Promise.resolve(), savedTokens: [], authEpoch: 0, pendingOrder: null,
    THRESHOLDS: ["notice_threshold", "low_threshold", "critical_threshold"],
    CONFIG_API: "api/config", key: "", PAGE_TOKEN: "", askForKey: () => {}, setTimeout, clearTimeout,
    newConfigToken: () => "0123456789abcdef",
    $: () => ({ elements: [input] }),
    fetch: async (p, opts) => { sent.push(JSON.parse(opts.body)); return reply(JSON.parse(opts.body)); },
    showApplied: (say) => { say("saved", "applied"); return Promise.resolve(true); },
    refillSaved: (intent) => refilled.push(intent),
    loadConfig: async () => { Object.assign(ctx.rawConfig, { timezone: sent.at(-1).timezone }); },
  });
  vm.runInContext(pageVar("PAGE_STATE_KEYS"), ctx);
  return { ctx, heading, input, sent, refilled };
}

test("saving the filled-in time zone writes it to the config", async () => {
  const { ctx, heading, input, sent, refilled } = timezoneSaveContext(() => ({ status: 200, ok: true, json: async () => ({}) }));
  // The browser's time zone shown in the field, saved with the button.
  input.value = "Asia/Shanghai";
  ctx.saveField({ target: input });
  await ctx.saveQueue;
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(sent, [{ timezone: "Asia/Shanghai", config_token: "0123456789abcdef" }]);
  assert.equal(ctx.rawConfig.timezone, "Asia/Shanghai");
  assert.equal(heading.textContent, "applied");
  assert.equal(refilled.length, 1);
  assert.match(source, /\$\("timezone-save"\)\.addEventListener\("click", function \(\) \{ saveField\(\{ target: \$\("settings"\)\.elements\.timezone \}\); \}\);/);
});

test("saving an empty time zone writes an empty string, which leaves it unset", async () => {
  const { ctx, input, sent } = timezoneSaveContext(() => ({ status: 200, ok: true, json: async () => ({}) }));
  ctx.rawConfig.timezone = "Asia/Shanghai";
  input.value = "  ";
  ctx.saveField({ target: input });
  await ctx.saveQueue;
  await new Promise((resolve) => setImmediate(resolve));
  // internal/config treats "" as no timezone and does not reject it.
  assert.deepEqual(sent, [{ timezone: "", config_token: "0123456789abcdef" }]);
});

test("a time zone the plugin rejects shows its 400 error as a save failure", async () => {
  const error = 'unknown time zone "Mars/Olympus"';
  const { ctx, heading, input, sent } = timezoneSaveContext(() => ({ status: 400, ok: false, json: async () => ({ error }) }));
  input.value = " Mars/Olympus ";
  ctx.saveField({ target: input });
  await ctx.saveQueue;
  assert.equal(sent[0].timezone, "Mars/Olympus");
  assert.equal(ctx.rawConfig.timezone, undefined);
  assert.equal(heading.textContent, "✕ Save failed✕ timezone save failed: " + error);
  assert.equal(input.getAttribute("aria-invalid"), "true");
  assert.equal(input.value, " Mars/Olympus ");
});

test("the ignition switch shows the saved config and assumes no default", () => {
  const effective = (raw, statusEnabled) => {
    const { ctx } = timezoneContext(raw, { ignition: { enabled: statusEnabled, start_hour: 7 } });
    return plain(ctx.effectiveConfig().ignition);
  };
  assert.deepEqual(effective({ ignition: { enabled: false } }, true), { enabled: false, start_hour: 7 });
  assert.deepEqual(effective({ ignition: { enabled: true } }, false), { enabled: true, start_hour: 7 });
  // A config file without the key shows what the plugin applies.
  assert.equal(effective({}, false).enabled, false);
  assert.equal(effective({ ignition: {} }, true).enabled, true);
  const html = pageHtml();
  assert.match(html, /<input name="ignition\.enabled" type="checkbox">/);
  // The hint about the time zone and Magpie's warm-up sits by the switch.
  assert.match(html, /name="ignition\.enabled"[^\n]*\n\s*<\/div>\n\s*<p class="hint" id="ignition-enable-hint" data-i18n="ignition_enable_hint">/);
  for (const language of ["zh", "en"]) {
    const dictionary = source.match(new RegExp("\\n    " + language + ": \\{[^]*?\\n    \\}"))[0];
    for (const name of ["ignition_enable_hint", "timezone", "timezone_name", "timezone_save", "timezone_unsaved", "timezone_hint"]) {
      assert.match(dictionary, new RegExp("\\b" + name + ": \""), language + " " + name);
    }
  }
  assert.match(source, /ignition_enable_hint: "开启前先设置时区，并在 Magpie 中关闭预热，以免重复点火。"/);
});

test("no text refers to CPA settings the Magpie plugin does not have", () => {
  const html = pageHtml();
  for (const text of [source, html]) {
    assert.doesNotMatch(text, /ignition_models_hint|no_models_key|API key|模型列表 API|管理中心|Management (key|Center)|CLIProxy|lock_error/i);
  }
});

test("without a configured time zone times show in the browser's", () => {
  const translate = (key, ...args) => key + "(" + args.join("|") + ")";
  const ctx = context(["zoneSet", "offsetSeconds", "shifted", "pad", "fmtDateTime", "utcText", "timezoneText"], { translate, status: null });
  // 15:55 UTC; the browser is 8 hours ahead of UTC at that time.
  const at = Date.UTC(2026, 10, 21, 15, 55);
  const t = { getTime: () => at, getTimezoneOffset: () => -480 };
  for (const status of [{ timezone: "Local", utc_offset_seconds: 0 }, { timezone: "", utc_offset_seconds: 0 }, null]) {
    ctx.status = status;
    assert.equal(ctx.zoneSet(status), false);
    assert.equal(ctx.fmtDateTime(t), "11/21 23:55", JSON.stringify(status));
  }
  ctx.status = { timezone: "Local", utc_offset_seconds: 0 };
  assert.equal(ctx.timezoneText(), "tz_note_unset(UTC+0)");
  // A configured time zone wins over the browser's.
  ctx.status = { timezone: "UTC", utc_offset_seconds: 0 };
  assert.equal(ctx.zoneSet(ctx.status), true);
  assert.equal(ctx.fmtDateTime(t), "11/21 15:55");
  ctx.status = { timezone: "America/St_Johns", utc_offset_seconds: -12600 };
  assert.equal(ctx.fmtDateTime(t), "11/21 12:25");
  assert.equal(ctx.timezoneText(), "tz_note(America/St_Johns (UTC-3.5)|tz_config())");
  ctx.status = { timezone: "UTC+8", utc_offset_seconds: 28800 };
  assert.equal(ctx.timezoneText(), "tz_note(UTC+8|tz_config())");
  // The quota view says when no time zone is set and leads to the setting.
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /<div id="view-quota">\s*<div id="zone-notice" class="notice notice-info" hidden><\/div>/);
  assert.match(source, /box\.hidden = zoneSet\(status\);/);
  assert.match(source, /showView\("settings", true\);/);
});

test("a quota group of an account with several names its service apart from the card", () => {
  const titles = { antigravity: "Antigravity", claude: "Claude", codex: "GPT" };
  const status = {
    accounts: [
      { auth_index: "1", provider: "codex", email: "a@example.com", groups: [{ key: "codex:1:gpt", label: "GPT", source_label: "GPT", windows: [{ id: "five-hour", kind: "5h", short: "5h", remaining: 50, reset: "" }] }] },
      { auth_index: "3", provider: "claude", email: "c@example.com", groups: [{ key: "claude:3:claude", label: "Claude", source_label: "Claude", windows: [{ id: "five-hour", kind: "5h", short: "5h", remaining: 40, reset: "" }] }] },
      { auth_index: "4", provider: "antigravity", groups: [
        { key: "antigravity:4:gemini", label: "Gemini", source_label: "Gemini", windows: [{ id: "five-hour", kind: "5h", short: "5h", remaining: 100, reset: "" }] },
        { key: "antigravity:4:cg", label: "Claude / GPT", source_label: "Claude / GPT", windows: [{ id: "five-hour", kind: "5h", short: "5h", remaining: 65, reset: "" }] },
      ] },
    ],
    group_order: ["GPT", "Claude", "Gemini", "Claude / GPT"],
  };
  const providerTitle = (id) => titles[id] || id;
  const ctx = context(["sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName", "groupTitle", "fitName", "graphemes", "chartRows", "hiddenGroups", "groupHidden", "windowKind", "cycleFloor", "accountWeight", "pooledRow", "parseTime"], {
    status, providerTitle, rawConfig: {}, chartSort: "default", historyWindow: "5h", RESET_JUMP: 5,
    gapLimit: () => 900, pooled: () => [], graphemeSegmenter: undefined,
  });
  vm.runInContext(pageVar("SERVICE_SHORT"), ctx);
  assert.deepEqual(plain(ctx.sharedGroups(status.accounts)), { "antigravity:4:gemini": "antigravity", "antigravity:4:cg": "antigravity" });
  assert.equal(ctx.groupTitle("antigravity:4:cg", "Claude / GPT"), "Antigravity · Claude / GPT");
  assert.equal(ctx.groupTitle("claude:3:claude", "Claude"), "Claude");
  assert.equal(ctx.groupTitle("gone:9:x", "X"), "X");
  assert.equal(ctx.groupTitle("antigravity:4:cg", ""), "");

  const now = 1795280000;
  const series = (s, value) => ({ ...s, window: "five-hour", window_label: "5h", kind: "5h", points: [[now - 600, value], [now, value]], resets: [] });
  ctx.history = { from: new Date((now - 6 * 3600) * 1000).toISOString(), series: [
    series({ group: "codex:1:gpt", label: "GPT", source_label: "GPT", provider: "codex", auth_index: "1" }, 50),
    series({ group: "claude:3:claude", label: "Claude", source_label: "Claude", provider: "claude", auth_index: "3" }, 40),
    series({ group: "antigravity:4:gemini", label: "Gemini", source_label: "Gemini", provider: "antigravity", auth_index: "4" }, 100),
    series({ group: "antigravity:4:cg", label: "Claude / GPT", source_label: "Claude / GPT", provider: "antigravity", auth_index: "4" }, 65),
  ] };
  const rows = ctx.chartRows();
  assert.deepEqual(plain(rows.map((r) => r.name)), ["GPT", "Claude", "Antigravity · Gemini", "Antigravity · Claude / GPT"]);
  assert.deepEqual(plain(rows.map((r) => r.short)), ["", "", "AG · ", "AG · "]);
  // Pooled rows of several Antigravity accounts name the service too.
  status.accounts.push({ auth_index: "5", provider: "antigravity", groups: [
    { key: "antigravity:5:gemini", label: "Gemini#b", source_label: "Gemini", windows: [{ id: "five-hour", kind: "5h", short: "5h", remaining: 30, reset: "" }] },
    { key: "antigravity:5:cg", label: "Claude / GPT#b", source_label: "Claude / GPT", windows: [{ id: "five-hour", kind: "5h", short: "5h", remaining: 20, reset: "" }] },
  ] });
  ctx.history.series.push(series({ group: "antigravity:5:cg", label: "Claude / GPT#b", source_label: "Claude / GPT", provider: "antigravity", auth_index: "5" }, 20));
  const pooled = ctx.chartRows().filter((r) => r.children);
  assert.deepEqual(plain(pooled.map((r) => r.name)), ["Antigravity · Gemini", "Antigravity · Claude / GPT"]);
  assert.deepEqual(plain(pooled.map((r) => [r.prefix, r.short])), [["Antigravity · ", "AG · "], ["Antigravity · ", "AG · "]]);
  assert.deepEqual(plain(pooled[1].children.map((c) => c.name)), ["Antigravity · Claude / GPT", "Antigravity · Claude / GPT#b"]);

  // A narrow label column uses the short form of the service, which always
  // stays in front of the group label.
  // Widths in characters here; the page measures the text.
  const chars = (text) => Array.from(text).length;
  const ag = (name, max) => ctx.fitName(name, "Antigravity · ", max, "AG · ", chars);
  assert.equal(ag("Antigravity · Claude / GPT", 30), "Antigravity · Claude / GPT");
  assert.equal(ag("Antigravity · Claude / GPT", 20), "AG · Claude / GPT");
  assert.equal(ag("Antigravity · Claude / GPT", 17), "AG · Claude / GPT");
  assert.equal(ag("Antigravity · Claude / GPT", 15), "AG · Claude / …");
  assert.equal(ag("Antigravity · Gemini", 15), "AG · Gemini");
  // The narrowest pooled row, and narrower: never the group label alone.
  assert.equal(ag("Antigravity · Claude / GPT", 10), "AG · Clau…");
  assert.equal(ag("Antigravity · Claude / GPT", 4), "AG · C…");
  for (const max of [1, 4, 8, 10, 12, 15, 16, 17]) assert.match(ag("Antigravity · Claude / GPT", max), /^AG · /, String(max));
  assert.equal(ctx.fitName("alice.long.name@example.com", "", 10, "", chars), "alice.lon…");
  // Measured widths: narrow letters fit more characters.
  const narrow = (text) => Array.from(text).reduce((w, c) => w + (/[ ·.il\/]/.test(c) ? 3 : 7), 0);
  assert.equal(ctx.fitName("Antigravity · Claude / GPT", "Antigravity · ", 106, "AG · ", narrow), "AG · Claude / GPT");
  assert.equal(ctx.fitName("Antigravity · Claude / GPT", "Antigravity · ", 80, "AG · ", narrow), "AG · Claude / …");
  assert.equal(ctx.fitName("Antigravity · Claude / GPT", "Antigravity · ", 60, "AG · ", narrow), "AG · Clau…");
  assert.equal(ctx.serviceShort("antigravity"), "AG");
  assert.equal(ctx.serviceShort("codex"), "GPT");
  assert.equal(ctx.serviceShort("claude"), "CL");

  // Events and the ignition table name it the same way.
  const events = eventContext({ status, providerTitle });
  const ev = event("ignite_ok", 0, { group: "antigravity:4:cg", label: "Claude / GPT", params: {} });
  assert.equal(events.eventParts(ev).label, "Antigravity · Claude / GPT");
  assert.equal(events.eventMasked(ev).label[0].text, "Antigravity · Claude / GPT");
  assert.match(source, /title = groupTitle\(t\.key, t\.label\)/);
});

test("time axis labels thin out to fit a narrow chart", () => {
  const ctx = context(["chartTicks", "offsetSeconds", "zoneSet", "unixDate", "fmtDay", "fmtClock", "shifted", "pad"], { status: { timezone: "UTC", utc_offset_seconds: 0 } });
  vm.runInContext("var CHAR_W = 6.6;" + pageVar("TICK_STEPS") + pageVar("TICK_LABEL_W"), ctx);
  const to = Date.UTC(2026, 10, 21, 12) / 1000;
  const geometry = (plotW, days) => ({ from: to - days * 86400, to, plotW });
  const spacing = (g) => { const t = ctx.chartTicks(g); return { gap: g.plotW * (t.list[1] - t.list[0]) / (g.to - g.from), labels: t.list.map(t.label) }; };
  // Wide: one tick a day over 8 days.
  const wide = spacing(geometry(900, 8));
  assert.equal(wide.labels.length, 8);
  // The narrowest overview (480 px, 248 px of plot): every other day.
  const narrow = spacing(geometry(248, 8));
  assert.ok(narrow.gap >= 5 * 6.6 + 10, String(narrow.gap));
  assert.ok(narrow.labels.length <= 4 && narrow.labels.length >= 3, narrow.labels.join(" "));
  // Hours thin out the same way.
  const hours = spacing(geometry(248, 1));
  assert.ok(hours.gap >= 5 * 6.6 + 10);
  assert.ok(hours.labels.every((l) => /^\d\d:00$/.test(l)), hours.labels.join(" "));
});

test("the threshold label of the detail chart keeps at most one decimal", () => {
  const ctx = context(["axisPercent"]);
  assert.equal(ctx.axisPercent(20), "20%");
  assert.equal(ctx.axisPercent(12.5), "12.5%");
  assert.equal(ctx.axisPercent(12.345), "12.3%");
  assert.equal(ctx.axisPercent(33.333333), "33.3%");
  assert.match(source, /fill: cssVar\("--failure-text"\) \}, axisPercent\(low\)\)/);
});

test("links to other sites open outside the sandbox and show their address inside it", () => {
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.doesNotMatch(html + source, /target="_blank"|target: "_blank"|raw\.githubusercontent|ghfast/);
  // The name is no link until page.js makes it one outside the sandbox.
  assert.match(html, /<a class="home" id="home"><img class="logo" src="\/\*LOGO\*\/"/);
  const made = [];
  const node = () => { const n = { attrs: {}, listeners: {}, setAttribute: (k, v) => { n.attrs[k] = v; }, addEventListener: (k, f) => { n.listeners[k] = f; } }; made.push(n); return n; };
  const shown = [];
  const ctx = context(["outsideLink", "setupHome"], { el: node, SANDBOXED: false, showAddress: (text, url) => shown.push([text, url]) });
  const home = node();
  ctx.$ = () => home;
  ctx.REPO_URL = "https://github.com/PosvdM/magpie-lamplighter";
  const open = ctx.outsideLink({}, "https://example.com/appeal", "copy");
  assert.equal(open.attrs.target, "_blank");
  assert.equal(open.attrs.href, "https://example.com/appeal");
  ctx.setupHome();
  assert.equal(home.attrs.href, ctx.REPO_URL);
  ctx.SANDBOXED = true;
  const boxed = ctx.outsideLink({}, "https://example.com/appeal", "copy");
  assert.equal(boxed.attrs.target, undefined);
  let prevented = false;
  boxed.listeners.click({ preventDefault: () => { prevented = true; } });
  assert.ok(prevented);
  assert.deepEqual(shown, [["copy", "https://example.com/appeal"]]);
  const plainHome = node();
  ctx.$ = () => plainHome;
  ctx.setupHome();
  assert.deepEqual(plainHome.attrs, {});
});

// The page.css blocks outside @supports, and the declarations of a rule.
function cssParts() {
  const css = fs.readFileSync(path.join(__dirname, "page.css"), "utf8").replace(/\/\*[^]*?\*\//g, "");
  let outside = "", supports = "", i = 0;
  while (i < css.length) {
    const at = css.indexOf("@supports", i);
    if (at < 0) { outside += css.slice(i); break; }
    outside += css.slice(i, at);
    let depth = 0, j = css.indexOf("{", at);
    for (; j < css.length; j++) {
      if (css[j] === "{") depth++;
      else if (css[j] === "}" && --depth === 0) break;
    }
    supports += css.slice(at, j + 1);
    i = j + 1;
  }
  const rule = (selector) => {
    const m = outside.match(new RegExp(selector.replace(/[[\]().*"]/g, "\\$&") + "\\s*\\{([^}]*)\\}"));
    assert.ok(m, selector);
    return Object.fromEntries([...m[1].matchAll(/(--[\w-]+):\s*([^;]+);/g)].map((d) => [d[1], d[2].trim()]));
  };
  return { css, outside, supports, rule };
}

test("browsers without color-mix() get static colors, and token colors are background-color", () => {
  const { css, outside, supports, rule } = cssParts();
  // Every color-mix() is inside a @supports (color: color-mix(...)) block.
  assert.doesNotMatch(outside, /color-mix/);
  assert.match(supports, /^@supports \(color: color-mix\(in srgb, red, blue\)\)/);
  // Each variable the block mixes has a static value outside it.
  const light = rule(":root"), dark = rule(':root[data-theme="dark"]');
  for (const [, name] of supports.matchAll(/(--[\w-]+):\s*color-mix/g)) {
    if (name === "--type-bg") continue;
    assert.ok(light[name], name);
  }
  // The static values of the dark theme replace those of the light theme
  // that are fixed colors.
  for (const name of ["--bg-tertiary", "--bg-hover", "--border-primary-static", "--border-hover", "--primary-hover", "--focus-ring", "--quota-track",
    "--row-hover", "--fieldset-bg", "--seg-track", "--warning-bg", "--success-badge-bg", "--failure-badge-bg", "--amber-bg", "--success-badge-border",
    "--failure-badge-border", "--amber-border", "--chart-lane-static", "--chart-hatch-static", "--chart-select-static"]) {
    assert.ok(dark[name] && dark[name] !== light[name], name);
  }
  for (const type of ["codex", "claude", "antigravity", "kimi"]) assert.ok(light["--type-" + type + "-bg"] && dark["--type-" + type + "-bg"], type);
  // Plain colors of the theme set background-color, never the background
  // shorthand.
  assert.doesNotMatch(css, /background:\s*(var|color-mix|url)/);
  for (const [, value] of css.matchAll(/background:\s*([^;}]+)/g)) assert.match(value.trim(), /^(transparent|currentColor)$/, value);
});

// WCAG contrast of two colors, #rrggbb or rgba() over the color under it.
function rgb(c, under) {
  if (c.startsWith("#")) return [1, 3, 5].map((i) => parseInt(c.slice(i, i + 2), 16));
  const [r, g, b, a] = c.match(/[\d.]+/g).map(Number);
  const u = rgb(under);
  return [r, g, b].map((v, i) => v * a + u[i] * (1 - a));
}
function contrast(a, b) {
  const lum = (c) => { const f = c.map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }); return 0.2126 * f[0] + 0.7152 * f[1] + 0.0722 * f[2]; };
  const [x, y] = [lum(a), lum(b)];
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
}

test("small status text reads at 4.5:1 or more in both themes", () => {
  const { rule } = cssParts();
  const light = rule(":root"), darkRule = rule(':root[data-theme="dark"]');
  const themes = {
    light: { ...light, card: "#ffffff", bg: "#f4f4f6", green: "#189a58", red: "#d64545", amber: "#b45309" },
    dark: { ...light, ...darkRule, card: "#232327", bg: "#1a1a1e", green: "#6fd99b", red: "#f28b82", amber: "#f2b544" },
  };
  for (const [name, t] of Object.entries(themes)) {
    const texts = {
      green: [t["--success-text-static"], t["--success-badge-bg"]],
      red: [t["--failure-text-static"], t["--failure-badge-bg"]],
      amber: [name === "light" ? t["--amber-text"] : t.amber, t["--amber-bg"]],
    };
    for (const [color, [text, tint]] of Object.entries(texts)) {
      for (const ground of [t.card, t.bg]) {
        assert.ok(contrast(rgb(text), rgb(ground)) >= 4.5, `${name} ${color} on ${ground}`);
        assert.ok(contrast(rgb(text), rgb(tint, ground)) >= 4.5, `${name} ${color} on its tint over ${ground}`);
      }
    }
    for (const type of ["codex", "claude", "antigravity", "kimi"]) {
      assert.ok(contrast(rgb(t["--type-" + type]), rgb(t["--type-" + type + "-bg"])) >= 4.5, `${name} ${type} badge`);
    }
  }
  // The light green, red and amber of Magpie stay for icons, lines and
  // fills; text that is small takes the text colors.
  const css = fs.readFileSync(path.join(__dirname, "page.css"), "utf8");
  assert.match(css, /--success-text: color-mix\(in srgb, var\(--green\) 62%, var\(--fg\)\)/);
  for (const selector of [".badge-success", ".source-passive", ".quota-percent.percent-empty", '.save-state[data-state="failed"]', ".event-mark.failed", ".test-result.failed"]) {
    const m = css.match(new RegExp(selector.replace(/[[\]().*"]/g, "\\$&") + "\\s*\\{([^}]*)\\}"));
    assert.ok(m, selector);
    assert.doesNotMatch(m[1], /color: var\(--(green|red|amber|error-color|viz-success|viz-failure)\)/, selector);
  }
  assert.doesNotMatch(source, /svgText\([^)]*cssVar\("--viz-(success|failure)"/);
});

test("a color the browser cannot resolve falls back to the static value", () => {
  const SENTINEL = "rgb(1, 2, 3)";
  // A browser that knows rgb() and #hex but not color-mix(): the probe keeps
  // its parent's color.
  const make = (knows) => {
    const vars = {
      "--chart-lane": "color-mix(in srgb, #ededf1 4%, transparent)", "--chart-lane-static": "rgba(237, 237, 241, 0.04)",
      "--odd": "color-mix(in srgb, red, blue)", "--fg": "#ededf1",
    };
    const node = (tag) => ({ tag, style: {}, children: [], parent: null });
    const ctx = context(["cssVar", "resolveColor"], {
      colorCache: {}, PROBE_SENTINEL: SENTINEL,
      el: (tag, attrs, children) => { const n = node(tag); (children || []).forEach((c) => { c.parent = n; n.children.push(c); }); return n; },
      document: { documentElement: { root: true }, body: { appendChild: () => {}, removeChild: () => {} } },
      getComputedStyle: (n) => n.root ? { getPropertyValue: (k) => vars[k] || "" }
        : { color: knows(n.style.color) ? "rgb(9, 9, 9)" : n.parent && n.parent.style.color },
    });
    // The style setter of an element rejects what the browser cannot parse.
    return ctx;
  };
  // Assignment taken, computed color inherited: a failure.
  let ctx = make(() => false);
  assert.equal(ctx.cssVar("--chart-lane"), "rgba(237, 237, 241, 0.04)");
  assert.equal(ctx.cssVar("--odd"), "#ededf1");
  // Resolved: the computed color.
  ctx = make((v) => /^color-mix/.test(v));
  assert.equal(ctx.cssVar("--chart-lane"), "rgb(9, 9, 9)");
  // Plain colors are returned as they are.
  assert.equal(ctx.cssVar("--fg"), "#ededf1");
  // The probe sits in a parent of the sentinel color, and an assignment the
  // browser rejects (style.color stays "") counts as a failure too.
  assert.match(source, /host\.style\.color = PROBE_SENTINEL;/);
  assert.match(source, /if \(!probe\.style\.color\) return "";/);
  assert.match(source, /return color && color !== PROBE_SENTINEL \? color : "";/);
});

test("a change of the displayed time zone redraws the chart from the loaded history", () => {
  let drawn = 0;
  const ctx = context(["zoneSet", "zoneKey", "renderZoneChange"], {
    status: { timezone: "Local", utc_offset_seconds: 0 }, history: { series: [] }, shownZone: null,
    chartPointer: false, chartBehind: false, chartCard: { contains: () => false }, document: { activeElement: null },
    renderChart: () => drawn++, timezoneText: () => "note", $: () => ({}),
  });
  ctx.renderZoneChange();
  assert.equal(drawn, 0);
  // The same status again: nothing to redraw.
  ctx.renderZoneChange();
  assert.equal(drawn, 0);
  // From unset to set, then another offset, then a different zone.
  ctx.status = { timezone: "Asia/Shanghai", utc_offset_seconds: 28800 };
  ctx.renderZoneChange();
  assert.equal(drawn, 1);
  ctx.status = { timezone: "Asia/Shanghai", utc_offset_seconds: 28800 };
  ctx.renderZoneChange();
  assert.equal(drawn, 1);
  ctx.status = { timezone: "America/St_Johns", utc_offset_seconds: -12600 };
  ctx.renderZoneChange();
  assert.equal(drawn, 2);
  // With the mouse on the chart the redraw waits until it leaves.
  ctx.chartPointer = true;
  ctx.status = { timezone: "", utc_offset_seconds: 0 };
  ctx.renderZoneChange();
  assert.equal(drawn, 2);
  assert.equal(ctx.chartBehind, true);
  assert.equal(ctx.zoneKey({ timezone: "Local", utc_offset_seconds: 3600 }), "browser");
  // Nothing new is fetched, and renderStatus calls it.
  assert.doesNotMatch(source.match(/  function renderZoneChange\(\) \{[^]*?\n  \}/)[0], /fetchHistory|loadHistory/);
  assert.match(source, /renderMcp\(\);\n    renderZoneChange\(\);\n  \}/);
});

test("account events keep their account label, without the service in front again", () => {
  const titles = { antigravity: "Antigravity", claude: "Claude" };
  const status = { accounts: [{ auth_index: "4", provider: "antigravity", groups: [
    { key: "antigravity:4:gemini", label: "Gemini#main", source_label: "Gemini" },
    { key: "antigravity:4:cg", label: "Claude / GPT#main", source_label: "Claude / GPT" },
  ] }] };
  const ctx = eventContext({ status, providerTitle: (id) => titles[id] || id });
  // Credential events name the account and carry its first group's key.
  const credential = event("credential_invalid", 0, { group: "antigravity:4:gemini", label: "Antigravity#main", level: "error", params: { reason: "auth" } });
  assert.equal(ctx.eventParts(credential).label, "Antigravity#main");
  assert.equal(ctx.eventMasked(credential).label[0].text, "Antigravity#main");
  const restored = event("credential_restored", 0, { group: "antigravity:4:gemini", label: "Antigravity", params: { source: "query" } });
  assert.equal(ctx.eventParts(restored).label, "Antigravity");
  for (const kind of ["credential", "suspended", "rate_limited", "credential_ok"]) {
    const sent = event("notify", 0, { group: "antigravity:4:gemini", label: "Antigravity#main", params: { title: "x", notify_kind: kind } });
    assert.equal(ctx.eventParts(sent).label, "Antigravity#main", kind);
  }
  // Quota group events, and their notifications, get the service.
  assert.equal(ctx.eventParts(event("reset", 0, { group: "antigravity:4:cg", label: "Claude / GPT#main", params: { window: "five-hour", kind: "5h", label: "5h", remaining: "40" } })).label,
    "Antigravity · Claude / GPT#main");
  assert.equal(ctx.eventParts(event("notify", 0, { group: "antigravity:4:cg", label: "Claude / GPT#main", params: { title: "x", notify_kind: "quota" } })).label,
    "Antigravity · Claude / GPT#main");
  assert.equal(ctx.eventParts(event("ignite_paused", 0, { group: "antigravity:4:cg", label: "Claude / GPT#main", params: { until: "2026-11-21T08:00:00Z" } })).label,
    "Antigravity · Claude / GPT#main");
  // An account label never gets the service twice, whatever the kind.
  assert.equal(ctx.groupTitle("antigravity:4:cg", "Antigravity#main"), "Antigravity#main");
  assert.equal(ctx.groupTitle("antigravity:4:cg", "Antigravity"), "Antigravity");
});

test("a Claude account with several groups never shows Claude · Claude", () => {
  const titles = { claude: "Claude" };
  const win = (remaining) => [{ id: "five-hour", kind: "5h", short: "5h", remaining, reset: "" }];
  // Two Claude accounts, each with its main group and a Fable group.
  const status = {
    accounts: [
      { auth_index: "1", provider: "claude", email: "a@example.com", groups: [
        { key: "claude:1:main", label: "Claude#rk", source_label: "Claude", windows: win(40) },
        { key: "claude:1:fable", label: "Fable#rk", source_label: "Fable", windows: win(70) },
      ] },
      { auth_index: "2", provider: "claude", email: "b@example.com", groups: [
        { key: "claude:2:main", label: "Claude#am", source_label: "Claude", windows: win(60) },
        { key: "claude:2:fable", label: "Fable#am", source_label: "Fable", windows: win(90) },
      ] },
    ],
    group_order: ["Claude", "Fable"],
  };
  const ctx = context(["sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName", "groupTitle", "chartRows", "hiddenGroups", "groupHidden", "windowKind", "cycleFloor",
    "accountWeight", "pooledRow", "parseTime", "renderChartTable"], {
    status, providerTitle: (id) => titles[id] || id, rawConfig: {}, chartSort: "default", historyWindow: "5h", RESET_JUMP: 5,
    gapLimit: () => 900, pooled: () => [],
  });
  vm.runInContext(pageVar("SERVICE_SHORT"), ctx);
  const now = 1795280000;
  const series = (group, label, source, auth, value) => ({ group, label, source_label: source, provider: "claude", auth_index: auth,
    window: "five-hour", window_label: "5h", kind: "5h", points: [[now - 600, value], [now, value]], resets: [] });
  ctx.history = { from: new Date((now - 6 * 3600) * 1000).toISOString(), series: [
    series("claude:1:main", "Claude#rk", "Claude", "1", 40), series("claude:1:fable", "Fable#rk", "Fable", "1", 70),
    series("claude:2:main", "Claude#am", "Claude", "2", 60), series("claude:2:fable", "Fable#am", "Fable", "2", 90),
  ] };
  const rows = ctx.chartRows();
  // The totals, the rows of their accounts and the table name the main
  // group as the service itself, and the Fable group with the service.
  assert.deepEqual(plain(rows.map((r) => [r.name, r.prefix, r.short])), [["Claude", "", ""], ["Claude · Fable", "Claude · ", "CL · "]]);
  assert.deepEqual(plain(rows[0].children.map((c) => [c.name, c.prefix])), [["Claude#rk", ""], ["Claude#am", ""]]);
  assert.deepEqual(plain(rows[1].children.map((c) => c.name)), ["Claude · Fable#rk", "Claude · Fable#am"]);
  const cells = [];
  ctx.$ = () => ({ appendChild: (tr) => cells.push(tr.children[0]) });
  ctx.clear = () => {};
  ctx.el = (tag, attrs = {}, children = []) => ({ tag, ...attrs, children });
  ctx.maskedNodes = (text) => [text];
  ctx.pct = String;
  ctx.fmtCountdown = String;
  ctx.translate = (k) => k;
  ctx.SENSITIVE = "account-sensitive";
  ctx.renderChartTable(rows);
  assert.deepEqual(plain(cells.map((td) => td.text || td.children[0])), ["Claude#rk", "Claude#am", "Claude · Fable#rk", "Claude · Fable#am"]);
  // The events and the ignition table take the same titles.
  assert.equal(ctx.groupTitle("claude:1:main", "Claude#rk"), "Claude#rk");
  assert.equal(ctx.groupTitle("claude:1:main", "Claude"), "Claude");
  assert.equal(ctx.groupTitle("claude:1:fable", "Fable#rk"), "Claude · Fable#rk");
  const all = JSON.stringify(plain(rows));
  assert.doesNotMatch(all, /Claude · Claude/);
  // Every title comes from groupName: nothing else puts the service in front.
  assert.equal(source.match(/servicePrefix\(/g).length, 2);
  assert.equal(source.match(/shortPrefix\(/g).length, 2);
});

test("a label without a length limit is cut with few measurements", () => {
  const ctx = context(["fitName", "graphemes"], { graphemeSegmenter: undefined });
  let calls = 0;
  const chars = (text) => { calls++; return Array.from(text).length; };
  const long = "Antigravity · " + "Model ".repeat(2000) + "end";
  const name = ctx.fitName(long, "Antigravity · ", 20, "AG · ", chars);
  assert.equal(name, "AG · Model Model Mo…");
  // The whole name, its short form, about log2(12000) halvings and the
  // check of the candidate: not one measurement per character.
  assert.ok(calls <= 2 + Math.ceil(Math.log2(long.length)) + 1, String(calls));
  // Too narrow even for the service: the short form and one character.
  assert.equal(ctx.fitName(long, "Antigravity · ", 2, "AG · ", chars), "AG · M…");
});

test("a cut name keeps each letter with its accent and each emoji whole", () => {
  const ctx = context(["fitName", "graphemes"], { graphemeSegmenter: undefined });
  // Widths by graphemes, as a reader counts them.
  const seen = (text) => Array.from(new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(text)).length;
  const accent = "Cafe\u0301 Cafe\u0301 Cafe\u0301", coder = "👩‍💻👩‍💻👩‍💻👩‍💻 team";
  assert.deepEqual([...ctx.graphemes("e\u0301👩‍💻x")], ["e\u0301", "👩‍💻", "x"]);
  for (let room = 2; room < 12; room++) {
    const cut = ctx.fitName(accent, "", room, "", seen);
    assert.ok(!/e…/.test(cut), cut);
    assert.ok(seen(cut) <= room, cut);
  }
  assert.equal(ctx.fitName(accent, "", 5, "", seen), "Cafe\u0301…");
  assert.equal(ctx.fitName(coder, "", 3, "", seen), "👩‍💻👩‍💻…");
  for (let room = 2; room < 10; room++) {
    const cut = ctx.fitName(coder, "", room, "", seen);
    // Never a part of the emoji, such as 👩 alone or a dangling joiner.
    for (const g of new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(cut)) assert.ok(["👩‍💻", " ", "t", "e", "a", "m", "…"].includes(g.segment), cut);
  }
  // Widths that grow with each code point, as the estimate without a
  // canvas does: a cut by code points would drop the accent ("Cafe…") or
  // keep 👩 without the rest of the emoji.
  const points = (text) => Array.from(text).length;
  assert.equal(ctx.fitName(accent, "", 5, "", points), "Caf…");
  assert.equal(ctx.fitName(coder, "", 4, "", points), "👩‍💻…");
  assert.equal(ctx.fitName(coder, "", 3, "", points), "👩‍💻…");
  // The short form of the service counts in graphemes too.
  assert.equal(ctx.fitName("Café · Claude", "Café · ", 9, "Cafe\u0301 · ", seen), "Cafe\u0301 · C…");
  assert.equal(ctx.fitName("Café · Claude", "Café · ", 10, "Cafe\u0301 · ", seen), "Cafe\u0301 · Cl…");
  // Without Intl.Segmenter: code points.
  const old = context(["graphemes"], { Intl: {}, graphemeSegmenter: undefined });
  assert.deepEqual([...old.graphemes("e\u0301👩‍💻")], ["e", "\u0301", "👩", "\u200d", "💻"]);
});

test("masking blurs the suffix of an account and leaves its service readable", () => {
  const win = (remaining) => [{ id: "five-hour", kind: "5h", short: "5h", remaining, reset: "" }];
  const status = {
    accounts: [
      { auth_index: "1", provider: "claude", title: "Claude#rk", email: "ann.rk@example.com", name: "claude-ann.json", groups: [
        { key: "claude:1:main", label: "Claude#rk", source_label: "Claude", windows: win(40) },
        { key: "claude:1:fable", label: "Fable#rk", source_label: "Fable", windows: win(70) },
      ] },
      { auth_index: "2", provider: "claude", title: "Claude#am", email: "bo.am@example.com", groups: [
        { key: "claude:2:main", label: "Claude#am", source_label: "Claude", windows: win(60) },
      ] },
    ],
    events: [{ label: "Claude#old" }],
    group_order: ["Claude", "Fable"],
  };
  const element = (tag, attrs = {}, children = []) => ({ tag, ...attrs, children });
  const ctx = context(["accountSecrets", "hasSecret", "maskedNodes", "sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName",
    "chartRows", "hiddenGroups", "groupHidden", "windowKind", "cycleFloor", "accountWeight", "pooledRow", "parseTime", "renderChartTable"], {
    status, SENSITIVE: "account-sensitive", el: element, document: { createTextNode: (text) => ({ text }) },
    providerList: () => [{ id: "claude", title: "Claude" }, { id: "codex", title: "GPT" }], providerTitle: (id) => ({ claude: "Claude" })[id] || id,
    rawConfig: {}, chartSort: "default", historyWindow: "5h", RESET_JUMP: 5, gapLimit: () => 900, pooled: () => [],
    clear: () => {}, pct: String, fmtCountdown: String, translate: (k) => k,
  });
  vm.runInContext(pageVar("SERVICE_SHORT"), ctx);
  const now = 1795280000;
  const series = (group, label, source, auth, value) => ({ group, label, source_label: source, provider: "claude", auth_index: auth,
    window: "five-hour", window_label: "5h", kind: "5h", points: [[now - 600, value], [now, value]], resets: [] });
  ctx.history = { from: new Date((now - 6 * 3600) * 1000).toISOString(), events: [], series: [
    series("claude:1:main", "Claude#rk", "Claude", "1", 40), series("claude:1:fable", "Fable#rk", "Fable", "1", 70),
    series("claude:2:main", "Claude#am", "Claude", "2", 60),
    // A series of a group no current account has: named by its group key.
    series("claude:9:main", "Claude#gone", "Claude", "9", 10),
  ] };
  // The parts as [text, sensitive].
  const parts = (nodes) => plain(nodes.map((n) => [n.text, n.class === "account-sensitive"]));
  const secrets = ctx.accountSecrets();
  assert.ok(secrets.includes("#rk") && secrets.includes("#am") && secrets.includes("#old"), secrets.join(" "));
  assert.ok(!secrets.includes("Claude#rk") && !secrets.includes("Fable#rk"));
  assert.ok(secrets.includes("ann.rk@example.com") && secrets.includes("claude-ann.json"));
  // The orphan series counts as a whole, whatever it looks like.
  assert.ok(secrets.includes("Claude#gone") && secrets.includes("claude:9:main"));
  assert.deepEqual(parts(ctx.maskedNodes("Claude#rk")), [["Claude", false], ["#rk", true]]);
  assert.deepEqual(parts(ctx.maskedNodes("Claude · Fable#rk")), [["Claude · Fable", false], ["#rk", true]]);
  assert.deepEqual(parts(ctx.maskedNodes("Claude#old")), [["Claude", false], ["#old", true]]);
  assert.deepEqual(parts(ctx.maskedNodes("Claude#gone")), [["Claude#gone", true]]);
  assert.deepEqual(parts(ctx.maskedNodes("ann.rk@example.com")), [["ann.rk@example.com", true]]);
  assert.equal(ctx.hasSecret("Claude#rk"), true);
  assert.equal(ctx.hasSecret("Claude · Fable"), false);
  // The chart's table with the real maskedNodes: the service shows, the
  // suffix is blurred, and the row of the gone account is blurred whole.
  const rows = ctx.chartRows();
  const cells = [];
  ctx.$ = () => ({ appendChild: (tr) => cells.push(tr.children[0]) });
  ctx.renderChartTable(rows);
  const byName = Object.fromEntries(cells.map((td) => [td.text || td.children.map((n) => n.text).join(""), td.class ? [[td.text, true]] : parts(td.children)]));
  assert.deepEqual(byName["Claude#rk"], [["Claude", false], ["#rk", true]]);
  assert.deepEqual(byName["Claude#am"], [["Claude", false], ["#am", true]]);
  assert.deepEqual(byName["Claude · Fable#rk"], [["Claude · Fable", false], ["#rk", true]]);
  assert.deepEqual(byName["Claude#gone"], [["Claude#gone", true]]);
  // The detail heading masks the same way as the table.
  assert.match(source, /row\.orphan \? \[el\("span", \{ class: SENSITIVE, text: row\.name \}\)\] : maskedNodes\(row\.name\)/);
});

test("a redraw reads the font once and sets the canvas font only when it changes", () => {
  let reads = 0, sets = 0, font = "";
  const canvas = { measureText: (text) => ({ width: text.length * (font.startsWith("600 13px") ? 8 : 6) }) };
  Object.defineProperty(canvas, "font", { get: () => font, set: (v) => { sets++; font = v; } });
  const ctx = context(["textMeasure"], {
    measureContext: null,
    document: { body: {}, createElement: () => ({ getContext: () => canvas }) },
    getComputedStyle: () => { reads++; return { fontFamily: "Inter, sans-serif" }; },
  });
  const width = ctx.textMeasure();
  for (let i = 0; i < 50; i++) width("Claude · Fable", 13, 600);
  assert.equal(width("abc", 13, 600), 24);
  assert.equal(width("abc", 11, 400), 18);
  assert.equal(reads, 1);
  assert.equal(sets, 2);
  assert.equal(font, "400 11px Inter, sans-serif");
  // Without a canvas the width is estimated per character.
  const none = context(["textMeasure"], { measureContext: false });
  assert.equal(none.textMeasure()("abcd", 10, 600), 4 * 10 * 0.54);
  // renderOverview makes one measure for all its rows.
  const overview = source.match(/  function renderOverview\([^]*?\n  \}\n/)[0];
  assert.equal(overview.match(/textMeasure\(\)/g).length, 1);
  assert.doesNotMatch(overview, /getComputedStyle/);
});

function pooledContext() {
  return context(["valueAt", "pooled", "resetAsOf", "gapValue", "estimatedRuns"], { RESET_JUMP: 5 });
}

test("a pooled total estimates an account missing data rather than leaving it out", () => {
  const ctx = pooledContext();
  const now = Math.floor(Date.now() / 1000), H = 3600, limit = 900;
  const t0 = now - 4 * H;
  const every = (from, to, value) => { const out = []; for (let t = from; t <= to; t += 300) out.push([t, value, 0]); return out; };
  // a stays full; b reads 51% until a two-hour gap, then a passive 52%
  // (a header reads a point above the usage API) and 51% again.
  const a = { weight: 1, resets: [[t0, now + 5 * 86400]], points: every(t0, now, 100) };
  const b = { weight: 1, resets: [[t0, now + 2 * 86400]],
    points: every(t0, t0 + H, 51).concat([[t0 + 3 * H, 52, 1]], every(t0 + 3 * H + 300, now, 51)) };
  a.floor = a.points; b.floor = b.points;
  const out = ctx.pooled([a, b], limit, "points");
  const gap = out.filter((p) => p[0] > t0 + H + limit && p[0] < t0 + 3 * H);
  assert.ok(gap.length > 10);
  // Never the 100% of a alone; b holds 51% and the total stays at 75.5%.
  gap.forEach((p) => { assert.equal(p[1], 75.5); assert.equal(p[5], 1); });
  const measured = out.filter((p) => p[0] <= t0 + H + limit || p[0] >= t0 + 3 * H);
  measured.forEach((p) => assert.equal(p[5], 0));
  // With no account fresh there is no point: a gap of the total.
  const both = ctx.pooled([{ weight: 1, resets: [], points: [[t0, 50, 0], [t0 + 2 * H, 40, 0]] },
    { weight: 1, resets: [], points: [[t0, 70, 0], [t0 + 2 * H, 60, 0]] }], limit, "points");
  assert.deepEqual(JSON.parse(JSON.stringify(both.map((p) => p[0]))), [t0, t0 + 2 * H]);

  // A reset within the gap shows at the next sample of a fresh account;
  // the total gets no point of its own there, so a stretch without any
  // fresh account stays a gap.
  const resetB = { weight: 1, resets: [[t0, t0 + H + 1800]], points: [[t0, 20, 0], [t0 + 3 * H, 100, 0]] };
  const fresh = { weight: 1, resets: [], points: every(t0, t0 + 3 * H, 50) };
  const total = ctx.pooled([resetB, fresh], limit, "points");
  assert.deepEqual(JSON.parse(JSON.stringify(total.filter((p) => p[0] === t0 + H + 1800).map((p) => p[1]))), [75]);
  assert.deepEqual(JSON.parse(JSON.stringify(total.filter((p) => p[0] === t0 + H + 1500).map((p) => p[1]))), [35]);
  const lone = ctx.pooled([resetB, { weight: 1, resets: [], points: [[t0, 50, 0], [t0 + 3 * H, 50, 0]] }], limit, "points");
  assert.deepEqual(JSON.parse(JSON.stringify(lone.map((p) => p[0]))), [t0, t0 + 3 * H]);
});

test("an estimate falls between the samples around a gap and returns to full at a reset", () => {
  const ctx = pooledContext();
  const limit = 900, t0 = 1000000;
  const plain = (v) => JSON.parse(JSON.stringify(v));
  // The same cycle: straight from one sample to the next, never above the first.
  const falling = [[t0, 60, 0], [t0 + 4000, 40, 0]];
  assert.deepEqual(plain(ctx.gapValue(falling, [[t0, t0 + 9000]], t0 + 2000, limit)), { value: 50, estimated: true });
  const rising = [[t0, 51, 0], [t0 + 4000, 53, 1]];
  assert.equal(ctx.gapValue(rising, [[t0, t0 + 9000]], t0 + 2000, limit).value, 51);
  // A recent sample is measured.
  assert.deepEqual(plain(ctx.gapValue(falling, [], t0 + 600, limit)), { value: 60, estimated: false });
  assert.equal(ctx.gapValue(falling, [], t0 - 1, limit), null);
  // A reset within the gap: the last value until the reset, then from 100%
  // to the next sample.
  const across = [[t0, 10, 0], [t0 + 4000, 80, 0]];
  const resets = [[t0, t0 + 2000], [t0 + 4000, t0 + 20000]];
  assert.equal(ctx.gapValue(across, resets, t0 + 1999, limit).value, 10);
  assert.equal(ctx.gapValue(across, resets, t0 + 2000, limit).value, 100);
  assert.equal(ctx.gapValue(across, resets, t0 + 3000, limit).value, 90);
  // A rise by a reset of unknown time holds the last value.
  assert.equal(ctx.gapValue(across, [], t0 + 3000, limit).value, 10);
  // After the last sample: the last value until the reset, then 100%.
  assert.equal(ctx.gapValue([[t0, 30, 0]], [[t0, t0 + 2000]], t0 + 1500, limit).value, 30);
  assert.equal(ctx.gapValue([[t0, 30, 0]], [[t0, t0 + 2000]], t0 + 2500, limit).value, 100);
});

test("estimated stretches of a line are split off and share their ends", () => {
  const ctx = pooledContext();
  const plain = (v) => JSON.parse(JSON.stringify(v));
  const pts = [[1, 80, 0, 0, 0, 0], [2, 79, 0, 0, 0, 1], [3, 78, 0, 0, 0, 1], [4, 77, 0, 0, 0, 0], [5, 76, 0, 0, 0, 0]];
  assert.deepEqual(plain(ctx.estimatedRuns(pts).map((r) => [r.estimated, r.pts.map((p) => p[0])])),
    [[true, [1, 2, 3, 4]], [false, [4, 5]]]);
  assert.deepEqual(plain(ctx.estimatedRuns([[1, 50, 0]]).map((r) => [r.estimated, r.pts.length])), [[false, 1]]);
});

// ---- Page state kept in the config ----

const STATE_RANGES = {
  "5h": [["1h"], ["3h"], ["6h"], ["12h"], ["24h"], ["26h"]],
  "7d": [["24h"], ["4d"], ["8d"], ["15d"], ["1mo"], ["36d"]],
};
const stateFunctions = ["writePageState", "sendPageState", "forgetStores", "readOldMask", "restorePageState", "rememberChart", "rememberExpanded",
  "currentChartView", "saveChartFilters", "expandedGroups", "saveChartExpanded", "saveChartSwitch", "rangeFor", "restoreChartState"];

// The page state functions with a settings form of two switches and a
// config endpoint that records each PATCH and answers when told to.
function pageStateContext(switches = {}, globals = {}) {
  const writes = [], results = [], pressed = {}, pending = [];
  const form = {
    remember_chart_filters: { name: "remember_chart_filters", checked: !!switches.filters },
    remember_chart_expanded: { name: "remember_chart_expanded", checked: !!switches.expanded },
  };
  const ctx = context(stateFunctions, {
    RANGES: STATE_RANGES, DEFAULT_RANGE: { "5h": "6h", "7d": "8d" },
    rawConfig: {}, chartWindow: "5h", range: "6h", chartSort: "default", chartExpanded: {}, chartView: null,
    pageStateReady: false, chartRestored: false, pageStateWrites: {}, pageStateFailed: {},
    masked: false, showFiveHourResets: false,
    $: (id) => { assert.equal(id, "settings"); return { elements: form }; },
    windowKinds: () => ["5h", "7d"],
    pressSegment: (id, value) => { pressed[id] = value; },
    renderRangeButtons: () => { pressed.ranges = ctx.range; },
    setMasked: (on) => { ctx.masked = on; },
    translate: (key) => key,
    saveHost: () => ({}),
    claimState: (host, input) => (state) => { results.push(input.name + ":" + state); return true; },
    pageStateSaved: (group, err, switched) => { if (err) results.push(group + ":" + err.message + (switched ? ":switch" : "")); },
    patchConfig: (patch) => {
      writes.push(plain(patch));
      if (!ctx.hold) return Promise.resolve("");
      return new Promise((resolve, reject) => pending.push({ resolve, reject }));
    },
    ...globals,
  });
  // As the script loads.
  vm.runInContext(pageVar("OLD_MASK_STORE") + pageVar("OLD_STORES") + pageVar("oldMask"), ctx);
  return { ctx, form, writes, results, pressed, pending };
}
const settle = () => new Promise(setImmediate);

test("a chart switch saves with its state in one patch, and turning it off deletes both", async () => {
  const { ctx, form, writes, results } = pageStateContext();
  ctx.restoreChartState({});
  // Off: changes are not saved.
  ctx.chartWindow = "7d"; ctx.range = "15d"; ctx.saveChartFilters();
  assert.equal(writes.length, 0);
  form.remember_chart_filters.checked = true;
  ctx.saveChartSwitch(form.remember_chart_filters);
  await settle();
  assert.deepEqual(writes, [{ remember_chart_filters: true, chart_view: { window: "7d", sort: "default", ranges: { "7d": "15d" } } }]);
  assert.deepEqual(results, ["remember_chart_filters:saved"]);
  ctx.chartSort = "low"; ctx.saveChartFilters();
  await settle();
  assert.deepEqual(writes.at(-1), { chart_view: { window: "7d", sort: "low", ranges: { "7d": "15d" } } });
  form.remember_chart_filters.checked = false;
  ctx.saveChartSwitch(form.remember_chart_filters);
  await settle();
  assert.deepEqual(writes.at(-1), { remember_chart_filters: null, chart_view: null });
  form.remember_chart_expanded.checked = true;
  ctx.chartExpanded["group:claude|Claude"] = true;
  ctx.saveChartSwitch(form.remember_chart_expanded);
  form.remember_chart_expanded.checked = false;
  ctx.hold = false;
  await settle();
  ctx.saveChartSwitch(form.remember_chart_expanded);
  await settle();
  assert.deepEqual(writes.slice(-2), [{ remember_chart_expanded: true, chart_expanded: ["claude|Claude"] }, { remember_chart_expanded: null, chart_expanded: null }]);
  // The settings save leaves the switches to saveChartSwitch.
  assert.match(source, /if \(!top \|\| top === "remember_chart_filters" \|\| top === "remember_chart_expanded"\) return;/);
});

test("switch and state changes queue in order: one write at a time, later values winning", async () => {
  const { ctx, form, writes, pending, results } = pageStateContext({ filters: true });
  ctx.restoreChartState({ remember_chart_filters: true });
  ctx.hold = true;
  for (const r of ["1h", "3h", "12h", "24h"]) { ctx.range = r; ctx.saveChartFilters(); }
  assert.equal(writes.length, 1, "one write is under way");
  pending.shift().resolve("");
  await settle();
  assert.equal(writes.length, 2, "the waiting values collapse into the last one");
  assert.equal(writes[1].chart_view.ranges["5h"], "24h");
  // Turning the switch off while a write is under way deletes both last,
  // and a change made after that is not saved.
  form.remember_chart_filters.checked = false;
  ctx.saveChartSwitch(form.remember_chart_filters);
  ctx.range = "1h"; ctx.saveChartFilters();
  pending.shift().reject(new Error("offline"));
  await settle();
  assert.deepEqual(writes.at(-1), { remember_chart_filters: null, chart_view: null });
  pending.shift().resolve("");
  await settle();
  assert.equal(writes.length, 3);
  assert.deepEqual(results, ["chart_view:offline", "remember_chart_filters:saved"]);
  assert.equal(ctx.pageStateWrites.chart_view.running, null);
  // On again while a state write waits: the switch and the newest state go
  // in one patch.
  form.remember_chart_filters.checked = true;
  ctx.saveChartSwitch(form.remember_chart_filters);
  ctx.chartSort = "low"; ctx.saveChartFilters();
  ctx.range = "3h"; ctx.saveChartFilters();
  pending.shift().resolve("");
  await settle();
  assert.deepEqual(writes.at(-1), { chart_view: { window: "5h", sort: "low", ranges: { "5h": "3h" } } });
  assert.deepEqual(writes.at(-2), { remember_chart_filters: true, chart_view: { window: "5h", sort: "default", ranges: { "5h": "1h" } } });
});

test("the chart state is restored from the first valid config, switches included", () => {
  const view = { window: "7d", sort: "low", ranges: { "5h": "12h", "7d": "15d" } };
  const { ctx, form, pressed, writes } = pageStateContext();
  // The form was filled from a newer config; the restore follows its own.
  ctx.rawConfig = { remember_chart_filters: false };
  ctx.restoreChartState({ remember_chart_filters: true, chart_view: view, remember_chart_expanded: true, chart_expanded: ["claude|Claude"] });
  assert.equal(form.remember_chart_filters.checked, true);
  assert.equal(form.remember_chart_expanded.checked, true);
  assert.equal(ctx.chartWindow, "7d");
  assert.equal(ctx.chartSort, "low");
  assert.equal(ctx.range, "15d");
  assert.equal(ctx.rangeFor("5h"), "12h");
  assert.deepEqual(pressed, { "window-buttons": "7d", "sort-buttons": "low", ranges: "15d" });
  assert.deepEqual(plain(ctx.chartExpanded), { "group:claude|Claude": true });
  assert.equal(writes.length, 0, "restoring writes nothing");
  // Only once.
  ctx.restoreChartState({ remember_chart_filters: true, chart_view: { window: "5h" } });
  assert.equal(ctx.chartWindow, "7d");
});

test("a saved chart choice that is not offered falls back to the defaults", () => {
  const { ctx } = pageStateContext();
  ctx.restoreChartState({ remember_chart_filters: true, chart_view: { window: "30d", sort: "random", ranges: { "5h": "8d", "7d": 3 } } });
  assert.equal(ctx.chartWindow, "5h");
  assert.equal(ctx.chartSort, "default");
  assert.equal(ctx.range, "6h");
  assert.equal(ctx.rangeFor("7d"), "8d");
  // With the switch off the saved choice is ignored.
  const off = pageStateContext();
  off.ctx.restoreChartState({ chart_view: { window: "7d", sort: "low", ranges: { "7d": "15d" } }, chart_expanded: ["claude|Claude"] });
  assert.equal(off.ctx.chartWindow, "5h");
  assert.equal(off.ctx.rangeFor("7d"), "8d");
  assert.deepEqual(plain(off.ctx.chartExpanded), {});
});

test("a saved window that only the history still has is restored once its history is read, or falls back", async () => {
  const now = Date.now() / 1000;
  // With samples in the range, without, and when the history cannot be read
  // in LOAD_TIMEOUT; for 7d with accounts that report 5h, and for 5h, the
  // window the chart starts with, with accounts that report 7d.
  for (const [saved, answer, want, wantRange] of [["7d", "samples", "7d", "15d"], ["7d", "empty", "5h", "12h"], ["7d", "failed", "5h", "12h"],
    ["5h", "samples", "5h", "12h"], ["5h", "empty", "7d", "15d"], ["5h", "failed", "7d", "15d"]]) {
    const other = saved === "7d" ? "5h" : "7d";
    const requested = [], timeouts = [];
    const { ctx, pressed } = pageStateContext({ filters: true }, {
      status: { accounts: [{ groups: [{ windows: [{ kind: other }] }] }] },
      historyCache: {}, historyPending: {}, historySeq: 0, API: "api", pendingOrder: null, LOAD_TIMEOUT: 15000,
      $: (id) => id === "settings" ? { elements: { remember_chart_filters: { checked: true }, remember_chart_expanded: { checked: false } } }
        : { querySelectorAll: () => [] },
      api: async (method, p, body, timeout) => {
        requested.push(p);
        timeouts.push(timeout);
        if (answer === "failed") throw new Error("request_timed_out");
        const from = new Date((now - 15 * 86400) * 1000).toISOString();
        return { from, series: answer === "samples" ? [{ kind: saved, points: [[now - 600, 50]] }] : [] };
      },
    });
    for (const name of ["windowKinds", "recentSamples", "hiddenGroups", "groupHidden", "windowKind", "historyKey", "fetchHistory", "renderWindowButtons"]) {
      vm.runInContext(source.match(new RegExp("  function " + name + "\\([^]*?\\n  }"))[0], ctx);
    }
    const restored = ctx.restoreChartState({ remember_chart_filters: true, chart_view: { window: saved, sort: "low", ranges: { "7d": "15d", "5h": "12h" } } });
    // Nothing is drawn until the window is chosen; the sort is restored at once.
    assert.ok(restored && typeof restored.then === "function");
    assert.equal(ctx.chartSort, "low");
    assert.equal(ctx.chartWindow, "5h");
    await restored;
    assert.deepEqual(requested, ["api/history?range=" + (saved === "7d" ? "15d" : "12h") + "&window=" + saved]);
    assert.deepEqual(timeouts, [15000]);
    // The page then shows the window buttons (renderStatus).
    ctx.renderWindowButtons();
    assert.equal(ctx.chartWindow, want, saved + " " + answer);
    assert.equal(ctx.range, wantRange, saved + " " + answer);
    assert.equal(pressed["window-buttons"], want);
  }
  // The app stays hidden until then: preparePage waits for the restore.
  assert.match(source.match(/  function preparePage\([^]*?\n  }/)[0], /return Promise\.resolve\(restoreChartState\(cfg\)\);/);
});

// Pooled rows from the real chartRows: two Antigravity accounts share
// "Claude / GPT".
function pooledChartRows() {
  const now = 1795280000;
  const win = (remaining) => [{ id: "five-hour", kind: "5h", short: "5h", remaining, reset: "" }];
  const status = { accounts: [
    { auth_index: "4", provider: "antigravity", groups: [{ key: "antigravity:4:cg", label: "Claude / GPT", source_label: "Claude / GPT", windows: win(65) }] },
    { auth_index: "5", provider: "antigravity", groups: [{ key: "antigravity:5:cg", label: "Claude / GPT#b", source_label: "Claude / GPT", windows: win(20) }] },
    { auth_index: "3", provider: "claude", groups: [{ key: "claude:3:claude", label: "Claude", source_label: "Claude", windows: win(40) }] },
  ], group_order: [] };
  const ctx = context(["sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName", "groupTitle", "chartRows", "hiddenGroups", "groupHidden", "windowKind", "cycleFloor", "accountWeight", "pooledRow", "parseTime"], {
    status, providerTitle: (id) => ({ antigravity: "Antigravity" })[id] || id, rawConfig: {}, chartSort: "default", historyWindow: "5h", RESET_JUMP: 5,
    gapLimit: () => 900, pooled: () => [],
  });
  vm.runInContext(pageVar("SERVICE_SHORT"), ctx);
  const series = (s, value) => ({ ...s, window: "five-hour", window_label: "5h", kind: "5h", points: [[now - 600, value], [now, value]], resets: [] });
  ctx.history = { from: new Date((now - 6 * 3600) * 1000).toISOString(), series: [
    series({ group: "antigravity:4:cg", label: "Claude / GPT", source_label: "Claude / GPT", provider: "antigravity", auth_index: "4" }, 65),
    series({ group: "antigravity:5:cg", label: "Claude / GPT#b", source_label: "Claude / GPT", provider: "antigravity", auth_index: "5" }, 20),
    series({ group: "claude:3:claude", label: "Claude", source_label: "Claude", provider: "claude", auth_index: "3" }, 40),
  ] };
  return ctx.chartRows();
}

test("expanded groups are saved by row id without group:, and ids the chart lacks are kept", async () => {
  const rows = pooledChartRows();
  const pooled = rows.filter((r) => r.children);
  assert.deepEqual(plain(pooled.map((r) => r.id)), ["group:antigravity|Claude / GPT"]);
  const { ctx, form, writes } = pageStateContext({ expanded: true });
  ctx.restoreChartState({ remember_chart_expanded: true, chart_expanded: ["antigravity|Claude / GPT", "codex|Gone", 7] });
  // The saved id expands the real row; an id without a row stays.
  assert.equal(ctx.chartExpanded[pooled[0].id], true);
  assert.deepEqual(plain(ctx.chartExpanded), { "group:antigravity|Claude / GPT": true, "group:codex|Gone": true });
  // Collapsing the row, as a click on it does, saves the rest.
  ctx.chartExpanded[pooled[0].id] = false; ctx.saveChartExpanded();
  await settle();
  assert.deepEqual(writes.at(-1), { chart_expanded: ["codex|Gone"] });
  ctx.chartExpanded[pooled[0].id] = true; ctx.saveChartExpanded();
  await settle();
  assert.deepEqual(writes.at(-1), { chart_expanded: ["antigravity|Claude / GPT", "codex|Gone"] });
  ["group:codex|Gone", pooled[0].id].forEach((id) => { ctx.chartExpanded[id] = false; });
  ctx.saveChartExpanded();
  await settle();
  assert.deepEqual(writes.at(-1), { chart_expanded: null });
  // Off: nothing is saved.
  form.remember_chart_expanded.checked = false;
  ctx.chartExpanded[pooled[0].id] = true; ctx.saveChartExpanded();
  await settle();
  assert.equal(writes.length, 3);
  // The click on an expanded row saves.
  assert.match(source.match(/  function renderOverview\([^]*?\n  }/)[0], /chartExpanded\[r\.id\] = !chartExpanded\[r\.id\];\n\s*saveChartExpanded\(\);/);
});

// Stubs every name fn reads but globals lack with an object that answers
// any use, so the real drawing code runs.
function universal() {
  const p = new Proxy(function () {}, {
    get: (t, k) => k === Symbol.toPrimitive ? () => 0 : k === "length" ? 0 : (k === "then" || k === Symbol.iterator) ? undefined : p,
    set: () => true, apply: () => p, construct: () => p,
  });
  return p;
}
function runStubbed(ctx, call) {
  for (let i = 0; i < 200; i++) {
    try { return call(); } catch (err) {
      const m = /^(\w+) is not defined/.exec(err.message);
      if (!m) throw err;
      ctx[m[1]] = universal();
    }
  }
  throw new Error("too many names");
}

test("drawing the chart keeps the remembered range of each window", async () => {
  const { ctx, writes } = pageStateContext({ filters: true }, { chartSelected: "", hoverListeners: [] });
  vm.runInContext(source.match(/  function renderOverview\([^]*?\n  }/)[0], ctx);
  const draw = () => runStubbed(ctx, () => ctx.renderOverview(pooledChartRows(), { clientWidth: 900, appendChild() {} }, 0));
  ctx.restoreChartState({ remember_chart_filters: true });
  // As the range and window buttons do, drawing after each pick.
  const pickWindow = (value) => { ctx.chartWindow = value; ctx.range = ctx.rangeFor(value); ctx.saveChartFilters(); draw(); };
  ctx.range = "12h"; ctx.saveChartFilters(); draw();
  pickWindow("7d");
  assert.equal(ctx.range, "8d");
  pickWindow("5h");
  assert.equal(ctx.range, "12h");
  await settle();
  assert.deepEqual(writes.at(-1), { chart_view: { window: "5h", sort: "default", ranges: { "5h": "12h", "7d": "8d" } } });
  assert.ok(ctx.chartGeom, "the overview keeps its geometry apart");
  // The page's functions share one scope, so no two may declare one name.
  const names = [...source.matchAll(/\n  var (\w+)/g)].map((m) => m[1]);
  assert.deepEqual(names.filter((n, i) => names.indexOf(n) !== i), []);
});

test("masking and the 5-hour resets come from the first config only", async () => {
  const { ctx, writes } = pageStateContext();
  ctx.restorePageState({ mask_accounts: true, events_show_5h_resets: true });
  assert.equal(ctx.masked, true);
  assert.equal(ctx.showFiveHourResets, true);
  assert.equal(writes.length, 0);
  ctx.writePageState("mask_accounts", { mask_accounts: null });
  await settle();
  assert.deepEqual(writes, [{ mask_accounts: null }]);
  ctx.restorePageState({ mask_accounts: true, events_show_5h_resets: false });
  assert.equal(ctx.showFiveHourResets, true);
  assert.match(source, /writePageState\("mask_accounts", \{ mask_accounts: masked \|\| null \}\);/);
  assert.match(source, /writePageState\("events_show_5h_resets", \{ events_show_5h_resets: showFiveHourResets \|\| null \}\);/);
  assert.doesNotMatch(source, /\bMASK_STORE|EVENTS_STORE|MCP_URL_STORE|CHART_STORE|pageStateTouched/);
});

test("the masking switch of an earlier version moves to the config once, and the other old keys go", async () => {
  const OLD = { "lamplighter-mask-accounts": "1", "lamplighter-events-5h": "1", "lamplighter-chart-filters": "{}", "lamplighter-mcp-url": "x", "lamplighter-view": "settings" };
  const setup = (old, globals) => {
    const stored = { ...old };
    const state = pageStateContext({}, {
      localStorage: { getItem: (k) => k in stored ? stored[k] : null, removeItem: (k) => { delete stored[k]; } },
      ...globals,
    });
    return { ...state, stored };
  };
  // No mask_accounts in the config and the old switch on: masked at once,
  // saved quietly, and the old key goes once saved.
  let { ctx, writes, results, pending, stored } = setup(OLD, { hold: true });
  ctx.restorePageState({});
  assert.equal(ctx.masked, true);
  assert.deepEqual(writes, [{ mask_accounts: true }]);
  assert.deepEqual(Object.keys(stored).sort(), ["lamplighter-mask-accounts", "lamplighter-view"]);
  pending.shift().resolve("");
  await settle();
  assert.deepEqual(Object.keys(stored), ["lamplighter-view"], "the view stays in the browser");
  assert.deepEqual(results, []);
  // A failed save keeps the old key, so the next opening moves it again.
  ({ ctx, writes, pending, stored } = setup(OLD, { hold: true, pageStateSaved: () => {} }));
  ctx.restorePageState({});
  pending.shift().reject(new Error("down"));
  await settle();
  assert.equal(ctx.masked, true);
  assert.equal(stored["lamplighter-mask-accounts"], "1");
  // The config wins when it has mask_accounts; the old switch goes either way.
  for (const [cfg, old, want] of [[{ mask_accounts: false }, "1", false], [{ mask_accounts: true }, "0", true], [{}, "0", false], [{}, null, false]]) {
    ({ ctx, writes, stored } = setup(old === null ? {} : { "lamplighter-mask-accounts": old }));
    ctx.restorePageState(cfg);
    assert.equal(ctx.masked, want, JSON.stringify([cfg, old]));
    assert.deepEqual(writes, []);
    assert.deepEqual(stored, {});
  }
  // Without localStorage, as in Magpie, only the config counts.
  ({ ctx, writes } = setup({}, { localStorage: undefined }));
  ctx.restorePageState({});
  assert.equal(ctx.masked, false);
  assert.deepEqual(writes, []);
});

test("a second tab opened before the masking moves still masks with the config it read before", async () => {
  // Two tabs of one browser share the old key; both load before either
  // has its config.
  const stored = { "lamplighter-mask-accounts": "1" };
  const localStorage = { getItem: (k) => k in stored ? stored[k] : null, removeItem: (k) => { delete stored[k]; } };
  const first = pageStateContext({}, { localStorage });
  const second = pageStateContext({}, { localStorage });
  // The first tab moves the masking and deletes the key.
  first.ctx.restorePageState({});
  await settle();
  assert.equal(first.ctx.masked, true);
  assert.deepEqual(stored, {});
  // The second tab read its config before that save: still masked.
  second.ctx.restorePageState({});
  assert.equal(second.ctx.masked, true);
  assert.deepEqual(second.writes, [{ mask_accounts: true }]);
});

// loadPage with its real gate and the real api: the app, the loading
// card and the key form are plain objects, and each request waits until
// the test answers it.
function loadPageContext(globals = {}) {
  const nodes = {}, calls = [], timers = [], requests = [], stored = {};
  const node = (id) => nodes[id] || (nodes[id] = { id, hidden: true, textContent: "", value: "" });
  const ctx = context(["loadPage", "preparePage", "showPage", "hideContent", "showContent", "setLoadState", "renderLoadCard", "keyAsked",
    "askForKey", "cancelLoad", "submitKey", "validKey", "begin", "forgetStores", "readOldMask", "restorePageState", "api", "fetchStatus", "fetchConfig"], {
    pageLoad: null, pagePrepared: false, pageStateReady: false, contentReady: false,
    loadState: { state: "", message: "" }, loadCardTimer: null, LOAD_TIMEOUT: 15000, LOAD_CARD_DELAY: 300,
    key: "", KEY_STORE: "k", history: null, masked: false, showFiveHourResets: false, reloads: 0,
    localStorage: { getItem: (k) => k in stored ? stored[k] : null, setItem: (k, v) => { stored[k] = v; }, removeItem: (k) => { delete stored[k]; } },
    writePageState: (group, patch, say) => { calls.push("write:" + JSON.stringify(patch)); say("saved"); },
    location: { reload: () => { ctx.reloads++; } },
    API: "api", CONFIG_API: "api/config", PAGE_TOKEN: "", window: { AbortController }, AbortController, rawConfig: {},
    $: node, translate: (k, a) => a ? k + ":" + a : k,
    setTimeout: (fn, ms) => { timers.push([fn, ms]); return timers.length; }, clearTimeout: () => {},
    // A request ends when the test answers it or, unless it holds on, when
    // it is aborted.
    fetch: (url, opts) => new Promise((resolve, reject) => {
      const request = { url, opts, key: opts.headers.Authorization || "", holdOn: ctx.holdOn,
        answer: (body, code = 200) => resolve({ status: code, ok: code < 300, json: typeof body === "function" ? body : async () => body }),
        fail: reject };
      requests.push(request);
      if (opts.signal) opts.signal.addEventListener("abort", () => { if (!request.holdOn) reject(new Error("aborted")); });
    }),
    start: () => { nodes["key-section"].hidden = true; ctx.loadPage(); },
    useStatus: (data) => { ctx.status = data; calls.push("useStatus"); },
    useConfig: (data) => { ctx.rawConfig = data; calls.push("useConfig"); },
    setMasked: (on) => { ctx.masked = on; calls.push("mask:" + on); },
    renderStatus: () => calls.push("renderStatus:" + (nodes.app.hidden ? "hidden" : "shown") + ":" + ctx.masked),
    fillSettings: () => calls.push("fill"), checkUpdate: () => {},
    restoreChartState: (cfg) => { calls.push("restoreChart:" + JSON.stringify(cfg)); return null; },
    loadHistory: async () => { calls.push("history"); return false; }, prefetchHistory: () => {},
    ...globals,
  });
  vm.runInContext(pageVar("OLD_MASK_STORE") + pageVar("OLD_STORES") + pageVar("oldMask"), ctx);
  for (const id of ["app", "key-section", "key-input", "key-error", "load-section", "load-text", "load-retry"]) node(id);
  // The requests of the latest load: [status, config].
  const last = () => [requests.findLast((r) => r.url === "api/status"), requests.findLast((r) => r.url === "api/config")];
  // A key entered in the form; resolves as the load it starts on this
  // page, if any.
  const enterKey = (secret) => {
    nodes["key-input"].value = " " + secret + " ";
    ctx.submitKey({ preventDefault: () => {} });
    return ctx.pageLoad && ctx.pageLoad.promise;
  };
  return { ctx, nodes, calls, timers, requests, stored, last, enterKey };
}

test("the app shows only once a valid config has set the masking and the chart state is restored", async () => {
  let restored;
  const { ctx, nodes, calls, timers, requests, last } = loadPageContext();
  ctx.restoreChartState = (cfg) => {
    calls.push("restoreChart:" + JSON.stringify(cfg));
    return new Promise((resolve) => { restored = resolve; });
  };
  const load = ctx.loadPage();
  assert.equal(ctx.loadPage(), load, "one load runs at a time");
  assert.deepEqual(requests.map((r) => r.url), ["api/status", "api/config"]);
  assert.deepEqual(timers.filter((t) => t[1] === 15000).length, 2, "each request gives up after LOAD_TIMEOUT");
  // The loading card waits, so a quick start does not flash it.
  assert.equal(nodes["load-section"].hidden, true);
  timers.find((t) => t[1] === 300)[0]();
  assert.equal(nodes["load-section"].hidden, false);
  assert.equal(nodes["load-text"].textContent, "loading");
  const first = { mask_accounts: true, remember_chart_filters: true, chart_view: { window: "7d" } };
  last()[1].answer(first);
  await settle();
  assert.deepEqual(calls, [], "nothing is taken before both answer");
  last()[0].answer({});
  await settle();
  // The chart state, with the history it may read, is restored while the
  // app is hidden and the load holds its lock.
  assert.deepEqual(calls, ["useStatus", "useConfig", "mask:true", "restoreChart:" + JSON.stringify(first)]);
  assert.equal(nodes.app.hidden, true);
  assert.equal(nodes["load-section"].hidden, false);
  assert.equal(ctx.loadPage(), load);
  assert.equal(requests.length, 2);
  restored();
  assert.equal(await load, true);
  assert.equal(nodes.app.hidden, false);
  assert.equal(nodes["load-section"].hidden, true);
  assert.equal(ctx.pagePrepared, true);
  assert.equal(ctx.pageLoad, null);
  await settle();
  assert.deepEqual(calls.slice(4), ["renderStatus:shown:true", "fill", "history"], "the first chart loads after the page shows");
});

test("a load holds its lock until both requests end, and the first failure cancels the other", async () => {
  const { ctx, nodes, requests, last, timers } = loadPageContext();
  // A config request that does not stop at once when cancelled.
  ctx.holdOn = true;
  const load = ctx.loadPage();
  ctx.holdOn = false;
  const [status, config] = last();
  status.answer({ error: "down" }, 500);
  await settle();
  assert.equal(config.opts.signal.aborted, true, "the failure cancels the config request");
  // Until it ends, a retry joins the load instead of sending more requests.
  assert.equal(ctx.loadPage(), load);
  assert.equal(requests.length, 2);
  assert.notEqual(ctx.pageLoad, null);
  config.answer({});
  assert.equal(await load, false);
  assert.equal(ctx.pageLoad, null);
  assert.equal(nodes.app.hidden, true, "a config without its status shows nothing");
  assert.equal(nodes["load-text"].textContent, "load_failed:down");
  assert.equal(nodes["load-retry"].hidden, false);
  // A request that hangs gives up at LOAD_TIMEOUT and cancels the other.
  const retry = ctx.loadPage();
  assert.equal(requests.length, 4);
  timers.filter((t) => t[1] === 15000).at(-2)[0]();
  assert.equal(await retry, false);
  assert.equal(last()[1].opts.signal.aborted, true);
  assert.equal(nodes["load-text"].textContent, "load_failed:request_timed_out");
  // The history is not part of the load: a history that never answers
  // leaves the lock free.
  ctx.loadHistory = () => new Promise(() => {});
  const shown = ctx.loadPage();
  last()[0].answer({});
  last()[1].answer({});
  assert.equal(await shown, true);
  assert.equal(ctx.pageLoad, null);
});

test("a config that is not an object, such as a proxy's page, keeps the app hidden until a retry reads one", async () => {
  const bodies = [async () => { throw new SyntaxError("Unexpected token <"); }, null, ["a"]];
  for (const body of bodies) {
    const { ctx, nodes, last } = loadPageContext();
    const load = ctx.loadPage();
    last()[0].answer({});
    last()[1].answer(body);
    assert.equal(await load, false);
    assert.equal(ctx.contentReady, false);
    assert.equal(nodes.app.hidden, true);
    assert.equal(ctx.pageStateReady, false);
    assert.deepEqual(plain(ctx.rawConfig), {});
    assert.equal(nodes["load-section"].hidden, false);
    assert.equal(nodes["load-text"].textContent, "load_failed:bad_response");
    assert.equal(nodes["load-retry"].hidden, false);
    // Retry, as the button does.
    const retry = ctx.loadPage();
    last()[0].answer({});
    last()[1].answer({});
    await retry;
    assert.equal(nodes.app.hidden, false);
    assert.equal(nodes["load-section"].hidden, true);
  }
  assert.match(source, /\$\("load-retry"\)\.addEventListener\("click", function \(\) \{ loadPage\(\); \}\);/);
  assert.match(pageHtml(), /<div id="load-section" class="card" hidden>/);
  assert.match(pageHtml(), /<button id="load-retry"[^>]*data-i18n="retry"/);
});

test("a 401 before any answers asks for the key, and the key entered is stored and loads the page anew", async () => {
  const { ctx, nodes, calls, requests, stored, last, enterKey } = loadPageContext();
  // Opened without a key: the status answers 401, the config is still on
  // its way and will not stop when cancelled.
  ctx.holdOn = true;
  const keyless = ctx.loadPage();
  ctx.holdOn = false;
  const [status, config] = last();
  status.answer({ error: "unauthorized" }, 401);
  await settle();
  assert.equal(nodes["key-section"].hidden, false);
  assert.equal(nodes["key-error"].hidden, true);
  assert.equal(nodes["load-section"].hidden, true, "the key form shows without a failure");
  assert.equal(ctx.pageLoad, null);
  assert.equal(config.opts.signal.aborted, true, "the load is cancelled");
  assert.equal(ctx.reloads, 0);
  // The key entered is stored and the page loads anew with it.
  assert.equal(enterKey("right"), null);
  assert.equal(stored.k, "right");
  assert.equal(ctx.reloads, 1);
  assert.equal(requests.length, 2, "this page sends nothing with the key");
  config.answer({ mask_accounts: false });
  assert.equal(await keyless, false);
  assert.deepEqual(calls, []);
  // The reloaded page sends the stored key; a wrong one asks again, with
  // the reason, and does not reload.
  const other = loadPageContext();
  other.ctx.key = other.stored.k = "wrong";
  const wrong = other.ctx.loadPage();
  other.last()[0].answer({}, 401);
  assert.equal(await wrong, false);
  assert.equal(other.nodes["key-error"].textContent, "key_invalid");
  assert.equal(other.nodes["key-error"].hidden, false);
  assert.equal(other.nodes.app.hidden, true);
  assert.equal(other.stored.k, undefined);
  assert.equal(other.ctx.reloads, 0);
});

test("without localStorage the key entered loads on the page, which has taken nothing", async () => {
  const { ctx, nodes, calls, last, enterKey } = loadPageContext();
  ctx.localStorage = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); }, removeItem: () => { throw new Error("denied"); } };
  ctx.holdOn = true;
  const keyless = ctx.loadPage();
  ctx.holdOn = false;
  const [status, config] = last();
  status.answer({}, 401);
  await settle();
  assert.equal(nodes["key-section"].hidden, false);
  const load = enterKey("right");
  assert.equal(ctx.reloads, 0);
  assert.equal(nodes["key-section"].hidden, true);
  assert.deepEqual(last().map((r) => r.key), ["Bearer right", "Bearer right"]);
  // The old config arriving then changes nothing.
  config.answer({ mask_accounts: false });
  assert.equal(await keyless, false);
  assert.deepEqual(calls, []);
  last()[1].answer({ mask_accounts: true });
  last()[0].answer({});
  assert.equal(await load, true);
  assert.equal(nodes.app.hidden, false);
  assert.equal(ctx.masked, true);
});

test("once the page has taken the answers of a key, a 401 reloads it and keeps the stored key, so it asks with the reason", async () => {
  // Before it shows: the chart state is still being restored when a
  // request of the page, such as the language report, answers 401.
  let restored;
  const early = loadPageContext({ restoreChartState: () => new Promise((resolve) => { restored = resolve; }) });
  early.ctx.key = early.stored.k = "old";
  const loading = early.ctx.loadPage();
  early.last()[0].answer({});
  early.last()[1].answer({ mask_accounts: false });
  await settle();
  const report = early.ctx.api("POST", "api/language", {});
  early.requests.at(-1).answer({ error: "unauthorized" }, 401);
  await assert.rejects(report, /unauthorized/);
  assert.equal(early.ctx.reloads, 1);
  assert.equal(early.stored.k, "old");
  assert.equal(early.nodes["key-section"].hidden, true, "the reloaded page asks for the key");
  // The load is dropped: it does not show the page while it reloads.
  restored();
  assert.equal(await loading, false);
  assert.equal(early.nodes.app.hidden, true);
  assert.equal(early.calls.some((c) => c.startsWith("renderStatus")), false);

  // After it has shown.
  const { ctx, nodes, requests, stored, last } = loadPageContext();
  ctx.key = stored.k = "old";
  const load = ctx.loadPage();
  last()[0].answer({});
  last()[1].answer({ mask_accounts: false });
  assert.equal(await load, true);
  const again = ctx.api("POST", "api/language", {});
  requests.at(-1).answer({ error: "unauthorized" }, 401);
  await assert.rejects(again, /unauthorized/);
  assert.equal(ctx.reloads, 1);
  assert.equal(nodes.app.hidden, true);
  assert.equal(nodes["key-section"].hidden, true);
  assert.equal(stored.k, "old");
  // After the reload the stored key answers 401 before any answers, which
  // asks with the reason (see the tests above).
});

test("a key a header cannot carry is not taken, and a stored one is asked for again at once", async () => {
  const { ctx, nodes, requests, stored, enterKey } = loadPageContext();
  ctx.askForKey("");
  for (const bad of ["密钥abc", "abc😀", "ab\tc", "abcé", "ab\x7fc"]) {
    assert.equal(enterKey(bad), null);
    assert.equal(stored.k, undefined, bad);
    assert.equal(ctx.reloads, 0);
    assert.equal(nodes["key-error"].hidden, false);
    assert.equal(nodes["key-error"].textContent, "key_format");
    assert.equal(nodes["key-section"].hidden, false);
  }
  assert.equal(requests.length, 0);
  // Spaces inside a key, as in a hand-written admin_key, are kept.
  enterKey("llad_A-z0~! b");
  assert.equal(stored.k, "llad_A-z0~! b");
  assert.equal(ctx.reloads, 1);
  // A bad key stored earlier, such as by an older page: asked for with
  // the reason, without a request, a failure card or a retry.
  const other = loadPageContext();
  other.stored.k = "坏的key";
  other.ctx.begin();
  assert.equal(other.requests.length, 0);
  assert.equal(other.stored.k, undefined);
  assert.equal(other.ctx.key, "");
  assert.equal(other.nodes["key-section"].hidden, false);
  assert.equal(other.nodes["key-error"].textContent, "key_invalid");
  assert.equal(other.nodes["load-section"].hidden, true);
  // A good stored key starts the load with it.
  for (const stored of ["good", "my admin key"]) {
    const good = loadPageContext();
    good.stored.k = stored;
    good.ctx.begin();
    assert.deepEqual(good.requests.map((r) => r.key), ["Bearer " + stored, "Bearer " + stored]);
    assert.equal(good.nodes["key-section"].hidden, true);
  }
  assert.match(source, /\n  begin\(\);\n\}\)\(\);\s*$/);
});

test("the old masking switch masks before the page shows", async () => {
  const { ctx, calls, stored, last } = loadPageContext();
  stored["lamplighter-mask-accounts"] = "1";
  ctx.oldMask = ctx.readOldMask();
  const load = ctx.loadPage();
  last()[0].answer({});
  last()[1].answer({});
  assert.equal(await load, true);
  await settle();
  assert.deepEqual(calls.slice(0, 6), ["useStatus", "useConfig", "mask:true", 'write:{"mask_accounts":true}', "restoreChart:{}", "renderStatus:shown:true"]);
  assert.equal(stored["lamplighter-mask-accounts"], undefined);
});

test("nothing that names an account is drawn while the content is not ready", () => {
  for (const name of ["renderStatus", "renderChart"]) {
    const body = source.match(new RegExp("  function " + name + "\\(\\) \\{\\n([^]*?)\\n  }"))[1];
    assert.match(body.split("\n").filter((l) => !/^\s*\/\//.test(l))[0], /^\s*if \(!contentReady\) return;$/, name);
  }
  let drawn = 0;
  const ctx = context(["renderStatus"], { contentReady: false, sortAccounts: () => { drawn++; } });
  ctx.renderStatus();
  assert.equal(drawn, 0);
  assert.match(source, /if \(status && contentReady\) renderEvents\(\);/);
  // The timer and a return to the page retry while the content is not ready.
  assert.match(source, /if \(document\.hidden \|\| !magpie\.visible \|\| keyAsked\(\)\) return;\n\s*if \(!contentReady\) loadPage\(\);/);
  assert.match(pageHtml(), /<main id="app" hidden>/);
});

test("a failed page state shows once in a toast and stays at the chart switch until it saves", () => {
  const toasts = [];
  const heading = saveNode("LEGEND");
  const input = saveNode("INPUT", { name: "remember_chart_filters" });
  const ctx = stateContext(["pageStateSaved"], {
    PAGE_STATE_SWITCH: { chart_view: "remember_chart_filters" }, pageStateFailed: {},
    $: () => ({ elements: { remember_chart_filters: input } }),
    toast: (text) => toasts.push(text),
  });
  ctx.saveHost = () => heading;
  ctx.translate = ((base) => (key, ...args) => key === "state_save_failed" ? "not saved: " + args[0] : base(key, ...args))(ctx.translate);
  // The switch's own save succeeded; the state then fails twice.
  ctx.setSaveState(heading, "saved", "saved", input);
  ctx.pageStateSaved("chart_view", new Error("offline"));
  ctx.pageStateSaved("chart_view", new Error("offline"));
  assert.deepEqual(toasts, ["not saved: offline"]);
  assert.match(heading.textContent, /remember_chart_filters save failed: offline/);
  assert.equal(input.getAttribute("aria-invalid"), "true");
  // A later save of the switch keeps the failure; a saved state clears it.
  ctx.setSaveState(heading, "saved", "saved", input);
  assert.equal(input.getAttribute("aria-invalid"), "true");
  ctx.pageStateSaved("chart_view", null);
  assert.equal(input.getAttribute("aria-invalid"), null);
  assert.equal(heading.textContent, "saved");
  // A write that carries the switch shows at the switch only.
  ctx.pageStateSaved("chart_view", new Error("down"), true);
  assert.equal(toasts.length, 1);
  assert.equal(input.getAttribute("aria-invalid"), null);
  // Masking and the resets have no switch: a toast only.
  ctx.pageStateSaved("mask_accounts", new Error("offline"));
  assert.equal(toasts.length, 2);
  ctx.pageStateSaved("mask_accounts", new Error("unauthorized"));
  assert.equal(toasts.length, 2);
});

test("a patch of page state only goes without a config token and needs no applying", async () => {
  const sent = [];
  let answer = (body) => body;
  const ctx = context(["patchConfig", "keepSaved"], {
    CONFIG_API: "api/config", savedTokens: [], newConfigToken: () => "t1", pendingOrder: null,
    rawConfig: { mask_accounts: true, chart_view: { window: "7d" }, bark_url: "a" },
    api: async (method, p, body) => { sent.push(plain(body)); return answer(body); },
  });
  vm.runInContext(pageVar("PAGE_STATE_KEYS"), ctx);
  // The plugin answers with the saved config: rawConfig takes the patched
  // keys from it, as saved, and keeps the others.
  answer = () => ({ mcp_public_url: "x.com", remember_chart_filters: true });
  assert.equal(await ctx.patchConfig({ mask_accounts: null, mcp_public_url: " x.com ", chart_view: { window: "5h" } }), "");
  assert.deepEqual(plain(ctx.rawConfig), { mcp_public_url: "x.com", bark_url: "a" });
  answer = () => null;
  assert.equal(await ctx.patchConfig({ mask_accounts: true, bark_url: "b" }), "t1");
  assert.deepEqual(sent, [{ mask_accounts: null, mcp_public_url: " x.com ", chart_view: { window: "5h" } }, { mask_accounts: true, bark_url: "b", config_token: "t1" }]);
  assert.deepEqual(plain(ctx.savedTokens), ["t1"]);
  assert.deepEqual(plain(ctx.rawConfig), { mcp_public_url: "x.com", bark_url: "b", mask_accounts: true }, "without a config in the answer, the sent values");
  // The same keys as config.IsPageState.
  const goKeys = [...fs.readFileSync(path.join(__dirname, "..", "config", "config.go"), "utf8")
    .match(/var pageStateKeys = map\[string\]bool\{([^}]*)\}/)[1].matchAll(/"(\w+)"/g)].map((m) => m[1]);
  assert.deepEqual([...ctx.PAGE_STATE_KEYS].sort(), goKeys.sort());
  const said = [];
  const applied = context(["showApplied"], { translate: (k) => k });
  assert.equal(await applied.showApplied((state, text) => said.push(state + ":" + text), ""), true);
  assert.deepEqual(said, ["saved:save_saved"]);
});

// publicUrl with the expressions it uses, and the functions names.
function publicUrlContext(names = [], globals = {}) {
  const ctx = context(["publicUrl", ...names], globals);
  for (const name of ["PUBLIC_URL_TRIM", "PUBLIC_URL_FORM"]) vm.runInContext(pageVar(name), ctx);
  return ctx;
}

test("the public MCP address is a setting that accepts only http and https addresses", async () => {
  const url = publicUrlContext();
  // The cases config.MCPPublicURLValid is tested with: both accept the
  // same addresses.
  const cases = JSON.parse(fs.readFileSync(path.join(__dirname, "..", "config", "testdata", "public_urls.json"), "utf8"));
  assert.ok(cases.length > 30);
  for (const [value, want] of cases) assert.equal(url.publicUrl(value), want, JSON.stringify(value));
  // The expressions are those of the Go check.
  const go = fs.readFileSync(path.join(__dirname, "..", "config", "config.go"), "utf8");
  assert.equal(url.PUBLIC_URL_FORM.source.replace(/\\\//g, "/"), go.match(/publicURLForm += regexp\.MustCompile\(`([^`]*)`\)/)[1]);
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: { mcp_public_url: "https://old.example.com" }, status: { config: {} },
    $: () => ({ elements: [{ name: "mcp_public_url", value: "  ", getAttribute: () => null, type: "text" }] }),
  });
  assert.deepEqual(plain(ctx.buildPatch("mcp_public_url")), { mcp_public_url: null });
  const field = { name: "mcp_public_url", value: "mcp.example.com/mcp/", getAttribute: () => null, type: "text" };
  const base = publicUrlContext(["mcpBaseUrl"], {
    $: () => ({ elements: { mcp_public_url: field, public_listen: { value: "0.0.0.0:8318" } } }),
    status: null, publicState: () => null, MCP_LISTEN: "127.0.0.1:8318", location: { hostname: "nas" },
  });
  assert.equal(base.mcpBaseUrl(), "https://mcp.example.com");
  field.value = "";
  assert.equal(base.mcpBaseUrl(), "http://nas:8318");
  field.value = "ftp://x.com";
  assert.equal(base.mcpBaseUrl(), "http://nas:8318", "an invalid address is not offered to clients");
  // /mcp comes off the path only, never off a host named mcp.
  for (const [value, want] of [["mcp", "https://mcp"], ["mcp/", "https://mcp"], ["mcp/mcp", "https://mcp"], ["http://mcp:8318/mcp/", "http://mcp:8318"],
    ["https://x.com/a/mcp//", "https://x.com/a"], ["https://x.com/mcpx", "https://x.com/mcpx"], ["https://x.com/a/", "https://x.com/a"]]) {
    field.value = value;
    assert.equal(base.mcpBaseUrl(), want, value);
  }
  assert.match(pageHtml(), /<input id="mcp-public-url" name="mcp_public_url"/);
  const effective = context(["effectiveConfig"], { status: { config: {} }, rawConfig: { mcp_public_url: "https://mcp.example.com" }, MCP_LISTEN: "x" });
  assert.equal(effective.effectiveConfig().mcp_public_url, "https://mcp.example.com");

  // Saving refuses an invalid address before any request.
  const heading = saveNode("LEGEND");
  const input = saveNode("INPUT", { name: "mcp_public_url" });
  input.type = "text";
  input.value = "ftp://x.com";
  input.checkValidity = () => true;
  const sent = [];
  const save = stateContext(["saveField", "captureIntent", "buildPatch", "inputTop", "topKey", "setPath", "getPath", "noteEdit", "rereadConfig"], {
    rawConfig: {}, status: { config: {} }, saveQueue: Promise.resolve(), THRESHOLDS: [],
    $: () => ({ elements: [input] }), renderIgnitionOff: () => {}, autofilled: () => false,
    patchConfig: async (patch) => { sent.push(plain(patch)); return ""; },
    showApplied: async (say) => { say("saved", "save_saved"); return true; }, refillSaved: () => {}, loadConfig: async () => {},
  });
  for (const name of ["PUBLIC_URL_TRIM", "PUBLIC_URL_FORM"]) vm.runInContext(pageVar(name), save);
  vm.runInContext(source.match(/  function publicUrl\([^]*?\n  }/)[0], save);
  save.saveHost = () => heading;
  save.translate = ((base) => (key, ...args) => key === "mcp_public_url_invalid" ? "not an http address" : base(key, ...args))(save.translate);
  save.saveField({ target: input });
  await save.saveQueue;
  assert.deepEqual(sent, []);
  assert.match(heading.textContent, /mcp_public_url save failed: not an http address/);
  input.value = "mcp.example.com";
  save.saveField({ target: input });
  await save.saveQueue;
  assert.deepEqual(sent, [{ mcp_public_url: "mcp.example.com" }]);
});

test("a saved public address stays in the field while the config is read again", async () => {
  const heading = saveNode("LEGEND");
  const input = saveNode("INPUT", { name: "mcp_public_url" });
  input.type = "text";
  input.value = "https://old.example.com";
  const sent = [], gets = [];
  let answer = null;
  const ctx = publicUrlContext(["saveField", "captureIntent", "buildPatch", "inputTop", "topKey", "setPath", "getPath", "noteEdit", "rereadConfig",
    "patchConfig", "keepSaved", "showApplied", "refillSaved", "fillSettings", "fillInput", "effectiveConfig", ...saveFunctions], {
    rawConfig: { mcp_public_url: "https://old.example.com" }, status: { config: {} }, saveQueue: Promise.resolve(), THRESHOLDS: [],
    savedTokens: [], pendingOrder: null, CONFIG_API: "api/config", MCP_LISTEN: "127.0.0.1:8318", newConfigToken: () => "t",
    saveSerial: 0, translate: (k) => k, saveText: (t) => t, saveItemLabel: (i) => i.name, el: (tag, attrs) => saveNode(tag.toUpperCase(), attrs),
    clear: (n) => { n.textContent = ""; }, maskedNodes: (t) => [{ textContent: t }],
    $: (id) => ({ settings: { elements: [input] }, "timezone-note": {} })[id], document: { activeElement: null, querySelectorAll: () => [] },
    PAGE_STATE_KEYS: [], renderIgnitionOff: () => {}, autofilled: () => false, providersWithGroups: () => [], renderTimezone: () => {},
    timezoneText: () => "", renderMcp: () => {},
    // The plugin answers with the saved config, trimmed; the GET after the
    // save answers only when the test says so.
    api: (method, p, body) => { sent.push(plain(body)); return answer || Promise.resolve(Object.assign({}, body.mcp_public_url ? { mcp_public_url: body.mcp_public_url.trim() } : {})); },
    loadConfig: () => new Promise((resolve, reject) => gets.push({ resolve, reject })),
  });
  vm.runInContext(pageVar("PAGE_STATE_KEYS"), ctx);
  ctx.saveHost = () => heading;
  const save = async (value) => {
    input.value = value;
    ctx.noteEdit(input);
    input.settingsDirty = true;
    ctx.saveField({ target: input });
    await settle();
  };
  await save("  new.example.com ");
  assert.deepEqual(sent.at(-1), { mcp_public_url: "new.example.com" });
  assert.equal(gets.length, 1, "the GET is still on its way");
  assert.equal(input.value, "new.example.com", "the field shows the saved value, not the one before");
  assert.equal(ctx.rawConfig.mcp_public_url, "new.example.com");
  assert.match(heading.textContent, /save_saved/);
  // The save queue goes on once the GET answers.
  gets.shift().resolve();
  await save("");
  assert.deepEqual(sent.at(-1), { mcp_public_url: null });
  assert.equal(input.value, "");
  assert.equal("mcp_public_url" in ctx.rawConfig, false);
  gets.shift().resolve();
  // Typing again while the save is under way: the new text stays.
  let reply;
  answer = new Promise((resolve) => { reply = resolve; });
  await save("a.example.com");
  input.value = "b.example.com";
  ctx.noteEdit(input);
  reply({ mcp_public_url: "a.example.com" });
  await settle();
  assert.equal(input.value, "b.example.com");
  assert.equal(input.settingsDirty, true);
  gets.shift().resolve();
  // A save stands when the config cannot be read again after it.
  answer = null;
  await save("c.example.com");
  gets.shift().reject(new Error("down"));
  await settle();
  assert.match(heading.textContent, /save_saved/);
  assert.doesNotMatch(heading.textContent, /fail/);
  assert.equal(ctx.rawConfig.mcp_public_url, "c.example.com");
  // So do the other saves: none waits on loadConfig for its result.
  assert.doesNotMatch(source, /return loadConfig\(\);/);
  assert.match(source, /function rereadConfig\(\) \{\n\s*return loadConfig\(\)\.catch\(function \(\) \{\}\);/);
});

test("the chart switches are not refilled after the first restore", () => {
  const filters = { name: "remember_chart_filters", type: "checkbox", checked: true, getAttribute: () => null, hasAttribute: () => false };
  const other = { name: "mask_accounts_note", type: "checkbox", checked: false, getAttribute: () => null, hasAttribute: () => false };
  const ctx = context(["fillSettings", "fillInput", "getPath"], {
    effectiveConfig: () => ({ remember_chart_filters: false, mask_accounts_note: true }), status: {}, rawConfig: {},
    document: { activeElement: null, querySelectorAll: () => [] }, providersWithGroups: () => [],
    $: (id) => id === "settings" ? { elements: [filters, other] } : {}, saveControls: () => [],
    renderTimezone: () => {}, timezoneText: () => "", renderIgnitionOff: () => {}, renderMcp: () => {}, renderAllShownGroups: () => {},
  });
  // Signed in again with a config whose switch is off: the switch keeps the
  // state this page restored and saves.
  ctx.fillSettings();
  assert.equal(filters.checked, true);
  assert.equal(other.checked, true);
});

test("the client commands quote the address and the key for the shell, and the JSON is built by JSON.stringify", () => {
  const { execFileSync } = require("node:child_process");
  const ctx = context(["mcpSnippets", "shellQuote"]);
  const url = "https://x.com/'$(touch /tmp/pwned)'`id`/mcp", secret = "k'ey";
  const snippets = Object.fromEntries(ctx.mcpSnippets(url, secret));
  assert.equal(ctx.shellQuote("a'b"), "'a'\\''b'");
  // The shell reads each command back as the words it was built from.
  const words = (command) => execFileSync("sh", ["-c", "for w in " + command.replace(/^\S+ /, "") + "; do printf '%s\\n' \"$w\"; done"], { encoding: "utf8" }).trim().split("\n");
  assert.deepEqual(words(snippets.mcp_claude), ["mcp", "add", "--transport", "http", "lamplighter", url, "--header", "Authorization: Bearer k'ey"]);
  assert.deepEqual(words(snippets.mcp_stdio), ["mcp-remote", url, "--header", "Authorization: Bearer k'ey"]);
  assert.deepEqual(JSON.parse(snippets.mcp_json), { mcpServers: { lamplighter: { type: "http", url, headers: { Authorization: "Bearer k'ey" } } } });
});

// ---- Hidden quota groups ----

const HIDDEN = { config: { hidden_groups: ["antigravity|Claude / GPT"] } };

test("a hidden quota group leaves its card block, and an account with only hidden groups its card", () => {
  const cards = saveNode("DIV");
  const element = (tag, attrs = {}, children = []) => {
    const node = saveNode(tag.toUpperCase(), attrs);
    if (attrs.text) node.textContent = attrs.text;
    children.filter(Boolean).forEach(child => node.appendChild(child));
    return node;
  };
  const win = [{ id: "five-hour", kind: "5h" }];
  const ctx = context(["renderAccounts", "hiddenGroups", "groupHidden"], {
    accountDrag: null, accountsBehind: false, SENSITIVE: "account-sensitive",
    status: { ...HIDDEN, accounts: [
      { name: "ag", provider: "antigravity", title: "Antigravity", groups: [
        { key: "antigravity:4:gemini", source_label: "Gemini", windows: win },
        { key: "antigravity:4:cg", source_label: "Claude / GPT", windows: win },
      ] },
      { name: "ag-cg", provider: "antigravity", title: "Antigravity#b", groups: [{ key: "antigravity:5:cg", source_label: "Claude / GPT", windows: win }] },
      { name: "broken", provider: "codex", title: "GPT", error: "invalid", groups: [] },
    ] },
    rawConfig: {}, masked: false, renderAccountSettings: () => {}, accountWeight: () => 1,
    fmtWeight: String, parseTime: () => null, $: () => cards, providerTitle: (id) => ({ antigravity: "Antigravity", codex: "GPT" })[id],
    clear: node => { node.textContent = ""; }, el: element, translate: (key) => key,
    maskedNodes: text => [element("span", { text })], snapshotButton: () => element("button"),
    quotaRow: (group, w) => element("div", { class: "quota-row", text: group.source_label }), freshness: () => null,
  });
  ctx.renderAccounts();
  assert.deepEqual(cards.children.map(card => card.attrs["data-account"]), ["ag", "broken"]);
  const blocks = cards.children[0].children.filter(node => node.attrs.class === "quota-group");
  assert.equal(blocks.length, 1);
  // The one group left keeps its title.
  assert.equal(blocks[0].children[0].attrs.class, "group-title");
  assert.equal(blocks[0].children[0].textContent, "Gemini");
  ctx.status.config = {};
  ctx.renderAccounts();
  assert.deepEqual(cards.children.map(card => card.attrs["data-account"]), ["ag", "ag-cg", "broken"]);
});

test("a hidden quota group has no chart rows, total or table rows, also from history alone", () => {
  const now = 1795280000;
  const win = (remaining) => [{ id: "five-hour", kind: "5h", short: "5h", remaining, reset: "" }];
  const status = { ...HIDDEN, accounts: [
    { auth_index: "4", provider: "antigravity", groups: [
      { key: "antigravity:4:gemini", label: "Gemini", source_label: "Gemini", windows: win(90) },
      { key: "antigravity:4:cg", label: "Claude / GPT", source_label: "Claude / GPT", windows: win(5) },
    ] },
    { auth_index: "5", provider: "antigravity", groups: [{ key: "antigravity:5:cg", label: "Claude / GPT#b", source_label: "Claude / GPT", windows: win(1) }] },
    { auth_index: "3", provider: "claude", groups: [{ key: "claude:3:claude", label: "Claude", source_label: "Claude", windows: win(40) }] },
  ], group_order: [] };
  const ctx = context(["sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName", "groupTitle", "chartRows", "hiddenGroups", "groupHidden",
    "windowKind", "cycleFloor", "accountWeight", "pooledRow", "parseTime", "findRow", "windowKinds", "recentSamples", "historyKey"], {
    status, providerTitle: (id) => ({ antigravity: "Antigravity" })[id] || id, rawConfig: {}, chartSort: "low", historyWindow: "5h", RESET_JUMP: 5,
    gapLimit: () => 900, pooled: () => [], historyCache: {}, chartWindow: "5h", range: "6h", rangeFor: () => "6h",
  });
  vm.runInContext(pageVar("SERVICE_SHORT"), ctx);
  const series = (s, value) => ({ ...s, window: "five-hour", window_label: "5h", kind: "5h", points: [[now - 600, value], [now, value]], resets: [] });
  ctx.history = { from: new Date((now - 6 * 3600) * 1000).toISOString(), series: [
    series({ group: "antigravity:4:gemini", label: "Gemini", source_label: "Gemini", provider: "antigravity", auth_index: "4" }, 90),
    series({ group: "antigravity:4:cg", label: "Claude / GPT", source_label: "Claude / GPT", provider: "antigravity", auth_index: "4" }, 5),
    series({ group: "antigravity:5:cg", label: "Claude / GPT#b", source_label: "Claude / GPT", provider: "antigravity", auth_index: "5" }, 1),
    // An account gone from the status whose group the history still names.
    series({ group: "antigravity:9:cg", label: "Claude / GPT#old", source_label: "Claude / GPT", provider: "antigravity", auth_index: "9" }, 0),
    series({ group: "claude:3:claude", label: "Claude", source_label: "Claude", provider: "claude", auth_index: "3" }, 40),
  ] };
  // "Lowest first" orders the rows left, unaffected by the hidden 0% to 5%.
  const rows = ctx.chartRows();
  assert.deepEqual(plain(rows.map((r) => r.id)), ["claude:3:claude|five-hour", "antigravity:4:gemini|five-hour"]);
  assert.equal(rows.some((r) => r.children), false);
  // The detail of a hidden row is not found; hiddenChartContext draws it.
  assert.equal(ctx.findRow(rows, "group:antigravity|Claude / GPT"), null);
  // Shown again, the rows come back with their history.
  ctx.status.config = {};
  ctx.chartSort = "default";
  assert.deepEqual(plain(ctx.chartRows().map((r) => r.id)),
    ["antigravity:4:gemini|five-hour", "group:antigravity|Claude / GPT", "antigravity:9:cg|five-hour", "claude:3:claude|five-hour"]);

  // A window kind only hidden groups report is not offered.
  ctx.status = { config: { hidden_groups: ["antigravity|Claude / GPT"] }, accounts: [{ provider: "antigravity", groups: [
    { source_label: "Gemini", windows: [{ kind: "5h" }] },
    { source_label: "Claude / GPT", windows: [{ kind: "5h" }, { kind: "7d" }] },
  ] }] };
  assert.deepEqual(plain(ctx.windowKinds()), ["5h"]);
  ctx.status.config = {};
  assert.deepEqual(plain(ctx.windowKinds()), ["5h", "7d"]);
  // Every window known is of a hidden group: no window is offered, also
  // with history of hidden groups alone.
  ctx.status = { config: { hidden_groups: ["antigravity|Claude / GPT"] }, accounts: [{ provider: "antigravity", groups: [
    { source_label: "Claude / GPT", windows: [{ kind: "5h" }, { kind: "7d" }] },
  ] }] };
  assert.deepEqual(plain(ctx.windowKinds()), []);
  ctx.status.accounts = [];
  const cg = { ...ctx.history, series: ctx.history.series.filter((x) => x.source_label === "Claude / GPT") };
  ctx.historyCache[ctx.historyKey("5h", "6h")] = { data: cg };
  assert.deepEqual(plain(ctx.windowKinds()), []);
  // History of a group shown offers its kind.
  ctx.status.config = {};
  assert.deepEqual(plain(ctx.windowKinds()), ["5h"]);
  // None known: both.
  ctx.historyCache = {};
  assert.deepEqual(plain(ctx.windowKinds()), ["5h", "7d"]);
  // Without a window to offer, the buttons hide and the chosen one stays.
  const buttons = ["5h", "7d"].map((value) => ({ hidden: false, getAttribute: () => value }));
  const bar = context(["renderWindowButtons"], { windowKinds: () => [], chartWindow: "7d", $: () => ({ querySelectorAll: () => buttons }) });
  assert.equal(bar.renderWindowButtons(), false);
  assert.equal(bar.chartWindow, "7d");
  assert.deepEqual(buttons.map((b) => b.hidden), [true, true]);
});

test("the chart is drawn again when hidden_groups changes", () => {
  let drawn = 0;
  const ctx = context(["renderHiddenChange", "hiddenGroups"], {
    status: { config: {} }, history: { series: [] }, shownHidden: null,
    chartPointer: false, chartBehind: false, chartCard: { contains: () => false }, document: { activeElement: null },
    renderChart: () => drawn++,
  });
  ctx.renderHiddenChange();
  ctx.renderHiddenChange();
  assert.equal(drawn, 0);
  ctx.status = HIDDEN;
  ctx.renderHiddenChange();
  assert.equal(drawn, 1);
  ctx.renderHiddenChange();
  assert.equal(drawn, 1);
  ctx.chartPointer = true;
  ctx.status = { config: {} };
  ctx.renderHiddenChange();
  assert.equal(drawn, 1);
  assert.equal(ctx.chartBehind, true);
  assert.match(source.match(/  function renderStatus\(\) \{[^]*?\n  \}/)[0], /renderHiddenChange\(\);/);
});

// The chart drawn by drawChart into recording nodes, masked and with both
// remember switches on, over the rows of two Antigravity accounts.
function hiddenChartContext() {
  const now = 1795280000;
  const node = (tag, attrs = {}) => ({
    tag, attrs: { ...attrs }, children: [], text: attrs.text || "", style: {}, hidden: false, clientWidth: 900,
    classList: { toggle() {}, add() {}, remove() {} },
    setAttribute(name, value) { this.attrs[name] = value; }, getAttribute(name) { return this.attrs[name] ?? null; },
    appendChild(child) { this.children.push(child); return child; },
    get textContent() { return this.text + this.children.map((c) => c.textContent).join(""); },
    set textContent(value) { this.text = String(value); this.children = []; },
    addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; },
  });
  const nodes = {};
  ["chart", "chart-detail-head", "chart-detail", "legend-estimated", "chart-table", "chart-detail-wrap", "legend-projection"].forEach((id) => { nodes[id] = node("div", { id }); });
  const win = (remaining) => [{ id: "five-hour", kind: "5h", short: "5h", remaining, reset: "" }];
  const status = { accounts: [
    { auth_index: "4", provider: "antigravity", title: "Antigravity#am", email: "am@example.com", groups: [
      { key: "antigravity:4:gemini", label: "Gemini#am", source_label: "Gemini", windows: win(90) },
      { key: "antigravity:4:cg", label: "Claude / GPT#am", source_label: "Claude / GPT", windows: win(65) },
    ] },
    { auth_index: "5", provider: "antigravity", title: "Antigravity#b2", email: "b2@example.com", groups: [{ key: "antigravity:5:cg", label: "Claude / GPT#b2", source_label: "Claude / GPT", windows: win(20) }] },
  ], group_order: [], config: {} };
  const series = (s, value) => ({ ...s, provider: "antigravity", window: "five-hour", window_label: "5h", kind: "5h", points: [[now - 600, value], [now, value]], resets: [] });
  const history = { from: new Date((now - 6 * 3600) * 1000).toISOString(), to: new Date(now * 1000).toISOString(), events: [], series: [
    series({ group: "antigravity:4:gemini", label: "Gemini#am", source_label: "Gemini", auth_index: "4" }, 90),
    series({ group: "antigravity:4:cg", label: "Claude / GPT#am", source_label: "Claude / GPT", auth_index: "4" }, 65),
    series({ group: "antigravity:5:cg", label: "Claude / GPT#b2", source_label: "Claude / GPT", auth_index: "5" }, 20),
  ] };
  const state = pageStateContext({ filters: true, expanded: true }, {
    status, history, chartSelected: "", hoverListeners: [], snapPoints: [], historyLoading: "", historyFailed: "",
    historyWindow: "5h", historyRange: "6h", RESET_JUMP: 5, SENSITIVE: "account-sensitive", historyCache: {},
    gapLimit: () => 900, pooled: () => [], fitName: (name) => name,
    providerTitle: (id) => ({ antigravity: "Antigravity" })[id] || id, providerList: () => [{ id: "antigravity", title: "Antigravity" }],
    translate: (key, ...args) => [key, ...args].join(" "),
    el: (tag, attrs = {}, children = []) => {
      const n = node(tag, attrs);
      children.filter((c) => c != null).forEach((c) => n.appendChild(typeof c === "string" ? { textContent: c } : c));
      return n;
    },
    svg: (tag, attrs) => node(tag, attrs),
    document: { createTextNode: (text) => ({ textContent: text }), activeElement: null },
    clear: (n) => { n.textContent = ""; },
  });
  const { ctx } = state;
  ctx.$ = (id) => id === "settings" ? { elements: state.form } : (nodes[id] = nodes[id] || node("div", { id }));
  for (const name of ["drawChart", "renderOverview", "renderDetail", "renderChartTable", "svgText", "svgSensitive", "maskedNodes", "hasSecret", "accountSecrets",
    "accountAlias", "findRow", "chartRows", "hiddenGroups", "groupHidden", "sharedGroups", "servicePrefix", "serviceShort", "shortPrefix", "groupName",
    "groupTitle", "windowKind", "historyKey", "recentSamples", "cycleFloor", "accountWeight", "pooledRow", "parseTime"]) {
    vm.runInContext(source.match(new RegExp("  function " + name + "\\([^]*?\\n  }"))[0], ctx);
  }
  vm.runInContext(pageVar("SERVICE_SHORT"), ctx);
  const texts = (n, out = []) => { if (n.tag === "text") out.push(n); (n.children || []).forEach((c) => texts(c, out)); return out; };
  const find = (n, pred, out = []) => { if (pred(n)) out.push(n); (n.children || []).forEach((c) => find(c, pred, out)); return out; };
  return { ...state, nodes, texts, find, draw: () => runStubbed(ctx, () => ctx.drawChart()) };
}

test("hiding the detail row while masked and remembering selects the first row shown, still masked", async () => {
  const { ctx, nodes, texts, find, draw, writes } = hiddenChartContext();
  ctx.restorePageState({ mask_accounts: true });
  ctx.restoreChartState({ remember_chart_filters: true, remember_chart_expanded: true,
    chart_view: { window: "5h", sort: "default", ranges: { "5h": "6h" } }, chart_expanded: ["antigravity|Claude / GPT"] });
  ctx.chartSelected = "group:antigravity|Claude / GPT";
  draw();
  assert.equal(ctx.chartSelected, "group:antigravity|Claude / GPT");
  assert.match(nodes["chart-detail-head"].textContent, /Claude \/ GPT · quota_5h/);

  ctx.status.config = { hidden_groups: ["antigravity|Claude / GPT"] };
  draw();
  assert.equal(ctx.chartSelected, "antigravity:4:gemini|five-hour");
  assert.equal(ctx.masked, true);
  // The detail of the first row shown names it with the account suffix in
  // a masked span, and the chart's label speaks of the account by number.
  const head = nodes["chart-detail-head"];
  assert.match(head.textContent, /^Antigravity · Gemini#am · quota_5h/);
  assert.deepEqual(find(head, (n) => n.attrs && n.attrs.class === "account-sensitive").map((n) => n.textContent), ["#am"]);
  const detail = find(nodes["chart-detail"], (n) => n.tag === "svg")[0];
  assert.doesNotMatch(detail.attrs["aria-label"], /#am/);
  // The overview has no Claude / GPT row; its name is blurred.
  const names = texts(nodes.chart).filter((t) => t.attrs.class === "row-name" && t.textContent !== "ignition_lane");
  assert.deepEqual(names.map((t) => [t.textContent, t.attrs.filter]), [["Antigravity · Gemini#am", "url(#mask-blur)"]]);
  assert.doesNotMatch(nodes["chart-table"].textContent, /Claude \/ GPT/);
  // The hidden group stays expanded in the remembered state, unsaved.
  await settle();
  assert.equal(ctx.chartExpanded["group:antigravity|Claude / GPT"], true);
  assert.deepEqual(writes, []);
});

test("a remembered expanded total shown again is drawn expanded", async () => {
  const { ctx, nodes, texts, draw, writes } = hiddenChartContext();
  ctx.status.config = { hidden_groups: ["antigravity|Claude / GPT"] };
  ctx.restorePageState({ mask_accounts: true });
  ctx.restoreChartState({ remember_chart_filters: true, remember_chart_expanded: true, chart_expanded: ["antigravity|Claude / GPT"] });
  draw();
  const rowNames = () => texts(nodes.chart).filter((t) => t.attrs.class === "row-name" && t.textContent !== "ignition_lane").map((t) => t.textContent);
  assert.deepEqual(rowNames(), ["Antigravity · Gemini#am"]);
  ctx.status.config = {};
  draw();
  // The total comes back with the rows of its accounts below it.
  assert.deepEqual(rowNames(), ["Antigravity · Gemini#am", "Antigravity · Claude / GPT", "am@example.com", "b2@example.com"]);
  assert.ok(texts(nodes.chart).some((t) => t.textContent === "▾"));
  // Still masked: the account rows are blurred.
  assert.deepEqual(texts(nodes.chart).filter((t) => /@example\.com$/.test(t.textContent)).map((t) => t.attrs.filter), ["url(#mask-blur)", "url(#mask-blur)"]);
  assert.equal(ctx.chartSelected, "antigravity:4:gemini|five-hour");
  await settle();
  assert.deepEqual(writes, []);
});

// A box of "Quota groups shown" with the inputs it holds.
function shownGroupsDom(ids) {
  const boxes = {};
  const element = (tag, attrs = {}, children = []) => {
    const node = { tag, attrs: { ...attrs }, children: [], settingsDirty: false,
      getAttribute(name) { return this.attrs[name] ?? null; },
      hasAttribute(name) { return name in this.attrs; } };
    children.forEach((child) => node.children.push(child));
    return node;
  };
  ids.forEach((id) => {
    const list = { inputs: [], querySelectorAll: () => list.inputs };
    boxes[id] = { hidden: true, list, querySelector: () => list, contains: () => false };
  });
  return {
    boxes,
    globals: {
      document: { querySelector: (selector) => boxes[/data-shown-groups='([^']+)'/.exec(selector)[1]] || null, createTextNode: (text) => ({ text }) },
      el: element,
      clear: (list) => { list.inputs = []; },
    },
  };
}

function shownGroupsContext(accounts) {
  const dom = shownGroupsDom(["antigravity", "claude", "codex"]);
  const ctx = context(["shownGroupChoices", "renderShownGroups", "renderAllShownGroups"], {
    ...dom.globals, status: { accounts }, shownEdits: {},
    providerList: () => [{ id: "antigravity", groups: ["Gemini", "Claude / GPT"] }, { id: "claude" }, { id: "codex" }],
  });
  // Labels keep their inputs in the list for the next render.
  const append = (box) => (label) => box.list.inputs.push(label.children[0]);
  Object.values(dom.boxes).forEach((box) => { box.list.appendChild = append(box); });
  return { ctx, boxes: dom.boxes };
}

test("Services lists the quota groups of each service to show, checked unless hidden", () => {
  const { ctx, boxes } = shownGroupsContext([
    { provider: "claude", groups: [{ source_label: "Claude" }, { source_label: "Fable" }] },
    { provider: "codex", groups: [{ source_label: "GPT" }] },
  ]);
  const shown = (id) => boxes[id].list.inputs.map((input) => [input.getAttribute("data-shown-group"), input.checked]);
  ctx.renderAllShownGroups({ hidden_groups: ["antigravity|Claude / GPT"] }, null);
  // The provider's groups, before any account reports them.
  assert.deepEqual(shown("antigravity"), [["antigravity|Gemini", true], ["antigravity|Claude / GPT", false]]);
  assert.deepEqual(shown("claude"), [["claude|Claude", true], ["claude|Fable", true]]);
  assert.deepEqual([boxes.antigravity.hidden, boxes.claude.hidden, boxes.codex.hidden], [false, false, true]);
  // The service with the focus is left alone.
  boxes.antigravity.contains = (node) => node === "focused";
  ctx.renderAllShownGroups({}, "focused");
  assert.deepEqual(shown("antigravity"), [["antigravity|Gemini", true], ["antigravity|Claude / GPT", false]]);
  assert.deepEqual(shown("claude"), [["claude|Claude", true], ["claude|Fable", true]]);
  // A single group shows no checkboxes unless it is hidden.
  ctx.renderAllShownGroups({ hidden_groups: ["codex|GPT", "codex|Gone"] }, null);
  assert.deepEqual(shown("codex"), [["codex|GPT", false], ["codex|Gone", false]]);
  assert.equal(boxes.codex.hidden, false);
  // A checkbox with an edit not yet applied shows the edit.
  const fable = boxes.claude.list.inputs[1];
  ctx.shownEdits["claude|Fable"] = { shown: false };
  ctx.renderAllShownGroups({}, null);
  assert.equal(boxes.claude.list.inputs[1], fable);
  assert.equal(fable.checked, false);
  assert.deepEqual(shown("antigravity"), [["antigravity|Gemini", true], ["antigravity|Claude / GPT", true]]);
});

test("the shown groups save as hidden_groups, keeping hidden groups the page does not list", async () => {
  const box = (id, checked) => ({ type: "checkbox", name: "", checked, value: "on", attrs: { "data-shown-group": id },
    getAttribute(name) { return this.attrs[name] ?? null; }, hasAttribute(name) { return name in this.attrs; },
    closest: () => null, checkValidity: () => true });
  const inputs = [box("antigravity|Gemini", true), box("antigravity|Claude / GPT", false), box("claude|Fable", false)];
  const writes = [], refilled = [];
  const ctx = context(["saveField", "captureIntent", "buildPatch", "refillSaved", "settleShown", "noteEdit", "inputTop", "topKey", "setPath", "saveControls", "rereadConfig", "hiddenList"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(),
    rawConfig: { hidden_groups: ["codex|Gone", "claude|Fable"] }, status: { config: {} }, shownEdits: {},
    saveHost: () => ({}), claimState: () => () => true,
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, showApplied: async () => true,
    fillSettings: (keys) => refilled.push(plain(keys)), renderIgnitionOff: () => {},
  });
  vm.runInContext(pageVar("PUBLIC_URL_TRIM"), ctx);
  assert.equal(ctx.inputTop(inputs[1]), "hidden_groups");
  inputs[1].settingsDirty = true;
  ctx.saveField({ target: inputs[1] });
  await ctx.saveQueue;
  await settle();
  assert.deepEqual(writes.at(-1), { hidden_groups: ["codex|Gone", "claude|Fable", "antigravity|Claude / GPT"] });
  assert.deepEqual(refilled.at(-1), { hidden_groups: true });
  assert.equal(inputs[1].settingsDirty, false);
  // Applied, the edit is done with; the checkboxes follow the config again.
  assert.deepEqual(plain(ctx.shownEdits), {});
  // Showing every group deletes the key.
  ctx.rawConfig.hidden_groups = ["antigravity|Claude / GPT"];
  inputs[1].checked = true;
  ctx.saveField({ target: inputs[1] });
  await ctx.saveQueue;
  assert.deepEqual(writes.at(-1), { hidden_groups: null });
});

function shownSaveContext(inputs, rawConfig) {
  const writes = [];
  const ctx = context(["saveField", "captureIntent", "buildPatch", "refillSaved", "settleShown", "noteEdit", "inputTop", "topKey", "setPath", "saveControls", "rereadConfig", "hiddenList"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(),
    rawConfig, status: { config: {} }, shownEdits: {},
    saveHost: () => ({}), claimState: () => () => true,
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, showApplied: async () => true,
    fillSettings: () => {}, renderIgnitionOff: () => {},
  });
  vm.runInContext(pageVar("PUBLIC_URL_TRIM"), ctx);
  return { ctx, writes };
}
const shownBox = (id, checked) => ({ type: "checkbox", name: "", checked, value: "on", attrs: { "data-shown-group": id },
  getAttribute(name) { return this.attrs[name] ?? null; }, hasAttribute(name) { return name in this.attrs; },
  closest: () => null, checkValidity: () => true });

test("a hidden group written by hand with spaces or twice is shown again by its checkbox", async () => {
  // The checkbox shows the applied list, where " claude|Fable" is "claude|Fable".
  const inputs = [shownBox("claude|Claude", true), shownBox("claude|Fable", true)];
  const { ctx, writes } = shownSaveContext(inputs, { hidden_groups: [" claude|Fable", "claude|Fable\u3000", "", 7, "antigravity|Gemini", "antigravity|Gemini"] });
  ctx.saveField({ target: inputs[1] });
  await ctx.saveQueue;
  assert.deepEqual(writes.at(-1), { hidden_groups: ["antigravity|Gemini"] });
  assert.deepEqual(plain(ctx.hiddenList([" a|b ", "a|b", "", "  ", null, "c|d"])), ["a|b", "c|d"]);
});

test("a queued save of hidden_groups keeps a checkbox that left the form meanwhile", async () => {
  const inputs = [shownBox("claude|Claude", true), shownBox("claude|Fable", false)];
  const { ctx, writes } = shownSaveContext(inputs, {});
  let release;
  ctx.saveQueue = new Promise((resolve) => { release = resolve; });
  inputs[1].settingsDirty = true;
  ctx.saveField({ target: inputs[1] });
  // While the save waits, the only account reporting Fable goes and the
  // refill drops its checkbox.
  inputs.splice(1, 1);
  release();
  await ctx.saveQueue;
  assert.deepEqual(writes, [{ hidden_groups: ["claude|Fable"] }]);
});

test("an edit waiting to be saved survives its checkbox going and coming back, and the next edit keeps it", async () => {
  const dom = shownGroupsDom(["claude"]);
  const list = dom.boxes.claude.list;
  list.appendChild = (label) => list.inputs.push(label.children[0]);
  const both = [{ provider: "claude", groups: [{ source_label: "Claude" }, { source_label: "Fable" }] }];
  const writes = [];
  const ctx = context(["shownGroupChoices", "renderShownGroups", "renderAllShownGroups", "saveField", "captureIntent", "buildPatch", "refillSaved", "settleShown",
    "noteEdit", "inputTop", "topKey", "setPath", "saveControls", "rereadConfig", "hiddenList"], {
    ...dom.globals, status: { accounts: both, config: {} }, shownEdits: {},
    providerList: () => [{ id: "claude" }],
    $: () => ({ elements: list.inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(), rawConfig: {},
    saveHost: () => ({}), claimState: () => () => true,
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, showApplied: async () => true, fillSettings: () => {}, renderIgnitionOff: () => {},
  });
  vm.runInContext(pageVar("PUBLIC_URL_TRIM"), ctx);
  const input = (id) => list.inputs.find((i) => i.getAttribute("data-shown-group") === id);
  ctx.renderAllShownGroups({}, null);
  let release;
  ctx.saveQueue = new Promise((resolve) => { release = resolve; });
  const fable = input("claude|Fable");
  fable.checked = false;
  ctx.saveField({ target: fable });
  // While the save waits, the config still shows Fable: its account stops
  // reporting it and reports it again, and the boxes are made anew.
  ctx.status.accounts = [{ provider: "claude", groups: [{ source_label: "Claude" }] }];
  ctx.renderAllShownGroups({}, null);
  list.inputs = [];
  ctx.status.accounts = both;
  ctx.renderAllShownGroups({}, null);
  assert.notEqual(input("claude|Fable"), fable);
  assert.equal(input("claude|Fable").checked, false);
  // An edit of another group saves Fable's edit with it.
  const claude = input("claude|Claude");
  claude.checked = false;
  ctx.saveField({ target: claude });
  release();
  await ctx.saveQueue;
  await settle();
  assert.deepEqual(writes, [{ hidden_groups: ["claude|Fable"] }, { hidden_groups: ["claude|Fable", "claude|Claude"] }]);
  assert.deepEqual(plain(ctx.shownEdits), {});
});

test("a failed edit of a shown group that a later save applies leaves the heading", async () => {
  const heading = saveNode("H2");
  const section = { tagName: "SECTION", querySelector: () => heading };
  const box = (id) => {
    const input = saveNode("INPUT", { "data-shown-group": id });
    Object.assign(input, { type: "checkbox", checked: true, closest: (selector) => selector === ".settings-section" ? section : null });
    return input;
  };
  const inputs = [box("claude|Claude"), box("claude|Fable")];
  const writes = [];
  const ctx = stateContext(["saveField", "captureIntent", "buildPatch", "refillSaved", "settleShown", "noteEdit", "inputTop", "topKey", "setPath", "rereadConfig", "hiddenList"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(), rawConfig: {}, status: { config: {} }, shownEdits: {},
    saveItemLabel: (input) => input.getAttribute("data-shown-group"),
    patchConfig: async (patch) => {
      writes.push(plain(patch));
      if (writes.length === 1) throw new Error("offline");
      Object.assign(ctx.rawConfig, patch);
      return "token";
    },
    showApplied: async (say) => { say("saved", "applied"); return true; },
    loadConfig: async () => {}, fillSettings: () => {}, renderIgnitionOff: () => {},
  });
  vm.runInContext(pageVar("PUBLIC_URL_TRIM"), ctx);
  const state = () => heading.children[0].getAttribute("data-state");
  const fable = inputs[1];
  fable.checked = false;
  ctx.saveField({ target: fable });
  await ctx.saveQueue;
  assert.equal(state(), "failed");
  assert.match(heading.textContent, /claude\|Fable save failed: offline/);
  // Fable's checkbox leaves the form; hiding Claude saves Fable's draft too.
  inputs.splice(1, 1);
  inputs[0].checked = false;
  ctx.saveField({ target: inputs[0] });
  await ctx.saveQueue;
  await settle();
  assert.deepEqual(writes.at(-1), { hidden_groups: ["claude|Fable", "claude|Claude"] });
  assert.equal(state(), "saved");
  assert.doesNotMatch(heading.textContent, /failed/);
  assert.equal(fable.hasAttribute("aria-invalid"), false);
});

test("the shown groups have their text in both languages", () => {
  const ctx = vm.createContext({});
  vm.runInContext(pageVar("I18N"), ctx);
  for (const lang of ["zh", "en"]) {
    assert.ok(vm.runInContext("I18N." + lang + ".shown_groups", ctx), lang);
    assert.match(vm.runInContext("I18N." + lang + ".monitor_hint", ctx), lang === "zh" ? /额度组/ : /quota groups/, lang);
  }
});
