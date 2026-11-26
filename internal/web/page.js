(function () {
  "use strict";

  // Relative paths, so that the page works at the root of its own listener
  // and behind the reverse proxy of a Magpie page service.
  var API = "api";
  var CONFIG_API = "api/config";
  var KEY_STORE = "lamplighter-admin-key";

  // In Magpie's sandboxed frame the page has an opaque origin, so its own
  // api/… requests are cross-origin (Origin: null). The internal listener
  // writes a token of its process into the page's lamplighter-page meta
  // tag and answers such requests with CORS headers only when they carry
  // it in X-Lamplighter-Page; another site cannot read the page to learn
  // it. The page takes no requests from its parent, so a site embedding it
  // cannot use the token through it.
  function pageToken(doc) {
    var meta = doc.querySelector('meta[name="lamplighter-page"]');
    return (meta && meta.getAttribute("content")) || "";
  }
  var PAGE_TOKEN = pageToken(document);

  var key = "";
  var status = null;
  var settingsFilled = false;
  var rawConfig = {};
  var chartWindow = "5h";
  var range = "6h";
  var chartSort = "default";
  var chartExpanded = {};
  var chartSelected = null;
  var history = null;
  // The window kind of the series in history. The chart keeps drawing it
  // until the history of a newly chosen window arrives.
  var historyWindow = "";
  var refreshTimer = null;

  function $(id) { return document.getElementById(id); }

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (name) {
        var value = attrs[name];
        if (value === undefined || value === null || value === false) return;
        if (name === "text") node.textContent = value;
        else if (name === "class") node.className = value;
        else if (name === "style") node.setAttribute("style", value);
        else if (name.indexOf("on") === 0) node.addEventListener(name.slice(2), value);
        else node.setAttribute(name, value === true ? "" : value);
      });
    }
    (children || []).forEach(function (child) {
      if (child === null || child === undefined) return;
      node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
    });
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  // ---- Magpie ----

  // In Magpie the page runs in the frame of a plugin page, sandboxed to
  // allow-scripts: an opaque origin without localStorage, clipboard,
  // dialogs or popups. Magpie's page service adds the same sandbox when
  // its address is opened in a tab of its own. SANDBOXED tells the page to
  // work around the missing parts.
  var SANDBOXED = String(window.origin) === "null";

  // The frame first loads the plugin's page file, which sends it to this
  // page. Magpie answers the messages of the frame, whatever document it
  // holds (pluginpages.js matches ev.source), so the page speaks the
  // protocol of Magpie's page bridge (internal/gui/pagebridge.js) itself:
  // "hello" answers with { dark, lang, tokens, visible }, a "theme" event
  // follows every change of theme or language, and a "visibility" event
  // tells when the reader leaves the page and comes back. ready is set once
  // Magpie has answered. The page never sends the "error" method, which
  // replaces the page with Magpie's error view.
  var magpie = { ready: false, theme: null, visible: true, calls: {}, next: 0 };

  // Magpie's window and magpie web always frame the page in a sandbox, so
  // only a sandboxed page in a frame speaks the protocol. Opened through
  // the public listener the page is not sandboxed; another site that
  // frames it is no Magpie and gets neither answers nor calls.
  function inFrame() {
    if (!SANDBOXED) return false;
    try { return window.parent !== window; } catch (e) { return false; }
  }

  function magpieCall(method, params) {
    if (!inFrame()) return Promise.reject(new Error("not in Magpie"));
    var id = ++magpie.next;
    return new Promise(function (resolve, reject) {
      magpie.calls[id] = { resolve: resolve, reject: reject };
      window.parent.postMessage({ magpie: 1, id: id, method: method, params: params }, "*");
    });
  }

  function magpieMessage(e) {
    if (!inFrame() || e.source !== window.parent) return;
    var m = e.data;
    if (!m || typeof m !== "object" || m.magpie !== 1) return;
    if (m.event === "theme") { magpieTheme(m.data); return; }
    if (m.event === "visibility") { magpieVisible(!(m.data && m.data.visible === false)); return; }
    var call = magpie.calls[m.id];
    if (!call) return;
    delete magpie.calls[m.id];
    if (m.error) call.reject(new Error(String(m.error)));
    else call.resolve(m.result);
  }

  function magpieTheme(theme) {
    magpie.theme = theme && typeof theme === "object" ? { dark: theme.dark, lang: theme.lang, tokens: themeTokens(theme.tokens) } : null;
    applyTheme();
    applyLanguage();
    if (status && history) renderChart();
  }

  // Coming back to the page refreshes it at once; while away it skips the
  // periodic refresh.
  function magpieVisible(visible) {
    var back = visible && !magpie.visible;
    magpie.visible = visible;
    if (!back || keyAsked()) return;
    if (!contentReady) loadPage();
    else loadStatus().then(refreshHistory).catch(function () {});
  }

  function magpieHello() {
    if (!inFrame()) return;
    window.addEventListener("message", magpieMessage);
    magpieCall("hello").then(function (answer) {
      magpie.ready = true;
      if (answer && answer.visible === false) magpie.visible = false;
      magpieTheme(answer);
      // Screenshots go through Magpie now that it answers.
      renderCardsSnapshot();
      if (status) renderStatus();
    }).catch(function () { /* not Magpie: the browser's settings stay */ });
  }

  // The theme follows Magpie, whose theme is light or dark, and the
  // browser when the page is opened on its own.
  function pageTheme(magpieTheme, browserDark) {
    if (magpieTheme && typeof magpieTheme.dark === "boolean") return magpieTheme.dark ? "dark" : "light";
    return browserDark ? "dark" : "light";
  }

  // The colors and fonts of Magpie's theme (pluginpages.js sends them as
  // tokens), which page.css takes as --magpie-<token> variables. Only these
  // keys are taken, and only values that are what the key names
  // (tokenValue); any other value counts as missing. A token missing from a
  // later theme is removed, so page.css falls back to its own value.
  var MAGPIE_TOKENS = ["bg", "card", "fg", "fg-2", "muted", "faint", "line", "accent", "accent-soft", "accent-fg", "green", "red", "amber", "font", "code"];
  var FONT_TOKENS = { font: true, code: true };

  // value as the token name takes it, or "". A value can never load
  // anything: colors must be a color to CSS.supports and hold no function
  // that refers to something else (url(), image-set(), var(), env(),
  // attr()); fonts must be a list of font families, without parentheses,
  // braces, semicolons, backslashes or angle brackets.
  function tokenValue(name, value) {
    if (typeof value !== "string") return "";
    value = value.trim();
    if (!value || value.length > 300) return "";
    if (FONT_TOKENS[name]) return /[(){};\\<>@]/.test(value) ? "" : value;
    if (/url|image|var\(|env\(|attr\(|[;{}\\<>@]/i.test(value)) return "";
    return typeof CSS !== "undefined" && CSS.supports && CSS.supports("color", value) ? value : "";
  }

  function themeTokens(tokens) {
    var out = {};
    if (!tokens || typeof tokens !== "object") return out;
    MAGPIE_TOKENS.forEach(function (name) {
      var value = tokenValue(name, tokens[name]);
      if (value) out[name] = value;
    });
    return out;
  }

  function applyTokens(style, tokens) {
    MAGPIE_TOKENS.forEach(function (name) {
      if (tokens && tokens[name]) style.setProperty("--magpie-" + name, tokens[name]);
      else style.removeProperty("--magpie-" + name);
    });
  }

  function applyTheme() {
    var dark = !!(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
    var theme = pageTheme(magpie.theme, dark);
    if (theme === "light") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", theme);
    applyTokens(document.documentElement.style, magpie.theme && magpie.theme.tokens);
    colorCache = {};
  }

  // ---- Language ----

  // Every visible text, per language. Keys are used by data-i18n attributes
  // in page.html and by translate() below; {0}, {1} are arguments. A new language
  // adds a dictionary here and a column in internal/notify/text.go.
  var I18N = {
    zh: {
      app_sub: "点灯人", repo_hint: "在 GitHub 上查看",
      update_hint: "有新版本 v{0}，可在 Magpie 的插件页面更新",
      link_release: "。复制下面的地址，可在浏览器中查看发布说明：",
      link_appeal: "复制下面的地址，在浏览器中打开以提出申诉：",
      key_title: "需要管理密钥",
      key_hint: "通过对外地址访问时需要 Lamplighter 的管理密钥（设置中的 admin_key），密钥保存在当前浏览器中。",
      key_placeholder: "Lamplighter 管理密钥",
      key_submit: "确定",
      key_invalid: "管理密钥无效，请重新输入。",
      key_format: "管理密钥只能包含英文字母、数字和符号。",
      refresh_all: "立即刷新全部",
      quota: "额度",
      ignition: "自动点火",
      col_group: "额度组", col_next: "下次点火", col_reset: "当前重置", col_last: "最近成功", col_model: "模型", col_status: "状态",
      chart_title: "额度变化", chart_window: "额度窗口", chart_range: "时间范围", chart_sort: "排序",
      quota_5h: "5 小时额度", quota_7d: "7 天额度",
      sort_default: "默认排序", sort_low: "最低优先",
      no_data: "无数据",
      legend_ignited: "● 点火成功，×n 为同一时刻的多个目标",
      legend_failed: "✕ 含点火失败",
      legend_reset: "↻ 重置",
      pace_reference: "匀速参考",
      pace_extended: "匀速参考（往回延长）",
      legend_pace_extended: "匀速参考往回延长到本周期起点（周期中途预定重置变了）",
      legend_pooled: "多账号的服务显示合计额度，点击行查看详情，再点一次展开各账号",
      legend_estimated: "合计中有账号缺数据，按它前后的读数估算",
      data_table: "数据表",
      col_quota: "额度", col_account: "账号", col_latest: "最新", col_min: "最低", col_max: "最高", col_samples: "样本", col_to_reset: "距重置",
      events: "事件",
      settings: "设置",
      bark_url: "Bark 推送地址", bark_group: "Bark 分组",
      webhook_url: "Webhook 地址", webhook_method: "请求方法", webhook_headers: "请求头（每行一个）", webhook_body: "请求体",
      webhook_body_placeholder: "留空时发送内置的 JSON 消息",
      webhook_success: "成功条件（可选）", webhook_success_hint: "JSON 响应必须包含这些字段和值才算发送成功；留空时只检查 HTTP 状态码",
      webhook_hint: "地址、请求头和请求体中可以使用占位符：{{title}} {{body}} {{text}} {{url}} {{priority}} {{kind}} {{label}} {{time}}。各服务的写法见 README。",
      headers_invalid: "每行写一个请求头，格式为 名称: 值", json_invalid: "请填写 JSON 对象，例如 {\"code\":0}",
      test_notify: "测试推送", test_notify_hint: "发送到所有已配置的渠道；刚修改的设置生效后才发送",
      test_waiting: "等待插件应用新设置…", test_not_applied: "配置尚未生效，未发送测试",
      test_ok: "{0}：已发送", test_failed: "{0}：失败，{1}", test_response: "响应：{0}",
      test_unchecked: "只检查了 HTTP 状态码。飞书、钉钉、企业微信等服务失败时也返回 200，请到客户端确认是否收到，或设置成功条件。",
      test_some_failed: "部分渠道测试失败", test_all_failed: "测试推送失败",
      thresholds: "提醒阈值", remaining_le: "剩余 ≤",
      threshold_notice: "提醒", threshold_low: "偏低", threshold_critical: "告急",
      thresholds_hint: "额度降到每一档时推送一次，耗尽时再推送一次。三档须从高到低，可以相等；前两档也决定额度条和图表的颜色。",
      thresholds_order: "阈值须满足：提醒 ≥ 偏低 ≥ 告急，未保存",
      window_5h: "5 小时窗口", window_7d: "7 天窗口",
      recovery_notify: "额度恢复时通知", reset_reminder: "重置前提醒",
      recovery_notify_5h: "额度恢复时通知 · 5 小时窗口", recovery_notify_7d: "额度恢复时通知 · 7 天窗口",
      reset_reminder_5h: "重置前提醒 · 5 小时窗口", reset_reminder_7d: "重置前提醒 · 7 天窗口",
      reminder_hours: "提前（小时）",
      reminder_hours_5h: "重置前提醒 · 提前（小时） · 5 小时窗口", reminder_hours_7d: "重置前提醒 · 提前（小时） · 7 天窗口",
      hours_empty: "至少填一个小时数；不想提醒请选“关闭”", hours_too_many: "最多填 3 个小时数",
      hours_invalid_5h: "填 0.5～4.5 之间的小时数，用逗号分隔", hours_invalid_7d: "填 1～167 之间的小时数，用逗号分隔",
      mode_off: "关闭", recovery_all: "每次", recovery_after_exhausted: "用完后",
      reminder_all: "每次", reminder_has_remaining: "有余量",
      mode_off_title: "关闭", recovery_all_title: "每次重置都通知", recovery_after_exhausted_title: "窗口用完后，在下一次重置时通知一次",
      reminder_all_title: "每次重置前都提醒", reminder_has_remaining_title: "剩余高于告急阈值时才提醒",
      notify_modes_hint: "“用完后”：窗口用完后，在下一次重置时通知一次。“有余量”：剩余高于告急阈值时才提醒。重置前提醒按“提前（小时）”中填的时间发送，可填 1～3 个，用逗号分隔；每个时间每个周期提醒一次。恢复通知附带上一周期的剩余额度，取自每次重置前 30 秒的查询。",
      quota_queries: "额度查询",
      poll_interval: "查询间隔（秒）", request_timeout: "请求超时（秒）", history_retention: "历史保留（天）",
      passive_skip: "被动数据跳过窗口（秒）", passive_skip_max: "最多连续跳过（分钟）",
      passive_skip_hint: "整点前这段时间内已有被动数据的账号跳过本轮主动查询；设为 0 则每轮都主动查询。",
      ignition_enabled: "启用自动点火",
      ignition_enable_hint: "开启前先设置时区，并在 Magpie 中关闭预热，以免重复点火。",
      timezone: "时区", timezone_name: "IANA 时区名称", timezone_save: "保存",
      timezone_unsaved: "时区尚未保存，确认输入框中的时区后点“保存”。",
      timezone_hint: "点火时间、通知和页面中的时间按这个时区计算，不依赖服务器的时区（Magpie 的 Docker 镜像为 UTC）。尚未保存时，输入框中预填浏览器的时区。",
      start_hour: "每日开始（时）", end_hour: "每日结束（时）", end_grace: "结束后宽限（分钟）",
      grace: "重置后延迟（秒）", failure_retry: "失败重试间隔（秒）", max_transient: "临时错误上限（次）",
      services: "服务", col_service: "服务", col_ignition_model: "点火模型",
      monitor_service: "监控 {0}", ignite_service: "点火 {0}", model_auto: "自动：{0}",
      ignite_groups: "{0} 点火额度组", no_ignition: "不支持点火", list_or: " 或 ",
      shown_groups: "显示的额度组",
      codex_reset_enabled: "转发 Did Codex Reset 发布的 Codex 重置信号",
      codex_reset_pending: "首次开启时推送当前待生效的排期",
      chart_prefs: "额度图表", remember_chart_filters: "记住额度窗口、时间范围和排序",
      remember_chart_filters_hint: "“额度变化”上次选择的额度窗口、时间范围和排序存在服务器上，各设备共用；关闭后每次打开页面都使用默认选择。",
      remember_chart_expanded: "记住展开的分组",
      remember_chart_expanded_hint: "“额度变化”中展开的多账号分组存在服务器上，各设备共用，5 小时和 7 天额度共用一套；关闭后每次打开页面分组都是收起的。",
      state_save_failed: "未能保存这项选择：{0}",
      save_hint: "修改后自动保存到 Lamplighter 的 config.json 并立即应用。文本和数字在离开输入框或按回车时保存，保存结果显示在区块标题旁，高级设置按分组显示。",
      language_note: "通知语言跟随最近一次打开此页面时的页面语言，当前为{0}。",
      lang_zh: "中文", lang_en: "English",
      due: "已到", minutes: "{0} 分钟", hours_minutes: "{0} 小时 {1} 分", days: "{0} 天",
      soon: "即将", in_time: "{0}后", just_now: "刚刚", ago: "{0}前",
      reset_now: "已重置",
      state_starting: "启动中", state_running: "运行中",
      last_poll: "上次查询 {0}（{1}）", next_poll: "下次 {0}（{1}）",
      config_error: "配置无法解析，仍在使用上一份配置：{0}",
      models_error: "模型列表不可用：{0}",
      notify_off: "未设置 Bark 推送地址或 Webhook 地址，通知已关闭。",
      notify_error: "Webhook 设置无效，未启用：{0}",
      remaining_aria: "{0} {1} 剩余",
      source_passive: "被动", source_active: "主动", updated: "更新于 {0}",
      no_accounts: "没有可监控的 {0} 凭证。",
      first_query: "启动约 5 秒后开始第一次查询。",
      refresh: "刷新", refreshed: "已刷新 {0}",
      account_settings: "账号", rename: "名称",
      rename_hint: "名称保存在 Lamplighter 的设置中，不修改 Magpie 的账号名称。留空则显示 Magpie 中的账号名称或按邮箱区分账号。",
      account_order_hint: "拖动手柄调整顺序，额度卡片和图表默认顺序随之调整。拖动时按 Esc 可取消。",
      account_name_aria: "{0} 的名称", account_weight_aria: "{0} 的权重", weight_default: "套餐默认值 {0}",
      weight: "权重", weight_hint: "同一服务有多个账号时，可设置各账号的权重，合计按权重平均剩余额度。权重须大于 0，可带小数；留空使用输入框中提示的套餐默认值。",
      plan: "套餐：{0}", weight_invalid: "权重必须是大于 0 的数字",
      blocked_5h: "5 小时额度已用完", blocked_7d: "7 天额度已用完", blocked_other: "{0}额度已用完",
      blocked_until: "{0} 重置后恢复点火", blocked_wait: "额度恢复后恢复点火",
      paused_until: "暂停至 {0}", failures: "失败 {0} 次", rolling: "窗口未开始", normal: "正常",
      ignition_note: "每天 {0}:00 开始，之后在每次重置后 {1} 秒点火，最晚到 {2}。",
      no_targets: "没有启用点火的额度组。",
      ignite_now: "立即点火",
      ignite_confirm: "立即向 {0} 发送一次点火请求？这会开始一个新的 5 小时窗口。",
      ignited: "{0} 点火成功",
      ev_ignite: "点火成功", ev_ignite_manual: "手动点火", ev_ignite_failed: "点火失败", ev_ignite_paused: "点火暂停", ev_model_refused: "模型被拒",
      ev_plan_change: "套餐变化", ev_notify: "已推送", ev_notify_failed: "推送失败", ev_codex_reset: "重置信号", ev_error: "错误",
      ev_ignited: "{0} · 下次重置 {1}",
      ev_retry: "{0}后重试：{1}", ev_retry_cooldown: "冷却结束，{0}后重试：{1}",
      ev_seconds: "{0} 秒", ev_minutes: "{0} 分钟",
      ev_paused: "暂停到 {0}：{1}",
      ev_paused_auth: "暂停，等待更新凭证：{0}", ev_paused_suspended: "暂停，账号已停用：{0}",
      ev_refused: "{0} 被上游拒绝，改用 {1}，{2} 小时内不再尝试",
      ev_notify_failed_detail: "{0}：{1}",
      ev_codex_sent: "已发送 {0} 条通知",
      ev_reset: "额度重置", ev_exhausted: "额度耗尽", ev_credential_invalid: "凭证失效", ev_credential_suspended: "账号停用",
      ev_credential_rate_limited: "被限流", ev_credential_restored: "凭证恢复", ev_notify_quota: "额度提醒", ev_notify_reminder: "重置提醒",
      ev_window_left: "{0} · 剩余 {1}", ev_window_next: "{0} · 剩余 {1} · 下次重置 {2}", ev_window_due: "{0} · 剩余 {1} · {2} 重置",
      ev_limited_until: "{0} · 限流到 {1}", ev_ignite_paused_note: "{0} · 点火已暂停", ev_ignite_paused_until: "{0} · 点火暂停到 {1}", ev_appeal: "申诉", ev_source_ignite: "点火时确认", ev_source_query: "额度查询时确认",
      ev_show_5h: "显示 5 小时重置", ev_show_5h_count: "显示 5 小时重置（{0}）",
      no_events: "暂无事件", collapse: "收起", show_more: "显示更多（共 {0} 条）",
      failed: "失败：{0}",
      range_h: "{0} 小时", range_d: "{0} 天", range_mo: "{0} 个月",
      overview_aria: "各额度剩余百分比总览",
      ignition_lane: "点火", lane_ok: "{0} 成功", lane_failed: " · {0} 失败",
      total: "合计", total_suffix: " 合计", estimated_suffix: "（估算）", more_accounts: "　… 另 {0} 个账号",
      pooled_note: "{0} 个账号合计，阴影为账号间的最低到最高",
      stat_total: "合计", stat_current: "当前", stat_pace: "匀速", stat_diff: "差值", stat_to_reset: "距重置",
      projected_5h: "按当前速度", projected_7d: "按本周期日均",
      legend_projection_5h: "按当前速度的延长线", legend_projection_7d: "按本周期日均的延长线", legend_hatch: "无数据或尚未到来",
      detail_aria: "{0} 剩余百分比",
      detail_keys: "按左右方向键查看各时刻的数值",
      passive_sample: "（被动）", active_sample: "（主动）",
      quota_reset: "额度重置",
      loading: "加载中…", no_history: "这个范围内还没有额度数据。",
      history_failed: "读取历史失败：{0}",
      tz_config: "配置中指定",
      tz_note: "通知和页面中的时间使用 {0}，{1}。",
      tz_note_unset: "尚未设置时区：点火和通知使用服务器的时区 {0}，页面中的时间按浏览器的时区显示。",
      zone_notice: "尚未设置时区。页面中的时间按浏览器的时区显示，点火时段和通知中的时间按服务器的时区（{0}）计算。",
      zone_notice_go: "设置时区",
      save_failed: "保存失败：{0}",
      load_failed: "加载失败：{0}", retry: "重试",
      bad_response: "服务返回的不是 Lamplighter 的数据", request_timed_out: "请求超时",
      mcp_public_url_invalid: "对外地址须为 http 或 https 地址，不能含空格、? 或 #",
      refreshed_all: "已刷新全部额度", test_sent: "测试通知已发送",
      level_full: "满", level_high: "高", level_medium: "中", level_low: "低", level_empty: "空",
      inactive: "已停用", inactive_suffix: "（已停用）", stat_last: "最后读数",
      legend_inactive: "已停用：账号不再报告这个窗口，保留到最后一个数据点移出所选范围",
      drag_handle: "拖动调整顺序",
      order_failed: "账号顺序保存失败：{0}",
      view_nav: "页面", advanced: "高级",
      notify_channels: "通知渠道", webhook_advanced: "高级：请求方法、请求头、请求体和成功条件", alert_rules: "提醒规则",
      ignition_off: "自动点火已关闭，这些设置暂不生效。",
      monitor_hint: "勾选要监控额度的服务。取消勾选的额度组不在额度页显示，也不发送额度通知。",
      save_saved: "已保存", save_saving: "保存中…",
      save_field_failed: "✕ {0}保存失败", save_failure_reason: "：{0}", save_failed_compact: "✕ 保存失败", save_more_failed: "（另有 {0} 项失败）", save_order: "排序",
      save_waiting: "已保存，等待生效", save_applied: "已生效", save_unconfirmed: "未确认生效",
     
      mask_on: "打码账号", mask_off: "显示账号", account_n: "账号 {0}",
      weekdays: "周日,周一,周二,周三,周四,周五,周六",
      snapshot_card: "复制这张卡片的截图", snapshot_cards: "复制所有卡片的截图", snapshot_detail: "复制曲线详情图的截图",
      snapshot_copied: "已复制截图", snapshot_failed: "截图复制失败：{0}", snapshot_unsupported: "当前浏览器不支持复制图片",
      mcp_hint: "AI 客户端（如 Claude Code）可以通过 MCP 查询额度、历史和事件，并刷新额度。",
      mcp_enabled: "启用 MCP 接口", public_listen: "对外监听地址", mcp_key: "密钥", mcp_key_unset: "未设置",
      mcp_show: "显示", mcp_hide: "隐藏", copy: "复制", copied: "已复制", copy_failed: "无法复制，请手动选择文本复制",
      mcp_generate: "生成新密钥", mcp_regenerate_confirm: "生成新密钥？旧密钥会立即失效，使用它的客户端需要换成新密钥。",
      mcp_status: "状态", mcp_unknown: "未知", mcp_running: "运行中", mcp_stopped: "未启动",
      mcp_public_url: "对外地址（可选）",
      mcp_public_hint: "对外开放时请使用反向代理并配置 HTTPS，然后在这里填写对外地址，下面的配置随之更新。",
      mcp_snippets: "客户端配置", mcp_claude: "Claude Code", mcp_json: "通用 JSON", mcp_stdio: "只支持 stdio 的客户端（mcp-remote）",
      mcp_no_key: "先生成密钥，配置中的 <key> 会换成实际的密钥。", mcp_error: "MCP 接口未启动：{0}",
      copy_failed_sandbox: "Magpie 中的页面不能写入剪贴板。请选中文本手动复制，或通过对外地址在浏览器中打开页面后复制",
      confirm_again: "{0} 再点一次确认。",
      public_access: "对外访问",
      public_hint: "内部监听只供 Magpie 使用，不需要密钥。对外监听让其他设备通过浏览器或 MCP 访问 Lamplighter：页面和接口需要管理密钥，MCP 需要 MCP 的密钥。两者都关闭时不监听。",
      web_enabled: "在对外监听上提供页面和接口", admin_key: "管理密钥",
      admin_regenerate_confirm: "生成新的管理密钥？旧密钥会立即失效，通过对外地址打开的页面需要输入新密钥。",
      public_status: "状态", public_off: "未启用", public_web: "页面和接口", public_mcp: "MCP",
      public_error: "对外监听未启动：{0}",
      warmup_notice: "Magpie 为这些账号开启了预热：{0}。预热和 Lamplighter 的自动点火会重复点火，请在 Magpie 中关闭这些账号的预热。"
    },
    en: {
      app_sub: "", repo_hint: "View on GitHub",
      update_hint: "v{0} is available; update it on the Plugins page of Magpie",
      link_release: ". Copy this address to read the release notes in a browser:",
      link_appeal: "Copy this address and open it in a browser to appeal:",
      key_title: "Admin key required",
      key_hint: "Access through the public address needs the Lamplighter admin key (admin_key in the settings). The key is kept in this browser.",
      key_placeholder: "Lamplighter admin key",
      key_submit: "OK",
      key_invalid: "The admin key is not valid. Enter it again.",
      key_format: "The admin key can only contain English letters, digits and symbols.",
      refresh_all: "Refresh all",
      quota: "Quota",
      ignition: "Ignition",
      col_group: "Quota group", col_next: "Next ignition", col_reset: "Current reset", col_last: "Last success", col_model: "Model", col_status: "Status",
      chart_title: "Quota history", chart_window: "Quota window", chart_range: "Time range", chart_sort: "Order",
      quota_5h: "5-hour quota", quota_7d: "7-day quota",
      sort_default: "Default order", sort_low: "Lowest first",
      no_data: "No data",
      legend_ignited: "● ignition succeeded; ×n marks several targets at the same time",
      legend_failed: "✕ an ignition failed",
      legend_reset: "↻ reset",
      pace_reference: "Even pace",
      pace_extended: "Even pace (extended back)",
      legend_pace_extended: "Even pace extended back to the cycle start (the reset moved within the cycle)",
      legend_pooled: "Services with several accounts show their total; click a row for details, click again to list the accounts",
      legend_estimated: "An account of the total is missing data; estimated from its readings before and after",
      data_table: "Data table",
      col_quota: "Quota", col_account: "Account", col_latest: "Latest", col_min: "Lowest", col_max: "Highest", col_samples: "Samples", col_to_reset: "Until reset",
      events: "Events",
      settings: "Settings",
      bark_url: "Bark push URL", bark_group: "Bark group",
      webhook_url: "Webhook URL", webhook_method: "Method", webhook_headers: "Headers (one per line)", webhook_body: "Body",
      webhook_body_placeholder: "Empty sends the built-in JSON message",
      webhook_success: "Success condition (optional)", webhook_success_hint: "The JSON response must contain these fields and values for the delivery to count; empty checks only the HTTP status",
      webhook_hint: "The URL, headers and body can use placeholders: {{title}} {{body}} {{text}} {{url}} {{priority}} {{kind}} {{label}} {{time}}. See the README for each service.",
      headers_invalid: "One header per line, as Name: value", json_invalid: "Enter a JSON object, such as {\"code\":0}",
      test_notify: "Send test", test_notify_hint: "Sends to every configured channel once the settings just changed are in effect",
      test_waiting: "Waiting for the plugin to apply the new settings…", test_not_applied: "The new settings are not in effect yet, so the test was not sent",
      test_ok: "{0}: sent", test_failed: "{0}: failed, {1}", test_response: "Response: {0}",
      test_unchecked: "Only the HTTP status was checked. Feishu, DingTalk, WeCom and similar services answer 200 on failure too, so check that the message arrived or set a success condition.",
      test_some_failed: "Some channels failed the test", test_all_failed: "The test notification failed",
      thresholds: "Alert thresholds", remaining_le: "Remaining ≤",
      threshold_notice: "Notice", threshold_low: "Low", threshold_critical: "Critical",
      thresholds_hint: "One notification when quota drops to each threshold, and one more when it runs out. The thresholds must not increase from first to third; the first two also set the colors of the bars and the chart.",
      thresholds_order: "Thresholds must satisfy Notice ≥ Low ≥ Critical; not saved",
      window_5h: "5-hour window", window_7d: "7-day window",
      recovery_notify: "Notify when quota recovers", reset_reminder: "Remind before resets",
      recovery_notify_5h: "Notify when quota recovers · 5-hour window", recovery_notify_7d: "Notify when quota recovers · 7-day window",
      reset_reminder_5h: "Remind before resets · 5-hour window", reset_reminder_7d: "Remind before resets · 7-day window",
      reminder_hours: "Hours before",
      reminder_hours_5h: "Remind before resets · Hours before · 5-hour window", reminder_hours_7d: "Remind before resets · Hours before · 7-day window",
      hours_empty: "Enter at least one number of hours. To stop reminders, choose Off", hours_too_many: "Enter at most 3 numbers of hours",
      hours_invalid_5h: "Enter hours from 0.5 to 4.5, separated by commas", hours_invalid_7d: "Enter hours from 1 to 167, separated by commas",
      mode_off: "Off", recovery_all: "Always", recovery_after_exhausted: "After out",
      reminder_all: "Always", reminder_has_remaining: "If left",
      mode_off_title: "Off", recovery_all_title: "Notify at every reset", recovery_after_exhausted_title: "After the window runs out, notify once at its next reset",
      reminder_all_title: "Remind before every reset", reminder_has_remaining_title: "Remind only when more than the Critical threshold is left",
      notify_modes_hint: "\"After out\": after the window runs out, notify once at its next reset. \"If left\": remind only when more than the Critical threshold is left. Reminders are sent at the times in \"Hours before\"; enter 1 to 3, separated by commas, and each is sent once per cycle. Recovery notifications include what was left of the ended cycle, from the query 30 seconds before every reset.",
      quota_queries: "Quota queries",
      poll_interval: "Query interval (s)", request_timeout: "Request timeout (s)", history_retention: "History retention (days)",
      passive_skip: "Passive data window (s)", passive_skip_max: "Longest skip (min)",
      passive_skip_hint: "Accounts with passive data within this time before a slot skip that active query; 0 queries every time.",
      ignition_enabled: "Enable ignition",
      ignition_enable_hint: "Before turning it on, set the time zone and turn off warm-up in Magpie, so that windows are not ignited twice.",
      timezone: "Time zone", timezone_name: "IANA time zone name", timezone_save: "Save",
      timezone_unsaved: "The time zone is not saved yet. Check the time zone in the field and click Save.",
      timezone_hint: "Ignition, notifications and this page use this time zone, whatever the server's time zone is (UTC in Magpie's Docker image). Until one is saved, the field shows this browser's time zone.",
      start_hour: "Daily start (hour)", end_hour: "Daily end (hour)", end_grace: "Grace after end (min)",
      grace: "Delay after reset (s)", failure_retry: "Retry after failure (s)", max_transient: "Transient error limit",
      services: "Services", col_service: "Service", col_ignition_model: "Ignition model",
      monitor_service: "Monitor {0}", ignite_service: "Ignite {0}", model_auto: "Auto: {0}",
      ignite_groups: "{0} ignition groups", no_ignition: "No ignition", list_or: " or ",
      shown_groups: "Quota groups shown",
      codex_reset_enabled: "Forward Codex reset signals published by Did Codex Reset",
      codex_reset_pending: "When first turned on, send the pending schedule",
      chart_prefs: "Quota chart", remember_chart_filters: "Remember the window, range and order",
      remember_chart_filters_hint: "The quota window, time range and order last chosen in the quota history are kept on the server and shared by every device. When off, the page opens with the default choices.",
      remember_chart_expanded: "Remember expanded groups",
      remember_chart_expanded_hint: "The multi-account groups expanded in the quota history are kept on the server and shared by every device, one set for the 5-hour and 7-day quotas. When off, the page opens with the groups collapsed.",
      state_save_failed: "This choice was not saved: {0}",
      save_hint: "Changes save to Lamplighter's config.json automatically and apply at once. Text and numbers save when you leave the field or press Enter. Save results appear beside section headings or advanced group legends.",
      language_note: "Notifications use the language of this page from the last time it was opened, now {0}.",
      lang_zh: "中文", lang_en: "English",
      due: "due", minutes: "{0} min", hours_minutes: "{0} h {1} min", days: "{0} days",
      soon: "soon", in_time: "in {0}", just_now: "just now", ago: "{0} ago",
      reset_now: "reset",
      state_starting: "Starting", state_running: "Running",
      last_poll: "last query {0} ({1})", next_poll: "next {0} ({1})",
      config_error: "The config cannot be parsed; the previous config stays in use: {0}",
      models_error: "Model list unavailable: {0}",
      notify_off: "No Bark push URL or webhook URL is set, so notifications are off.",
      notify_error: "The webhook settings are invalid, so the webhook is off: {0}",
      remaining_aria: "{0} {1} remaining",
      source_passive: "passive", source_active: "active", updated: "updated {0}",
      no_accounts: "No {0} credentials to monitor.",
      first_query: "The first query runs about 5 seconds after the start.",
      refresh: "Refresh", refreshed: "Refreshed {0}",
      account_settings: "Accounts", rename: "Name",
      rename_hint: "Names are saved in the Lamplighter settings and do not change the account names in Magpie. Leave the name empty to show the Magpie account name or distinguish accounts by email.",
      account_order_hint: "Drag a handle to set the default order of quota cards and charts. Press Esc during a drag to cancel.",
      account_name_aria: "Name of {0}", account_weight_aria: "Weight of {0}", weight_default: "Plan default: {0}",
      weight: "Weight", weight_hint: "For services with several accounts, the total averages their remaining quota by weight. Weights must be above 0; decimals are allowed. Leave a field empty to use the plan default shown in it.",
      plan: "Plan: {0}", weight_invalid: "The weight must be a number above 0",
      blocked_5h: "5-hour quota used up", blocked_7d: "7-day quota used up", blocked_other: "{0} quota used up",
      blocked_until: "Ignition resumes after the reset at {0}", blocked_wait: "Ignition resumes when the quota recovers",
      paused_until: "Paused until {0}", failures: "{0} failures", rolling: "Window not started", normal: "Normal",
      ignition_note: "Starts at {0}:00 every day, then ignites {1} seconds after each reset, until {2}.",
      no_targets: "No quota group has ignition enabled.",
      ignite_now: "Ignite now",
      ignite_confirm: "Send an ignition request to {0} now? This starts a new 5-hour window.",
      ignited: "{0} ignited",
      ev_ignite: "Ignited", ev_ignite_manual: "Manual ignition", ev_ignite_failed: "Ignition failed", ev_ignite_paused: "Ignition paused", ev_model_refused: "Model refused",
      ev_plan_change: "Plan changed", ev_notify: "Notified", ev_notify_failed: "Notification failed", ev_codex_reset: "Reset signal", ev_error: "Error",
      ev_ignited: "{0} · next reset {1}",
      ev_retry: "Retrying in {0}: {1}", ev_retry_cooldown: "Retrying when the cooldown ends, in {0}: {1}",
      ev_seconds: "{0} s", ev_minutes: "{0} min",
      ev_paused: "Paused until {0}: {1}",
      ev_paused_auth: "Paused until the credential is updated: {0}", ev_paused_suspended: "Paused, account suspended: {0}",
      ev_refused: "{0} was refused upstream; used {1}, not tried again for {2} hours",
      ev_notify_failed_detail: "{0}: {1}",
      ev_codex_sent: "Sent {0} notifications",
      ev_reset: "Quota reset", ev_exhausted: "Quota exhausted", ev_credential_invalid: "Credential invalid", ev_credential_suspended: "Account suspended",
      ev_credential_rate_limited: "Rate limited", ev_credential_restored: "Credential restored", ev_notify_quota: "Quota alert", ev_notify_reminder: "Reset reminder",
      ev_window_left: "{0} · {1} left", ev_window_next: "{0} · {1} left · next reset {2}", ev_window_due: "{0} · {1} left · resets {2}",
      ev_limited_until: "{0} · limited until {1}", ev_ignite_paused_note: "{0} · ignition paused", ev_ignite_paused_until: "{0} · ignition paused until {1}", ev_appeal: "Appeal", ev_source_ignite: "Confirmed by ignition", ev_source_query: "Confirmed by a quota query",
      ev_show_5h: "Show 5-hour resets", ev_show_5h_count: "Show 5-hour resets ({0})",
      no_events: "No events yet", collapse: "Show less", show_more: "Show more ({0} in total)",
      failed: "Failed: {0}",
      range_h: "{0}h", range_d: "{0}d", range_mo: "{0} month",
      overview_aria: "Remaining quota overview",
      ignition_lane: "Ignition", lane_ok: "{0} succeeded", lane_failed: " · {0} failed",
      total: "Total", total_suffix: " total", estimated_suffix: " (estimated)", more_accounts: "　… {0} more accounts",
      pooled_note: "Total of {0} accounts; the shade spans the lowest to the highest account",
      stat_total: "Total", stat_current: "Now", stat_pace: "Even pace", stat_diff: "Difference", stat_to_reset: "Until reset",
      projected_5h: "At the current rate", projected_7d: "At this cycle's daily average",
      legend_projection_5h: "Projection at the current rate", legend_projection_7d: "Projection at this cycle's daily average", legend_hatch: "No data, or not yet",
      detail_aria: "{0} remaining percent",
      detail_keys: "Use the left and right arrow keys to step through the values",
      passive_sample: " (passive)", active_sample: " (active)",
      quota_reset: "quota reset",
      loading: "Loading…", no_history: "No quota data in this range yet.",
      history_failed: "Could not read the history: {0}",
      tz_config: "set in the config",
      tz_note: "Notifications and this page show times in {0}, {1}.",
      tz_note_unset: "No time zone is set: ignition and notifications use the server's time zone, {0}, and this page shows times in the browser's time zone.",
      zone_notice: "No time zone is set. This page shows times in the browser's time zone; the ignition hours and the times in notifications use the server's ({0}).",
      zone_notice_go: "Set time zone",
      save_failed: "Could not save: {0}",
      load_failed: "Could not load: {0}", retry: "Retry",
      bad_response: "The server did not answer with Lamplighter data", request_timed_out: "The request timed out",
      mcp_public_url_invalid: "The public address must be an http or https address without spaces, ? or #",
      refreshed_all: "Refreshed all quota", test_sent: "Test notification sent",
      level_full: "Full", level_high: "High", level_medium: "Medium", level_low: "Low", level_empty: "Empty",
      inactive: "Inactive", inactive_suffix: " (inactive)", stat_last: "Last reading",
      legend_inactive: "Inactive: the account no longer reports this window; it shows until its last sample leaves the range",
      drag_handle: "Drag to reorder",
      order_failed: "Could not save the account order: {0}",
      view_nav: "Page", advanced: "Advanced",
      notify_channels: "Notification channels", webhook_advanced: "Advanced: method, headers, body and success condition", alert_rules: "Alert rules",
      ignition_off: "Ignition is off, so these settings do not apply for now.",
      monitor_hint: "Check the services whose quota to monitor. Unchecked quota groups do not show on the quota page and send no quota notifications.",
      save_saved: "Saved", save_saving: "Saving…",
      save_field_failed: "✕ {0} save failed", save_failure_reason: ": {0}", save_failed_compact: "✕ Save failed", save_more_failed: " ({0} more failed)", save_order: "Order",
      save_waiting: "Saved; waiting to take effect", save_applied: "In effect", save_unconfirmed: "Not confirmed in effect",
     
      mask_on: "Blur accounts", mask_off: "Show accounts", account_n: "Account {0}",
      weekdays: "Sun,Mon,Tue,Wed,Thu,Fri,Sat",
      snapshot_card: "Copy a screenshot of this card", snapshot_cards: "Copy a screenshot of all cards", snapshot_detail: "Copy a screenshot of the detail chart",
      snapshot_copied: "Screenshot copied", snapshot_failed: "Could not copy the screenshot: {0}", snapshot_unsupported: "This browser cannot copy images",
      mcp_hint: "AI clients such as Claude Code can read quota, history and events and refresh quota over MCP.",
      mcp_enabled: "Enable the MCP endpoint", public_listen: "Public listen address", mcp_key: "Key", mcp_key_unset: "Not set",
      mcp_show: "Show", mcp_hide: "Hide", copy: "Copy", copied: "Copied", copy_failed: "Could not copy; select the text and copy it",
      mcp_generate: "Generate new key", mcp_regenerate_confirm: "Generate a new key? The old key stops working at once, and clients using it need the new one.",
      mcp_status: "Status", mcp_unknown: "Unknown", mcp_running: "Running", mcp_stopped: "Not running",
      mcp_public_url: "Public address (optional)",
      mcp_public_hint: "To open it to other machines, put a reverse proxy with HTTPS in front and enter its address here; the configs below follow it.",
      mcp_snippets: "Client configuration", mcp_claude: "Claude Code", mcp_json: "Generic JSON", mcp_stdio: "stdio-only clients (mcp-remote)",
      mcp_no_key: "Generate a key first; it replaces <key> in the configs.", mcp_error: "The MCP endpoint is not running: {0}",
      copy_failed_sandbox: "Pages in Magpie cannot write to the clipboard. Select the text and copy it, or open the page through the public address in a browser",
      confirm_again: "{0} Click again to confirm.",
      public_access: "Public access",
      public_hint: "The internal listener serves Magpie only and needs no key. The public listener lets other devices reach Lamplighter in a browser or over MCP: the page and API need the admin key, MCP needs its own key. With both off, nothing listens.",
      web_enabled: "Serve the page and API on the public listener", admin_key: "Admin key",
      admin_regenerate_confirm: "Generate a new admin key? The old key stops working at once, and pages opened through the public address need the new one.",
      public_status: "Status", public_off: "Off", public_web: "Page and API", public_mcp: "MCP",
      public_error: "The public listener is not running: {0}",
      warmup_notice: "Magpie warms up these accounts: {0}. Its warm-up and Lamplighter's ignition would both start the window; turn warm-up off for these accounts in Magpie."
    }
  };

  var lang = "zh";

  function translate(key) {
    var text = (I18N[lang] && I18N[lang][key] !== undefined) ? I18N[lang][key] : (I18N.zh[key] !== undefined ? I18N.zh[key] : key);
    for (var i = 1; i < arguments.length; i++) text = text.split("{" + (i - 1) + "}").join(String(arguments[i]));
    return text;
  }

  // The language follows Magpie's ("zh", "zh-TW", "en", …), and the
  // browser's when the page is opened on its own. Chinese variants show
  // Chinese and every other language English.
  function pageLanguage(magpieTheme, browser) {
    var value = (magpieTheme && typeof magpieTheme.lang === "string" && magpieTheme.lang) || browser || "";
    return /^zh/i.test(value) ? "zh" : "en";
  }

  function browserLanguage() {
    return (navigator.languages && navigator.languages[0]) || navigator.language || "";
  }

  function applyStaticText() {
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
    [["data-i18n", null], ["data-i18n-placeholder", "placeholder"], ["data-i18n-title", "title"], ["data-i18n-aria-label", "aria-label"]].forEach(function (pair) {
      Array.prototype.forEach.call(document.querySelectorAll("[" + pair[0] + "]"), function (node) {
        var text = translate(node.getAttribute(pair[0]));
        if (pair[1]) node.setAttribute(pair[1], text);
        else node.textContent = text;
      });
    });
  }

  var languageApplied = false;
  function applyLanguage() {
    var next = pageLanguage(magpie.theme, browserLanguage());
    if (languageApplied && next === lang) return;
    lang = next;
    languageApplied = true;
    applyStaticText();
    renderSaveStates();
    renderRangeButtons();
    renderMaskButton();
    renderCardsSnapshot();
    if (loadState && loadState.state && !$("load-section").hidden) renderLoadCard();
    if (status) {
      renderStatus();
      $("timezone-note").textContent = timezoneText();
      reportLanguage();
    }
    if (status && history) renderChart();
  }

  // Notifications are sent without a page open, so the page tells the
  // plugin which language to use; it keeps the last one reported.
  var reportingLanguage = false;
  function reportLanguage() {
    if (!status || status.language === lang || reportingLanguage) return;
    reportingLanguage = true;
    api("POST", API + "/language", { language: lang }).then(function (data) {
      if (data.status) { status = data.status; renderLanguageNote(); }
    }).catch(function () { /* retried on the next status refresh */ }).then(function () { reportingLanguage = false; });
  }

  function renderLanguageNote() {
    $("language-note").textContent = translate("language_note", translate("lang_" + (status.language || "zh")));
  }

  // timeout, in milliseconds, aborts a request that takes longer; so does
  // signal, an AbortSignal, when it aborts.
  function api(method, path, body, timeout, signal) {
    var opts = { method: method, headers: { "Accept": "application/json" } };
    var sentKey = key;
    if (key) opts.headers.Authorization = "Bearer " + key;
    if (PAGE_TOKEN) opts.headers["X-Lamplighter-Page"] = PAGE_TOKEN;
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    var timer = null, expired = false;
    if ((timeout || signal) && window.AbortController) {
      var abort = new AbortController();
      opts.signal = abort.signal;
      if (timeout) timer = setTimeout(function () { expired = true; abort.abort(); }, timeout);
      if (signal && signal.aborted) abort.abort();
      else if (signal) signal.addEventListener("abort", function () { abort.abort(); });
    }
    var done = function () { clearTimeout(timer); };
    var timedOut = function (err) {
      throw expired ? new Error(translate("request_timed_out")) : err;
    };
    return fetch(path, opts).then(function (res) {
      return res.json().catch(function (err) {
        // A body cut off by the timeout is no answer. A success whose body
        // is not JSON, such as the login page of a proxy, is not one either.
        if (opts.signal && opts.signal.aborted) throw err;
        if (res.ok) throw new Error(translate("bad_response"));
        return {};
      }).then(function (data) {
        if (!data || typeof data !== "object") data = res.ok ? null : {};
        if (!data) throw new Error(translate("bad_response"));
        if (res.status === 401) {
          // A request sent before this page saved a new admin key does not
          // make the new key invalid.
          if (sentKey === key) askForKey(sentKey ? translate("key_invalid") : "");
          throw new Error("unauthorized");
        }
        if (!res.ok) throw new Error(data.error || data.message || ("HTTP " + res.status));
        return data;
      });
    }).catch(timedOut).then(function (data) { done(); return data; }, function (err) { done(); throw err; });
  }

  // A key a request header can carry: printable ASCII, spaces inside
  // included (the ends are trimmed). The browser refuses to send others,
  // such as one pasted with Chinese text, without a 401.
  function validKey(value) { return /^[\x20-\x7e]+$/.test(value); }

  // After the page saves a new admin key, a page that sends a key sends the
  // new one.
  function useKey(secret) {
    if (!key) return;
    key = secret;
    try { localStorage.setItem(KEY_STORE, secret); } catch (err) {}
  }

  // A page that has taken anything from a key (pagePrepared) never takes
  // another one: it loads anew, which drops the requests of the old key
  // still under way with all they could change, such as the masking, the
  // page state and the history. The stored key stays, so the reloaded page
  // asks with the reason; a key that was not stored is asked for without
  // it. A page that has taken nothing shows the key form. Either way the
  // load under way is cancelled, so it does not show the page meanwhile.
  function askForKey(message) {
    cancelLoad();
    hideContent();
    if (pagePrepared) {
      location.reload();
      return;
    }
    key = "";
    try { localStorage.removeItem(KEY_STORE); } catch (err) {}
    setLoadState("");
    $("key-section").hidden = false;
    $("key-error").hidden = !message;
    $("key-error").textContent = message || "";
  }

  // Magpie's sandbox has no dialogs: confirm() returns false there without
  // asking. Sandboxed, the action asks in a toast and runs on a second click
  // of the same button within 5 seconds.
  function confirmed(button, message) {
    if (!SANDBOXED) return confirm(message);
    if (button.confirmUntil && Date.now() < button.confirmUntil) {
      button.confirmUntil = 0;
      return true;
    }
    button.confirmUntil = Date.now() + 5000;
    toast(translate("confirm_again", message));
    return false;
  }

  var toastTimer = null;
  function toast(message) {
    var node = $("toast");
    clear(node);
    maskedNodes(message).forEach(function (child) { node.appendChild(child); });
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { node.hidden = true; }, 5000);
  }

  // The status names a time zone only when the config sets one; without
  // one it says "Local", the time zone of the plugin's process, which is
  // UTC in Magpie's Docker image.
  function zoneSet(st) {
    return !!(st && st.timezone && st.timezone !== "Local");
  }

  // Times are shown in the configured time zone, like notifications. Without
  // one they are shown in the browser's, at the offset of time t (now when
  // t is not given), and a notice says so.
  function offsetSeconds(t) {
    var value = zoneSet(status) ? Number(status.utc_offset_seconds) : NaN;
    return isFinite(value) ? value : -(t || new Date()).getTimezoneOffset() * 60;
  }

  function parseTime(value) {
    if (!value) return null;
    var t = new Date(value);
    if (isNaN(t.getTime()) || t.getUTCFullYear() < 2000) return null;
    return t;
  }

  function pad(n) { return (n < 10 ? "0" : "") + n; }

  function shifted(t) { return new Date(t.getTime() + offsetSeconds(t) * 1000); }

  function fmtDateTime(t) {
    if (!t) return "—";
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate()) + " " + pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  // A time ahead with its weekday, for resets and the next ignition.
  function fmtDateTimeDay(t) {
    if (!t) return "—";
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate()) + " " + translate("weekdays").split(",")[s.getUTCDay()] + " " +
      pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  function fmtClock(t) {
    var s = shifted(t);
    return pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  function fmtDay(t) {
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate());
  }

  function fmtDuration(ms) {
    if (ms <= 0) return translate("due");
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return translate("minutes", minutes);
    var hours = Math.floor(minutes / 60);
    if (hours < 48) return translate("hours_minutes", hours, minutes % 60);
    return translate("days", Math.round(hours / 24));
  }

  function fmtIn(t) {
    if (!t) return "";
    var ms = t.getTime() - Date.now();
    if (ms < 60000) return translate("soon");
    return translate("in_time", fmtDuration(ms));
  }

  function fmtAgo(t) {
    if (!t) return "";
    var ms = Date.now() - t.getTime();
    if (ms < 60000) return translate("just_now");
    return translate("ago", fmtDuration(ms));
  }

  // Bars and the chart color quota in five levels: empty at 0% and full at
  // 100%, judged on the raw value, so 99.6% is not full; between them low,
  // medium and high, split by the first two notification thresholds, the
  // same split as the 🟡 and 🔴 notifications. Cards, chart bands, swatches,
  // tooltips and the legend all use quotaLevel.
  function quotaThresholds() {
    var cfg = status && status.config ? status.config : {};
    var notice = Number(cfg.notice_threshold), low = Number(cfg.low_threshold);
    return { notice: isFinite(notice) ? notice : 50, low: isFinite(low) ? low : 20 };
  }

  // A weighted average of full accounts can land a rounding error below 100.
  var LEVEL_EPSILON = 1e-6;

  function quotaLevel(remaining) {
    var t = quotaThresholds();
    if (remaining <= LEVEL_EPSILON) return "empty";
    if (remaining >= 100 - LEVEL_EPSILON) return "full";
    if (remaining <= t.low) return "low";
    if (remaining <= t.notice) return "medium";
    return "high";
  }

  function fmtCountdown(ms) {
    if (ms <= 0) return translate("reset_now");
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return minutes + "m";
    var hours = Math.floor(minutes / 60);
    if (hours < 24) return hours + "h " + pad(minutes % 60) + "m";
    return Math.floor(hours / 24) + "d " + (hours % 24) + "h";
  }

  function windowName(w) {
    if (w.short === "5h") return translate("window_5h");
    if (w.short === "7d") return translate("window_7d");
    return w.label;
  }

  // The providers come from the status API in display order.
  function providerList() { return (status && status.providers) || []; }

  function providerTitle(provider) {
    var hit = providerList().filter(function (p) { return p.id === provider; })[0];
    return hit ? hit.title : provider;
  }

  // "A、B 或 C" in Chinese, "A, B or C" in English.
  function listText(items) {
    if (items.length < 2) return items.join("");
    return items.slice(0, -1).join(lang === "zh" ? "、" : ", ") + translate("list_or") + items[items.length - 1];
  }

  function localized(text) { return text ? (lang === "zh" ? text.zh : text.en) : ""; }

  function badge(kind, text) {
    return el("span", { class: "badge badge-" + kind }, [el("span", { class: "badge-dot" }), document.createTextNode(text)]);
  }

  // ---- Account masking ----

  // The eye button in the header blurs every text that names an account,
  // for screenshots to share. The choice is kept in mask_accounts of the
  // config (see writePageState). The app stays hidden and nothing that names
  // an account is drawn until a valid config has set the masking
  // (contentReady, see loadPage), so a masked page never shows the names
  // first, and an unmasked one never flashes blurred.
  // Text that names accounts carries SENSITIVE, which .mask-accounts
  // blurs; chart text in SVG gets the mask-blur filter instead. Masked,
  // title tooltips that name accounts are left out and labels for screen
  // readers number the accounts. Confirmations keep the names.
  var SENSITIVE = "account-sensitive";
  var masked = false;

  function accountAlias(index) { return translate("account_n", index + 1); }

  // The names that tell accounts apart: titles, emails, credential file
  // names and group labels of the current accounts, and the labels of
  // events and history series, which keep the names of renamed and deleted
  // accounts; without the provider and group names they share. A name made
  // of a shared name and the suffix of an account, such as "Claude#rk" or
  // "Claude / GPT#main", counts only by its suffix ("#rk"), so masking
  // blurs the suffix and leaves the service readable, as in "Claude#rk" of
  // a Claude account with several quota groups in the chart's table and
  // the detail heading. Longest first, so an email wins over a part of it.
  function accountSecrets() {
    var common = {}, found = {};
    providerList().forEach(function (p) { common[p.title] = true; common[p.id] = true; });
    var accounts = (status && status.accounts) || [];
    accounts.forEach(function (a) { (a.groups || []).forEach(function (g) { common[g.source_label] = true; }); });
    var add = function (name, whole) {
      name = typeof name === "string" ? name.trim() : "";
      var at = name.indexOf("#");
      if (!whole && at > 0 && common[name.slice(0, at)]) name = name.slice(at);
      if (name.length >= 2 && !common[name]) found[name] = true;
    };
    accounts.forEach(function (a) {
      [a.title, a.email, a.name].forEach(function (name) { add(name); });
      (a.groups || []).forEach(function (g) { add(g.label); });
    });
    var past = [], orphan = [];
    ((status && status.events) || []).concat((history && history.events) || []).forEach(function (ev) { past.push(ev.label); });
    // A series of a quota group no current account has lost its metadata:
    // the history names it by the internal group key, which identifies the
    // account, so its names count whatever they look like.
    var groups = {};
    accounts.forEach(function (a) { (a.groups || []).forEach(function (g) { groups[g.key] = true; }); });
    ((history && history.series) || []).forEach(function (s) {
      if (groups[s.group]) {
        if (s.label !== s.source_label) past.push(s.label);
      } else {
        orphan.push(s.label, s.source_label, s.group);
      }
    });
    past.forEach(function (name) { add(name); });
    orphan.forEach(function (name) { add(name, true); });
    return Object.keys(found).sort(function (a, b) { return b.length - a.length; });
  }

  function hasSecret(text) {
    text = String(text == null ? "" : text);
    return accountSecrets().some(function (name) { return text.indexOf(name) >= 0; });
  }

  // The text as nodes, with each account name in a SENSITIVE span. extra
  // lists more text to treat as account names.
  function maskedNodes(text, extra) {
    var rest = String(text == null ? "" : text), secrets = accountSecrets(), out = [];
    if (extra) {
      secrets = secrets.concat(extra.filter(function (name) { return typeof name === "string" && name.trim().length; }))
        .sort(function (a, b) { return b.length - a.length; });
    }
    while (rest) {
      var at = -1, hit = "";
      secrets.forEach(function (name) {
        var i = rest.indexOf(name);
        if (i >= 0 && (at < 0 || i < at)) { at = i; hit = name; }
      });
      if (at < 0) { out.push(document.createTextNode(rest)); break; }
      if (at) out.push(document.createTextNode(rest.slice(0, at)));
      out.push(el("span", { class: SENSITIVE, text: hit }));
      rest = rest.slice(at + hit.length);
    }
    return out;
  }

  // An SVG text that names an account, blurred while masked.
  function svgSensitive(node, always) {
    if (masked && (always || hasSecret(node.textContent))) node.setAttribute("filter", "url(#mask-blur)");
    return node;
  }

  // The header button and the eye buttons beside the screenshot buttons all
  // switch the same masking, so a screenshot needs no trip to the top.
  function drawMaskButton(button) {
    var label = translate(masked ? "mask_off" : "mask_on");
    button.setAttribute("aria-pressed", masked ? "true" : "false");
    button.setAttribute("title", label);
    button.setAttribute("aria-label", label);
    clear(button);
    var icon = svg("svg", { viewBox: "0 0 16 16", width: 16, height: 16, "aria-hidden": "true" });
    icon.appendChild(svg("path", { d: "M1.5 8s2.4-4.5 6.5-4.5S14.5 8 14.5 8s-2.4 4.5-6.5 4.5S1.5 8 1.5 8z" }));
    icon.appendChild(svg("circle", { cx: 8, cy: 8, r: 2.1 }));
    if (masked) icon.appendChild(svg("path", { d: "M2.5 2.5l11 11" }));
    button.appendChild(icon);
    return button;
  }

  function renderMaskButton() {
    Array.prototype.forEach.call(document.querySelectorAll(".mask-toggle"), drawMaskButton);
  }

  // An eye button for beside a screenshot button; screenshots leave it out.
  function maskButton() {
    return drawMaskButton(el("button", { type: "button", class: "btn btn-ghost btn-icon mask-toggle no-snapshot", onclick: toggleMask }));
  }

  function setMasked(on) {
    masked = on;
    document.documentElement.classList.toggle("mask-accounts", masked);
    renderMaskButton();
  }

  function toggleMask() {
    var detailFocus = document.activeElement === $("chart-detail-head").querySelector(".mask-toggle");
    setMasked(!masked);
    writePageState("mask_accounts", { mask_accounts: masked || null });
    if (!status) return;
    renderStatus();
    if (history) renderChart();
    if (detailFocus) {
      var eye = $("chart-detail-head").querySelector(".mask-toggle");
      if (eye) eye.focus({ preventScroll: true });
    }
  }

  // ---- Screenshots ----

  // The camera buttons copy a PNG of an account card, of all cards or of
  // the detail chart to the clipboard, as the page shows it, masking
  // included. The nodes are cloned without the .no-snapshot buttons and
  // drawn with the page's style sheet through an SVG foreignObject on a
  // canvas, at the screen's pixel ratio. Animations, transitions and hover
  // styles are left out. Nothing outside the page is loaded, so the canvas
  // stays readable. In Magpie the sandbox has no clipboard, and Magpie
  // copies the PNG for the page (the bridge's image.copy).
  function canCopyImage() {
    if (magpie.ready) return true;
    return !SANDBOXED && !!(window.ClipboardItem && navigator.clipboard && navigator.clipboard.write && window.isSecureContext);
  }

  // A PNG blob of the nodes side by side in a frame width pixels wide. The
  // frame's style (.snapshot-frame in page.css) is part of the page's style
  // sheet, so the frame is measured off screen with the same style the
  // image is drawn with.
  function snapshotPng(nodes, width) {
    var body = getComputedStyle(document.body);
    var frame = el("div", { class: "snapshot-frame" });
    frame.style.width = width + "px";
    frame.style.fontFamily = body.fontFamily;
    frame.style.fontSize = body.fontSize;
    frame.style.lineHeight = body.lineHeight;
    nodes.forEach(function (node) {
      var copy = node.cloneNode(true);
      Array.prototype.forEach.call(copy.querySelectorAll(".no-snapshot, .tooltip, [visibility='visible'][pointer-events='none']"), function (n) {
        n.parentNode.removeChild(n);
      });
      frame.appendChild(copy);
    });
    // Measured off screen, with the page's layout.
    var host = el("div", { class: "snapshot-host", "aria-hidden": "true" }, [frame]);
    document.body.appendChild(host);
    var w = Math.ceil(frame.getBoundingClientRect().width), h = Math.ceil(frame.getBoundingClientRect().height);
    document.body.removeChild(host);
    var scale = Math.max(1, Math.min(3, window.devicePixelRatio || 1));
    var css = Array.prototype.map.call(document.querySelectorAll("style"), function (s) { return s.textContent; }).join("\n");
    var root = document.documentElement, theme = root.getAttribute("data-theme");
    // The root's style holds Magpie's tokens (applyTokens); the image's root
    // takes them too, so it has the page's colors.
    var rootStyle = root.getAttribute("style") || "";
    var markup = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"" + w * scale + "\" height=\"" + h * scale + "\" viewBox=\"0 0 " + w + " " + h + "\"" +
      (theme ? " data-theme=\"" + theme + "\"" : "") + (rootStyle ? " style=\"" + xmlAttr(rootStyle) + "\"" : "") +
      " class=\"" + root.className + "\" lang=\"" + root.lang + "\">" +
      "<style><![CDATA[" + css.split("]]>").join("]]]]><![CDATA[>") + "]]></style>" +
      "<foreignObject x=\"0\" y=\"0\" width=\"" + w + "\" height=\"" + h + "\">" + new XMLSerializer().serializeToString(frame) + "</foreignObject></svg>";
    return new Promise(function (resolve, reject) {
      var img = new Image();
      img.onload = function () {
        var canvas = document.createElement("canvas");
        canvas.width = w * scale;
        canvas.height = h * scale;
        try {
          canvas.getContext("2d").drawImage(img, 0, 0, canvas.width, canvas.height);
          canvas.toBlob(function (blob) { if (blob) resolve(blob); else reject(new Error("canvas")); }, "image/png");
        } catch (err) { reject(err); }
      };
      img.onerror = function () { reject(new Error("image")); };
      img.src = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(markup);
    });
  }

  function xmlAttr(text) {
    return String(text).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");
  }

  // Writes the PNG to the clipboard within the click, which the browser
  // requires, while the image is still being drawn.
  function copySnapshot(nodes) {
    var width = nodes.reduce(function (max, n) { return Math.max(max, n.getBoundingClientRect().width); }, 0);
    var png;
    try { png = snapshotPng(nodes, Math.ceil(width) + 24); } catch (err) { png = Promise.reject(err); }
    var write;
    if (magpie.ready) write = png.then(function (blob) { return magpieCall("image.copy", { png: blob }); });
    else try { write = navigator.clipboard.write([new ClipboardItem({ "image/png": png })]); } catch (err) { write = Promise.reject(err); }
    png.catch(function () {});
    write.then(function () { toast(translate("snapshot_copied")); }, function (err) {
      toast(translate("snapshot_failed", (err && (err.message || err.name)) || String(err)));
    });
  }

  // The button for all cards, beside "Refresh all".
  function renderCardsSnapshot() {
    var old = $("snapshot-cards"), button = snapshotButton("snapshot_cards", function () { return [$("accounts")]; });
    button.id = "snapshot-cards";
    if (old) old.parentNode.replaceChild(button, old);
    else $("refresh-all").parentNode.insertBefore(button, $("refresh-all"));
    if (!$("mask-cards")) {
      var eye = maskButton();
      eye.id = "mask-cards";
      button.parentNode.insertBefore(eye, button);
    }
  }

  // A camera button that copies a screenshot of the nodes nodesOf returns.
  function snapshotButton(label, nodesOf) {
    var ok = canCopyImage();
    var icon = svg("svg", { viewBox: "0 0 16 16", width: 15, height: 15, "aria-hidden": "true" });
    icon.appendChild(svg("path", { d: "M2 5.5A1.5 1.5 0 0 1 3.5 4h1.6l1.1-1.5h3.6L10.9 4h1.6A1.5 1.5 0 0 1 14 5.5v6a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 2 11.5z" }));
    icon.appendChild(svg("circle", { cx: 8, cy: 8.5, r: 2.4 }));
    var text = ok ? translate(label) : translate("snapshot_unsupported");
    return el("button", { type: "button", class: "btn btn-ghost btn-icon snapshot no-snapshot", title: text, "aria-label": text, disabled: !ok,
      onclick: function () { copySnapshot(nodesOf()); } }, [icon]);
  }

  // ---- Status rendering ----

  function renderHeader() {
    var state = $("state");
    clear(state);
    if (!status.running) state.appendChild(badge("warning", translate("state_starting")));
    else state.appendChild(badge("success", translate("state_running")));
    var parts = [];
    var last = parseTime(status.last_poll), next = parseTime(status.next_poll);
    if (last) parts.push(translate("last_poll", fmtClock(last), fmtAgo(last)));
    if (next) parts.push(translate("next_poll", fmtClock(next), fmtIn(next)));
    var meta = $("meta");
    clear(meta);
    meta.appendChild(document.createTextNode("v" + status.version));
    if (status.latest_version && status.release_url) meta.appendChild(updateLink());
    if (parts.length) meta.appendChild(document.createTextNode(" · " + parts.join(" · ")));
  }

  // A green ring with a green up arrow after the version, linking to the newer
  // release.
  function updateLink() {
    var hint = translate("update_hint", status.latest_version);
    var icon = svg("svg", { viewBox: "0 0 16 16", width: 14, height: 14, "aria-hidden": "true" });
    icon.appendChild(svg("circle", { cx: 8, cy: 8, r: 7.25 }));
    icon.appendChild(svg("path", { d: "M8 11.5V4.5M5 7.5l3-3 3 3" }));
    var link = outsideLink({ class: "update", title: hint, "aria-label": hint }, status.release_url, hint + translate("link_release"));
    link.appendChild(icon);
    return link;
  }

  // ---- Links ----

  var REPO_URL = "https://github.com/PosvdM/magpie-lamplighter";

  // Magpie's sandbox lets the page open no window, and its bridge has no
  // way to open a link, so a link to another site opens in a new tab only
  // outside the sandbox. In the sandbox a click shows the address in the
  // toast, selected for copying, after text.
  function outsideLink(attrs, url, text) {
    var link = el("a", attrs);
    link.setAttribute("href", url);
    if (!SANDBOXED) {
      link.setAttribute("target", "_blank");
      link.setAttribute("rel", "noopener noreferrer");
      return link;
    }
    link.addEventListener("click", function (e) {
      e.preventDefault();
      showAddress(text, url);
    });
    return link;
  }

  // The logo and the name link to the repository only where the link can
  // open; in the sandbox they show as plain text.
  function setupHome() {
    if (SANDBOXED) return;
    var home = $("home");
    home.setAttribute("href", REPO_URL);
    home.setAttribute("target", "_blank");
    home.setAttribute("rel", "noopener");
    home.setAttribute("data-i18n-title", "repo_hint");
  }

  // The toast with text and the address in a read-only field, selected.
  // It stays while the field has the focus.
  function showAddress(text, url) {
    var node = $("toast");
    clear(node);
    var field = el("input", { class: "toast-address", type: "text", readonly: true, spellcheck: "false", "aria-label": text, value: url });
    node.appendChild(el("div", { text: text }));
    node.appendChild(field);
    node.hidden = false;
    clearTimeout(toastTimer);
    var hide = function (ms) {
      clearTimeout(toastTimer);
      toastTimer = setTimeout(function () { node.hidden = true; }, ms);
    };
    field.addEventListener("focus", function () { clearTimeout(toastTimer); });
    field.addEventListener("blur", function () { hide(3000); });
    field.focus();
    field.select();
  }

  function notice(text) {
    return el("div", { class: "notice" }, [el("span", { class: "notice-icon", text: "!" }), el("span", {}, maskedNodes(text))]);
  }

  // At the top of the quota view while no time zone is set, with a button
  // to the time zone setting.
  function renderZoneNotice() {
    var box = $("zone-notice");
    clear(box);
    box.hidden = zoneSet(status);
    if (box.hidden) return;
    box.appendChild(el("span", { class: "notice-icon", text: "i" }));
    box.appendChild(el("span", { class: "notice-text", text: translate("zone_notice", utcText(status)) }));
    box.appendChild(el("button", { type: "button", class: "btn btn-secondary btn-sm", text: translate("zone_notice_go"), onclick: function () {
      showView("settings", true);
      var input = $("settings").elements.timezone;
      input.scrollIntoView({ block: "center" });
      input.focus({ preventScroll: true });
    } }));
  }

  // The accounts Magpie warms up (status.magpie_warmup, a list of pins), by
  // the names the page shows: the service and the account name, else the
  // email; an account the status does not list by its name in
  // account_names, else its pin.
  function warmupNames(pins, accounts, names, titleOf) {
    if (!Array.isArray(pins)) return [];
    return pins.filter(function (pin) { return typeof pin === "string" && pin; }).map(function (pin) {
      var account = (accounts || []).filter(function (a) { return a.auth_index === pin; })[0];
      var saved = names && typeof names[pin] === "string" ? names[pin].split(/\r?\n/)[0].trim() : "";
      if (!account) return saved || pin;
      var name = (account.note || "").split(/\r?\n/)[0].trim() || account.email || saved || pin;
      return titleOf(account.provider) + " " + name;
    });
  }

  function renderNotices() {
    var box = $("notices");
    clear(box);
    if (status.config_error) box.appendChild(notice(translate("config_error", status.config_error)));
    if (status.list_error) box.appendChild(notice(status.list_error));
    if (status.models_error) box.appendChild(notice(translate("models_error", status.models_error)));
    if (status.notify_error) box.appendChild(notice(translate("notify_error", status.notify_error)));
    if (status.public) {
      if (!status.public.running && status.public.error) box.appendChild(notice(translate("public_error", status.public.error)));
    } else if (status.mcp && status.mcp.enabled && !status.mcp.running && status.mcp.error) box.appendChild(notice(translate("mcp_error", status.mcp.error)));
    var warmup = warmupNames(status.magpie_warmup, status.accounts, rawConfig.account_names, providerTitle);
    if (warmup.length) box.appendChild(notice(translate("warmup_notice", warmup.join(lang === "zh" ? "、" : ", "))));
    renderZoneNotice();
    // No channel is how notifications are turned off, so it is a hint in the
    // settings rather than a notice.
    $("notify-off").hidden = !(status.config && !status.config.bark_url && !(status.config.webhook && status.config.webhook.url));
  }

  // An empty window shows no fill: its whole track turns dark red and
  // striped instead.
  function quotaRow(group, w, index, account) {
    var remaining = Math.max(0, Math.min(100, w.remaining));
    var level = quotaLevel(w.remaining);
    var reset = parseTime(w.reset);
    var meta = [el("span", { class: "quota-percent" + (level === "empty" ? " percent-empty" : ""), text: Math.round(remaining) + "%" })];
    if (reset) {
      var left = reset.getTime() - Date.now();
      meta.push(el("span", { class: "quota-reset", text: fmtDateTimeDay(reset) }));
      meta.push(el("span", { class: "quota-countdown" + (left > 0 && left <= 3600000 ? " soon" : ""), text: fmtCountdown(left) }));
    }
    return el("div", { class: "quota-row" }, [
      el("div", { class: "quota-row-head" }, [
        el("span", { class: "quota-name", text: windowName(w) }),
        el("span", { class: "quota-meta" }, meta)
      ]),
      el("div", { class: "quota-bar" + (level === "empty" ? " bar-empty" : ""), role: "meter", "aria-valuemin": "0", "aria-valuemax": "100",
        "aria-valuenow": String(Math.round(remaining)), "aria-label": translate("remaining_aria", masked ? accountAlias(account) : group.label, windowName(w)) }, [
        level === "empty" ? null : el("span", { class: "quota-fill fill-" + level, style: "width:" + remaining + "%;--meter-index:" + index })
      ])
    ]);
  }

  function freshness(group) {
    var latest = null, source = "";
    (group.windows || []).forEach(function (w) {
      var t = parseTime(w.observed_at);
      if (t && (!latest || t > latest)) { latest = t; source = w.source; }
    });
    if (!latest) return null;
    return el("div", { class: "group-foot" }, [
      el("span", { class: "source source-" + source, text: source === "passive" ? translate("source_passive") : translate("source_active") }),
      el("span", { text: translate("updated", fmtAgo(latest)) })
    ]);
  }

  // ---- Account order ----

  // Cards follow account_order, a list of credential file names, the key
  // account_weights uses too. The status API already returns the accounts
  // in this order; the page sorts them again by the config it last read, so
  // a new order shows at once instead of after the plugin applies it.
  // Accounts not in the list keep their order from the API, after the
  // listed ones. pendingOrder is an order being saved; it stays in place
  // over configs read before that save finishes.
  var pendingOrder = null;

  function sortAccounts() {
    if (!status || !status.accounts) return;
    var order = Array.isArray(rawConfig.account_order) ? rawConfig.account_order : [];
    var pos = {};
    order.forEach(function (name, i) { if (!Object.prototype.hasOwnProperty.call(pos, name)) pos[name] = i; });
    var rank = function (a) { return Object.prototype.hasOwnProperty.call(pos, a.name) ? pos[a.name] : order.length; };
    status.accounts = status.accounts.map(function (a, i) { return { a: a, i: i }; }).sort(function (x, y) {
      return (rank(x.a) - rank(y.a)) || (x.i - y.i);
    }).map(function (x) { return x.a; });
  }

  // Saves the full list of credential file names in their new order through
  // the save queue. Cards and chart show it at once.
  function saveAccountOrder(names) {
    var say = claimState($("account-order-state"));
    pendingOrder = names.slice();
    rawConfig.account_order = names.slice();
    sortAccounts();
    renderAccounts();
    if (history) renderChart();
    saveQueue = saveQueue.then(function () {
      return patchConfig({ account_order: names }).then(function (token) {
        showApplied(say, token);
        if (pendingOrder && pendingOrder.join("\n") === names.join("\n")) pendingOrder = null;
        return rereadConfig();
      }, function (err) {
        if (pendingOrder && pendingOrder.join("\n") === names.join("\n")) pendingOrder = null;
        say("failed", err.message);
        if (err.message !== "unauthorized") toast(translate("order_failed", err.message));
        return loadConfig().catch(function () {}).then(function () {
          sortAccounts();
          renderAccounts();
          if (history) renderChart();
        });
      });
    });
  }

  function reducedMotion() { return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches; }

  function accountRows() { return Array.prototype.slice.call($("account-rows").children); }

  // FLIP begins at each row's current painted position, even if its previous
  // move is unfinished. Drop targets use offsetTop instead, without transforms.
  function animateAccountRows(change) {
    var rows = accountRows(), before = rows.map(function (row) { return row.getBoundingClientRect().top; });
    rows.forEach(function (row) { if (row.orderAnimation) row.orderAnimation.cancel(); });
    change();
    rows.forEach(function (row, i) {
      if (row.classList.contains("account-placeholder")) return;
      var dy = before[i] - row.getBoundingClientRect().top;
      if (dy && row.animate && !reducedMotion()) {
        row.orderAnimation = row.animate([{ transform: "translateY(" + dy + "px)" }, { transform: "none" }],
          { duration: 180, easing: getComputedStyle(row).getPropertyValue("--ease-out-strong").trim() });
      }
    });
  }

  // The original row is a placeholder; an inert fixed copy follows the
  // pointer. Status refreshes wait through both dragging and settling.
  var accountDrag = null;
  var accountsBehind = false;

  function startAccountDrag(e, row, handle) {
    if (e.button !== 0 || e.isPrimary === false || accountDrag) return;
    e.preventDefault();
    var active = document.activeElement;
    if (active && active.closest("#account-rows") && active.tagName === "INPUT") active.blur();
    if (row.orderAnimation) row.orderAnimation.cancel();
    var rect = row.getBoundingClientRect(), copy = row.cloneNode(true);
    copy.classList.add("account-floating");
    copy.setAttribute("aria-hidden", "true");
    copy.setAttribute("inert", "");
    copy.style.width = rect.width + "px";
    copy.style.height = rect.height + "px";
    copy.style.left = rect.left + "px";
    copy.style.top = rect.top + "px";
    $("settings").appendChild(copy);
    accountDrag = { row: row, copy: copy, handle: handle, pointer: e.pointerId,
      before: accountRows(), top: rect.top, grab: e.clientY - rect.top, y: e.clientY, settling: false };
    row.classList.add("account-placeholder");
    $("account-rows").classList.add("sorting");
    handle.setPointerCapture(e.pointerId);
    document.addEventListener("pointermove", moveAccountDrag);
    document.addEventListener("pointerup", endAccountDrag);
    document.addEventListener("pointercancel", endAccountDrag);
    document.addEventListener("keydown", cancelAccountDrag);
    window.addEventListener("scroll", scrollAccountDrag, true);
  }

  // The index is determined by the other rows' layout midpoints. Animated
  // transforms never feed back into hit testing, so a stationary pointer
  // cannot make rows oscillate between positions.
  function accountDropIndex(rows, y, top) {
    var index = 0;
    rows.forEach(function (row) { if (y > top + row.offsetTop + row.offsetHeight / 2) index++; });
    return index;
  }

  function moveAccountDrag(e) {
    var drag = accountDrag;
    if (!drag || drag.settling || e.pointerId !== drag.pointer) return;
    drag.y = e.clientY;
    drag.copy.style.transform = "translateY(" + (e.clientY - drag.grab - drag.top) + "px)";
    var box = $("account-rows"), rows = accountRows().filter(function (row) { return row !== drag.row; });
    var index = accountDropIndex(rows, e.clientY, box.getBoundingClientRect().top);
    var next = rows[index] || null;
    if (drag.row.nextElementSibling !== next) animateAccountRows(function () { box.insertBefore(drag.row, next); });
  }

  function scrollAccountDrag() {
    if (accountDrag) moveAccountDrag({ pointerId: accountDrag.pointer, clientY: accountDrag.y });
  }

  function cancelAccountDrag(e) {
    if (e.key !== "Escape" || !accountDrag) return;
    e.preventDefault();
    endAccountDrag({ pointerId: accountDrag.pointer, type: "pointercancel" });
  }

  function endAccountDrag(e) {
    var drag = accountDrag;
    if (!drag || drag.settling || e.pointerId !== drag.pointer) return;
    drag.settling = true;
    document.removeEventListener("pointermove", moveAccountDrag);
    document.removeEventListener("pointerup", endAccountDrag);
    document.removeEventListener("pointercancel", endAccountDrag);
    document.removeEventListener("keydown", cancelAccountDrag);
    window.removeEventListener("scroll", scrollAccountDrag, true);
    if (drag.handle.hasPointerCapture(drag.pointer)) drag.handle.releasePointerCapture(drag.pointer);
    var cancelled = e.type === "pointercancel", box = $("account-rows");
    if (cancelled) animateAccountRows(function () { drag.before.forEach(function (row) { box.appendChild(row); }); });
    var rect = drag.row.getBoundingClientRect(), from = drag.copy.getBoundingClientRect();
    var finish = function () {
      drag.copy.remove();
      drag.row.classList.remove("account-placeholder");
      box.classList.remove("sorting");
      var names = accountRows().map(function (row) { return row.getAttribute("data-account"); });
      var before = drag.before.map(function (row) { return row.getAttribute("data-account"); });
      accountDrag = null;
      if (!cancelled && names.join("\n") !== before.join("\n")) saveAccountOrder(names);
      else if (accountsBehind) renderAccounts();
      else renderAccountSettings();
    };
    if (drag.copy.animate && !reducedMotion()) {
      var animation = drag.copy.animate([
        { transform: "translateY(" + (from.top - drag.top) + "px)" },
        { transform: "translate(" + (rect.left - from.left) + "px," + (rect.top - drag.top) + "px)" }
      ], { duration: 180, easing: getComputedStyle(drag.copy).getPropertyValue("--ease-out-strong").trim(), fill: "forwards" });
      animation.finished.then(finish, finish);
    } else finish();
  }

  function makeAccountSetting(account) {
    var row = el("div", { class: "account-setting", "data-account": account.name });
    var handle = el("span", { class: "drag-handle", "aria-hidden": "true" });
    handle.addEventListener("pointerdown", function (e) { startAccountDrag(e, row, handle); });
    row.appendChild(handle);
    row.appendChild(el("span", { class: "type-badge" }));
    row.appendChild(el("div", { class: "account-identity" }, [
      el("span", { class: "strong " + SENSITIVE }), el("span", { class: "account-email " + SENSITIVE })
    ]));
    ["name", "weight"].forEach(function (kind) {
      var input = el("input", { type: kind === "weight" ? "number" : "text", class: SENSITIVE,
        "data-account-field": kind, autocomplete: "off", spellcheck: "false" });
      if (kind === "weight") { input.step = "any"; input.min = "0"; }
      input.addEventListener("change", function () { saveAccountField(input, row); });
      row.appendChild(el("label", {}, [el("span", { class: "field-label" }, [
        el("span", { text: translate(kind === "name" ? "rename" : "weight") })
      ]), input]));
    });
    return row;
  }

  function renderAccountSettings() {
    if (accountDrag) { accountsBehind = true; return; }
    var box = $("account-rows"), counts = {}, existing = new Map();
    var active = box.contains(document.activeElement) ? document.activeElement : null;
    var selection = active && active.type === "text" ? [active.selectionStart, active.selectionEnd] : null;
    status.accounts.forEach(function (a) { counts[a.provider] = (counts[a.provider] || 0) + 1; });
    accountRows().forEach(function (row) { existing.set(row.getAttribute("data-account"), row); });
    status.accounts.forEach(function (account, index) {
      var row = existing.get(account.name) || makeAccountSetting(account);
      existing.delete(account.name);
      row.account = account;
      // Avoid moving unchanged nodes: reparenting a focused input blurs it.
      if (box.children[index] !== row) box.insertBefore(row, box.children[index] || null);
      var spoken = masked ? accountAlias(index) : account.email || account.name;
      var badge = row.querySelector(".type-badge");
      badge.className = "type-badge type-" + account.provider;
      badge.textContent = providerTitle(account.provider);
      var identity = row.querySelector(".account-identity");
      identity.firstChild.textContent = account.title;
      identity.lastChild.textContent = account.email || account.name;
      [identity.firstChild, identity.lastChild].forEach(function (node) {
        if (masked) node.removeAttribute("title"); else node.title = node.textContent;
      });
      row.querySelector(".drag-handle").title = translate("drag_handle");
      Array.prototype.forEach.call(row.querySelectorAll("[data-account-field]"), function (input) {
        var kind = input.getAttribute("data-account-field"), weight = kind === "weight";
        input.parentNode.firstChild.firstChild.textContent = translate(weight ? "weight" : "rename");
        input.setAttribute("aria-label", translate(weight ? "account_weight_aria" : "account_name_aria", spoken));
        input.parentNode.classList.toggle("account-weight-empty", weight && counts[account.provider] <= 1);
        input.disabled = weight && counts[account.provider] <= 1;
        saveEntry(saveHost(input), input);
        var saved = rawConfig.account_weights && Number(rawConfig.account_weights[account.name]);
        var value = weight ? (saved > 0 ? fmtWeight(saved) : "") : (account.note || "").split(/\r?\n/)[0].trim();
        if (!input.accountDirty && !input.accountPending && document.activeElement !== input) {
          input.value = value;
          input.accountSaved = value;
        }
        if (weight) input.placeholder = translate("weight_default", fmtWeight(Number(account.default_weight) > 0 ? Number(account.default_weight) : 1));
      });
    });
    existing.forEach(function (row) { row.remove(); });
    if (active && active.isConnected && document.activeElement !== active) {
      active.focus({ preventScroll: true });
      if (selection) active.setSelectionRange(selection[0], selection[1]);
    }
  }

  function renderAccounts() {
    // Rebuilding the cards would drop the one being dragged.
    if (accountDrag) { accountsBehind = true; return; }
    accountsBehind = false;
    renderAccountSettings();
    var box = $("accounts");
    clear(box);
    if (!status.accounts.length) {
      var titles = providerList().map(function (p) { return p.title; });
      box.appendChild(el("p", { class: "empty", text: status.running ? translate("no_accounts", listText(titles)) : translate("first_query") }));
      return;
    }
    var meterIndex = 0;
    // Weights only matter for a total, so they show on services with
    // several accounts.
    var counts = {};
    status.accounts.forEach(function (a) { counts[a.provider] = (counts[a.provider] || 0) + 1; });
    var hidden = hiddenGroups();
    status.accounts.forEach(function (account, index) {
      var groups = (account.groups || []).filter(function (g) { return !groupHidden(hidden, account.provider, g.source_label); });
      // An account whose quota groups are all hidden shows no card.
      if (!groups.length && (account.groups || []).length) return;
      var card = el("div", { class: "card account", "data-account": account.name });
      var pooledAccount = counts[account.provider] > 1, weight = accountWeight(account);
      var refresh = el("button", { type: "button", class: "btn btn-ghost btn-sm no-snapshot", text: translate("refresh"), onclick: function () {
        runAction(refresh, API + "/refresh", { auth_index: account.auth_index }, translate("refreshed", account.title));
      } });
      card.appendChild(el("div", { class: "account-head" }, [
        el("span", { class: "type-badge type-" + account.provider }, maskedNodes(account.title).concat(
          pooledAccount && weight !== 1 ? [el("span", { class: "account-weight", title: translate("weight"), text: "×" + fmtWeight(weight) })] : []
        )),
        el("span", { class: "account-email " + SENSITIVE, title: masked ? null : account.email || account.name, text: account.email || account.name })
      ]));
      groups.forEach(function (group) {
        var block = el("div", { class: "quota-group" });
        if (group.source_label && group.source_label !== providerTitle(account.provider)) {
          block.appendChild(el("div", { class: "group-title", text: group.source_label }));
        }
        (group.windows || []).forEach(function (w) { block.appendChild(quotaRow(group, w, meterIndex++, index)); });
        var foot = freshness(group);
        if (foot) block.appendChild(foot);
        card.appendChild(block);
      });
      if (account.error) card.appendChild(el("div", { class: "error-text" }, maskedNodes(account.error)));
      card.appendChild(el("div", { class: "account-head cell-note" }, [
        el("span", { class: "cell-note", text: account.plan ? translate("plan", account.plan) : "" }),
        refresh,
        snapshotButton("snapshot_card", function () { return [card]; })
      ]));
      box.appendChild(card);
    });
  }

  function targetStatus(t) {
    var tr = translate;
    if (t.blocked_window) {
      var until = parseTime(t.blocked_until);
      var blocked = t.blocked_window === "5h" || t.blocked_window === "7d" ? tr("blocked_" + t.blocked_window) : tr("blocked_other", t.blocked_window);
      return { kind: "muted", text: blocked, title: until ? tr("blocked_until", fmtDateTime(until)) : tr("blocked_wait") };
    }
    var circuit = parseTime(t.circuit_until);
    if (circuit) return { kind: "failure", text: tr("paused_until", fmtDateTime(circuit)), title: t.circuit_reason };
    if (t.consecutive_failures > 0) return { kind: "warning", text: tr("failures", t.consecutive_failures), title: t.last_error };
    if (t.rolling) return { kind: "muted", text: tr("rolling") };
    return { kind: "success", text: tr("normal") };
  }

  // The ignition card shows only while ignition is on.
  function renderTargets() {
    var body = $("targets");
    clear(body);
    var ignition = (status.config && status.config.ignition) || {};
    $("ignition-card").hidden = !ignition.enabled;
    if (!ignition.enabled) return;
    var endMinutes = (ignition.end_hour || 0) * 60 + (ignition.end_grace_minutes || 0);
    $("ignition-note").textContent = translate("ignition_note", pad(ignition.start_hour), ignition.grace_seconds, pad(Math.floor(endMinutes / 60) % 24) + ":" + pad(endMinutes % 60));
    if (!status.targets.length) {
      body.appendChild(el("tr", {}, [el("td", { colspan: "7", class: "empty", text: translate("no_targets") })]));
      return;
    }
    status.targets.forEach(function (t) {
      var st = targetStatus(t), title = groupTitle(t.key, t.label);
      var button = el("button", { type: "button", class: "btn btn-secondary btn-sm", text: translate("ignite_now"), onclick: function () {
        if (!confirmed(button, translate("ignite_confirm", title))) return;
        runAction(button, API + "/ignite", { target: t.key }, translate("ignited", title));
      } });
      var next = parseTime(t.next_due), reset = parseTime(t.reset), last = parseTime(t.last_success);
      var statusCell = el("td", {}, [badge(st.kind, st.text)]);
      if (st.title) statusCell.appendChild(el("div", { class: "cell-note" }, maskedNodes(st.title)));
      body.appendChild(el("tr", {}, [
        el("td", { class: "strong" }, maskedNodes(title)),
        el("td", { class: "mono", text: next ? fmtDateTimeDay(next) : "—" }),
        el("td", { class: "mono", text: reset ? fmtDateTimeDay(reset) : "—" }),
        el("td", { class: "mono", text: last ? fmtDateTime(last) : "—" }),
        el("td", { class: "mono", text: t.last_model || "—" }),
        statusCell,
        el("td", { class: "actions" }, [button])
      ]));
    });
  }

  // ---- Events ----

  // Badge style per event type; the label is ev_<kind> in the dictionary.
  var EVENT_TYPES = {
    ignite: "success", ignite_manual: "success", ignite_failed: "warning", ignite_paused: "failure", model_refused: "warning",
    plan_change: "warning", notify: "muted", notify_failed: "warning", codex_reset: "muted", error: "failure",
    reset: "success", exhausted: "warning", credential_invalid: "failure", credential_restored: "success"
  };
  var EVENTS_COLLAPSED = 12, EVENTS_MAX = 100;
  var eventsExpanded = false;
  // Resets of 5-hour windows, one per service every five hours, show only
  // when chosen; the choice is kept in events_show_5h_resets of the config.
  var showFiveHourResets = false;

  // The events a notification reports, by its notify_kind. A notification
  // sent within NOTIFY_LAG of such an event of the same quota group shows
  // as a mark on that event's row instead of a row of its own.
  var NOTIFY_TRIGGERS = {
    recovery: ["reset"], circuit: ["ignite_paused"], plan: ["plan_change"],
    credential: ["credential_invalid"], suspended: ["credential_invalid"], rate_limited: ["credential_invalid"],
    credential_ok: ["credential_restored"], codex_reset: ["codex_reset"]
  };
  // How long, in seconds, a notification may follow its event, as [from,
  // to]. Notifications that failed are retried with later polls for up to
  // an hour, so most kinds allow 70 minutes. Did Codex Reset records its
  // event after sending, so its notifications come a little before it.
  var NOTIFY_LAG = {
    recovery: [0, 4200], circuit: [0, 4200], plan: [0, 4200], credential: [0, 4200],
    suspended: [0, 4200], rate_limited: [0, 4200], credential_ok: [0, 4200], codex_reset: [-300, 300]
  };
  // An ignition pause this many seconds from a credential problem of the
  // same quota group is part of it and shares its row.
  var PAUSE_PAIR = 60;
  // Error texts longer than this are cut in the list; the tooltip keeps all.
  var ERROR_CHARS = 160;

  function groupLabel(key) {
    var label = "";
    ((status && status.accounts) || []).forEach(function (a) {
      (a.groups || []).forEach(function (g) { if (g.key === key) label = g.label; });
    });
    return label;
  }

  // The quota groups of the accounts with more than one, by group key, with
  // the provider of the account. Their labels, such as "Claude / GPT" of an
  // Antigravity account, name models rather than the service, so the chart,
  // its table, the ignition table and the events name the service too. A
  // card shows the groups under the service badge and needs no such name.
  function sharedGroups(accounts) {
    var out = {};
    (accounts || []).forEach(function (a) {
      if ((a.groups || []).length > 1) a.groups.forEach(function (g) { out[g.key] = a.provider; });
    });
    return out;
  }

  // The quota groups that hidden_groups of the applied config hides, as a
  // set of "<provider>|<source label>". The cards and the chart leave them
  // out; the plugin still queries and records them.
  function hiddenGroups() {
    var out = {}, list = status && status.config && status.config.hidden_groups;
    (Array.isArray(list) ? list : []).forEach(function (id) { if (typeof id === "string") out[id] = true; });
    return out;
  }

  // list, a saved hidden_groups, as config.Parse reads it: its strings
  // trimmed of the white space strings.TrimSpace trims (PUBLIC_URL_TRIM),
  // without empty ones and duplicates. A list written by hand may differ
  // from the applied one the checkboxes show.
  function hiddenList(list) {
    var out = [];
    (Array.isArray(list) ? list : []).forEach(function (id) {
      if (typeof id !== "string") return;
      id = id.replace(PUBLIC_URL_TRIM, "");
      if (id && out.indexOf(id) < 0) out.push(id);
    });
    return out;
  }

  function groupHidden(hidden, provider, sourceLabel) {
    return !!sourceLabel && hidden[provider + "|" + sourceLabel] === true;
  }

  // "Antigravity · " before the label of a group of sharedGroups.
  function servicePrefix(provider) {
    var title = provider ? providerTitle(provider) : "";
    return title ? title + " · " : "";
  }

  // A short form of the service, for chart rows too narrow for its title:
  // "AG" for Antigravity, the initials of a title of several words, else
  // its first two letters.
  var SERVICE_SHORT = { antigravity: "AG" };
  function serviceShort(provider) {
    if (SERVICE_SHORT[provider]) return SERVICE_SHORT[provider];
    var title = provider ? providerTitle(provider) : "";
    if (title.length <= 3) return title;
    var words = title.split(/[\s-]+/).filter(Boolean);
    return (words.length > 1 ? words.map(function (w) { return w[0]; }).join("") : title.slice(0, 2)).toUpperCase();
  }

  function shortPrefix(provider) {
    var short = provider ? serviceShort(provider) : "";
    return short ? short + " · " : "";
  }

  // The title of a quota group label as it shows apart from its account
  // card, for provider as sharedGroups gives it (none for an account with
  // one group): name, the service in front (prefix) and its short form for
  // a narrow chart (short). A label that already names the service, such
  // as "Claude" of the main group of a Claude account or "Antigravity#main"
  // of a credential event, stays as it is. The chart's rows, its totals,
  // its table, the ignition table and the events all take their titles
  // here.
  function groupName(provider, label) {
    var title = provider && label ? providerTitle(provider) : "";
    if (!title || label === title || label.indexOf(title + "#") === 0) return { name: label, prefix: "", short: "" };
    var prefix = servicePrefix(provider);
    return { name: prefix + label, prefix: prefix, short: shortPrefix(provider) };
  }

  // The title of the label of quota group key.
  function groupTitle(key, label) {
    return groupName(sharedGroups(status && status.accounts)[key], label).name;
  }

  // Events of an account rather than one of its quota groups: credential
  // problems, their end, and their notifications. Their group key is the
  // account's first quota group only so that the event finds its account.
  var ACCOUNT_EVENTS = { credential_invalid: true, credential_restored: true };
  var ACCOUNT_NOTIFY_KINDS = { credential: true, suspended: true, rate_limited: true, credential_ok: true };

  // The label of an event in the list and the chart's tooltips: a quota
  // group's label with its service as groupTitle gives it, an account's as
  // it is.
  function eventTitle(ev, label) {
    if (ACCOUNT_EVENTS[ev.kind]) return label;
    if ((ev.kind === "notify" || ev.kind === "notify_failed") && ACCOUNT_NOTIFY_KINDS[notifyKind(ev)]) return label;
    return groupTitle(ev.group, label);
  }

  function retryText(seconds) {
    return seconds < 60 ? translate("ev_seconds", seconds) : translate("ev_minutes", Math.round(seconds / 60));
  }

  function cutText(text, max) {
    var chars = Array.from(text);
    return chars.length > max ? chars.slice(0, max - 1).join("").replace(/\s+$/, "") + "…" : text;
  }

  // One line for an error text, which can hold a JSON body from the
  // provider over many lines: the text before the body, then the status
  // code, reason and message from it. Without a JSON body, the first line.
  function errorSummary(text) {
    text = String(text == null ? "" : text).trim();
    var start = text.indexOf("{"), end = text.lastIndexOf("}"), body = null;
    if (start >= 0 && end > start) {
      try { body = JSON.parse(text.slice(start, end + 1)); } catch (err) { body = null; }
    }
    if (!body || typeof body !== "object") {
      var first = text.split(/\r?\n/).filter(function (line) { return line.trim(); })[0] || "";
      return cutText(first.trim(), ERROR_CHARS);
    }
    var e = body.error && typeof body.error === "object" ? body.error : body;
    var code = null, reason = "", message = "";
    [e.code, e.status, body.status, body.code].forEach(function (v) {
      if (code == null && (typeof v === "number" || /^\d{3}$/.test(String(v)))) code = String(v);
    });
    var http = text.slice(end + 1).match(/HTTP (\d{3})/) || text.slice(0, start).match(/HTTP (\d{3})/);
    if (code == null && http) code = http[1];
    (Array.isArray(e.details) ? e.details : []).forEach(function (d) {
      if (!reason && d && typeof d.reason === "string") reason = d.reason;
    });
    [e.status, e.type, e.code, typeof body.error === "string" ? body.error : null].forEach(function (v) {
      if (!reason && typeof v === "string" && v && !/^\d+$/.test(v)) reason = v;
    });
    [e.message, body.message, body.error_description, e.error_description].forEach(function (v) {
      if (!message && typeof v === "string" && v.trim()) message = v.trim().split(/\r?\n/)[0];
    });
    var parts = [code != null ? "HTTP " + code : "", reason, message].filter(function (s) { return s; });
    var prefix = text.slice(0, start).trim();
    var line = parts.join(" · ");
    if (prefix) line = prefix + (/[：:]$/.test(prefix) ? (/：$/.test(prefix) ? "" : " ") : " ") + line;
    return cutText(line || text.split(/\r?\n/)[0].trim(), ERROR_CHARS);
  }

  // What a notification reports. Notifications recorded before notify_kind
  // existed are told by their title, in either language; see
  // internal/notify/text.go.
  function notifyKind(ev) {
    var p = ev.params || {};
    if (p.notify_kind) return p.notify_kind;
    var title = String(p.title || ev.detail || ev.message || "").replace(/^推送失败 /, "");
    var starts = function (prefix) { return title.indexOf(prefix) === 0; };
    if (/^(📅|✅) Codex /.test(title)) return "codex_reset";
    if (starts("✅")) return "recovery";
    if (title.indexOf("点火已暂停") >= 0 || title.indexOf("ignition paused") >= 0) return "circuit";
    if (starts("🎫")) return "plan";
    if (starts("🟡") || starts("🔴")) return "quota";
    if (starts("⏰")) return "reminder";
    return "";
  }

  // The kind ("5h", "7d" or "") and short label of the window of a reset or
  // exhausted event. Events recorded without them have only the window ID.
  function eventWindow(p) {
    var id = String(p.window || "");
    if (p.label) return { kind: p.kind || "", label: p.label };
    var kind = id === "five-hour" || /-5h$/.test(id) ? "5h" : id === "seven-day" || id.indexOf("weekly") >= 0 ? "7d" : "";
    return { kind: kind, label: kind || id };
  }

  // Whether two events concern the same quota group. Notifications and some
  // events were recorded without the group and only have its label.
  function sameSubject(a, b) {
    if (a.group && b.group) return a.group === b.group;
    return !!a.label && a.label === b.label;
  }

  function eventSeconds(ev) { var t = parseTime(ev.time); return t ? t.getTime() / 1000 : 0; }

  // The rows of the event list, newest first like events: a notification
  // that reports an event joins that event's row (pushes); one that reports
  // none, such as a quota alert, stays a row of its own. A recovery joins
  // the reset of each window it names, or without window names the latest
  // reset and those of the same poll. Other notifications join their latest
  // event. Resets of 5-hour windows are left out unless chosen, together
  // with their notifications; hidden counts them.
  function eventRows(events, showFiveHour) {
    var rows = events.map(function (ev) { return { event: ev, t: eventSeconds(ev), pushes: [] }; });
    // A credential problem found by ignition also pauses it: the pause
    // joins the problem's row (paused), and so do its notifications.
    rows.forEach(function (row) {
      if (row.event.kind !== "ignite_paused") return;
      var host = rows.filter(function (other) {
        return other.event.kind === "credential_invalid" && !other.paused && Math.abs(other.t - row.t) <= PAUSE_PAIR && sameSubject(row.event, other.event);
      }).sort(function (a, b) { return Math.abs(a.t - row.t) - Math.abs(b.t - row.t); })[0];
      if (!host) return;
      host.paused = row.event;
      row.host = host;
    });
    rows.forEach(function (row) {
      var ev = row.event;
      if (ev.kind !== "notify" && ev.kind !== "notify_failed") return;
      row.notifyKind = notifyKind(ev);
      var triggers = NOTIFY_TRIGGERS[row.notifyKind];
      if (!triggers) return;
      var p = ev.params || {};
      var windows = row.notifyKind === "recovery" && p.windows ? p.windows.split(",").map(function (w) { return w.trim(); }) : null;
      var lag = NOTIFY_LAG[row.notifyKind];
      // Did Codex Reset concerns no account: its events and notifications
      // have no quota group.
      var global = row.notifyKind === "codex_reset";
      var hits = rows.filter(function (other) {
        var o = other.event, dt = row.t - other.t;
        if (triggers.indexOf(o.kind) < 0 || dt < lag[0] || dt > lag[1]) return false;
        if (global ? ev.group || o.group : !sameSubject(ev, o)) return false;
        return !windows || windows.indexOf((o.params || {}).window) >= 0;
      }).sort(function (a, b) { return Math.abs(row.t - a.t) - Math.abs(row.t - b.t); })
        .map(function (h) { return h.host || h; });
      if (!hits.length) return;
      var chosen;
      if (windows) {
        var seen = {};
        chosen = hits.filter(function (h) {
          var w = (h.event.params || {}).window;
          if (seen[w]) return false;
          seen[w] = true;
          return true;
        });
      } else if (row.notifyKind === "recovery") {
        chosen = hits.filter(function (h) { return Math.abs(hits[0].t - h.t) <= 60; });
      } else chosen = [hits[0]];
      chosen.forEach(function (h) { h.pushes.push(ev); });
      row.merged = true;
    });
    var hidden = 0, fiveHour = 0;
    var out = rows.filter(function (row) {
      if (row.merged || row.host) return false;
      var ev = row.event;
      if (ev.kind === "reset" && eventWindow(ev.params || {}).kind === "5h") {
        fiveHour++;
        if (!showFiveHour) { hidden++; return false; }
      }
      return true;
    });
    return { rows: out, hidden: hidden, fiveHour: fiveHour };
  }

  // A notification title without the label the label column shows, such as
  // "🟡 GPT · 5h 45%" under the label GPT.
  function notifyTitle(title, label) {
    title = String(title || "");
    var at = label ? title.indexOf(label + " · ") : -1;
    return at >= 0 && at <= 3 ? title.slice(0, at) + title.slice(at + label.length + 3) : title;
  }

  // How an event shows in the list, in the page language: the badge style
  // and text, the label, a one-line detail, the full text for the tooltip
  // (tip), an appeal link, and the texts of the detail that are sensitive
  // as a whole (secrets): error texts from the provider, which may name an
  // account. whole marks a detail that is sensitive as a whole: the detail
  // of an error event, and of an older event without a label, whose message
  // may name an account that no longer exists. Events written before params
  // existed keep their stored Chinese text.
  function eventView(ev, notifyKindOf, paused) {
    var p = ev.params || {};
    var known = EVENT_TYPES[ev.kind];
    var view = {
      style: known || (ev.level === "error" ? "failure" : ev.level === "warn" ? "warning" : "muted"),
      badge: known ? translate("ev_" + ev.kind) : ev.kind,
      label: ev.label || (ev.group ? groupLabel(ev.group) : ""),
      detail: "", tip: "", secrets: [], whole: false, link: ""
    };
    // Masked, these details hide whole: errors, and older events without
    // params that stored no label or whose text is a failure or warning,
    // as their text may name accounts since removed or renamed. The label
    // looked up from the current quota group does not count: it may differ
    // from the name in the text.
    view.whole = ev.kind === "error" || (!ev.params && (!ev.label || view.style === "failure" || view.style === "warning"));
    var time = function (value) { return fmtDateTime(parseTime(value)); };
    var error = function (summary, full) {
      var line = summary || errorSummary(full);
      view.secrets.push(line, String(full || ""));
      return line;
    };
    if (!ev.params) {
      var detail = ev.detail || ev.message || "";
      if (!ev.detail && view.label && detail.indexOf(view.label + " ") === 0) detail = detail.slice(view.label.length + 1);
      if (!ev.detail) detail = detail.replace(/^点火成功（(.+?)），下次重置 /, "$1 · 下次重置 ");
      view.detail = errorSummary(detail);
      view.tip = detail;
      return view;
    }
    switch (ev.kind) {
      case "ignite": case "ignite_manual":
        view.detail = p.reset ? translate("ev_ignited", p.model, time(p.reset)) : p.model || "";
        break;
      case "ignite_failed":
        // A manual ignition is not retried.
        if (p.manual === "1") {
          view.detail = error(p.summary, p.error);
          view.tip = p.error || view.detail;
          break;
        }
        view.detail = translate(p.cooldown ? "ev_retry_cooldown" : "ev_retry", retryText(Number(p.retry_seconds) || 0), error(p.summary, p.error));
        view.tip = translate(p.cooldown ? "ev_retry_cooldown" : "ev_retry", retryText(Number(p.retry_seconds) || 0), p.error || "");
        break;
      case "ignite_paused":
        // A pause for an invalid credential or a suspended account has no
        // end; it lasts until the credential changes.
        var pausedKey = parseTime(p.until) ? "ev_paused" : p.reason === "auth" || p.reason === "suspended" ? "ev_paused_" + p.reason : "";
        var pausedLine = error(p.summary, p.error);
        view.detail = !pausedKey ? pausedLine : pausedKey === "ev_paused" ? translate(pausedKey, time(p.until), pausedLine) : translate(pausedKey, pausedLine);
        view.tip = !pausedKey ? p.error || pausedLine : pausedKey === "ev_paused" ? translate(pausedKey, time(p.until), p.error || "") : translate(pausedKey, p.error || "");
        break;
      case "model_refused":
        view.detail = translate("ev_refused", p.model, p.fallback, p.hours);
        break;
      case "notify": case "notify_failed":
        var kind = notifyKindOf || notifyKind(ev);
        if (ev.kind === "notify" && (kind === "quota" || kind === "reminder")) view.badge = translate("ev_notify_" + kind);
        if (!view.label && kind === "codex_reset") view.label = "Did Codex Reset";
        var title = notifyTitle(p.title, view.label);
        if (ev.kind === "notify") view.detail = title;
        else {
          view.detail = translate("ev_notify_failed_detail", title, error("", p.error));
          view.tip = translate("ev_notify_failed_detail", p.title || "", p.error || "");
        }
        break;
      case "plan_change":
        view.detail = p.from + " → " + p.to;
        break;
      case "codex_reset":
        view.detail = translate("ev_codex_sent", p.count);
        break;
      case "reset": case "exhausted":
        var win = eventWindow(p), left = pct(Number(p.remaining)), at = parseTime(p.reset);
        view.detail = !at ? translate("ev_window_left", win.label, left)
          : translate(ev.kind === "reset" ? "ev_window_next" : "ev_window_due", win.label, left, fmtDateTime(at));
        break;
      case "credential_invalid":
        if (p.reason === "suspended" || p.reason === "rate_limited") view.badge = translate("ev_credential_" + p.reason);
        var codes = [p.status ? "HTTP " + p.status : "", p.code].filter(Boolean).join(" · ");
        var line = error(p.summary, p.error || codes);
        // A problem found by ignition, or with its pause in the row, says
        // that ignition is paused, until when for a rate limit.
        var stops = paused || p.source === "ignite";
        var until = parseTime(p.until) || parseTime(paused && (paused.params || {}).until);
        if (stops) view.detail = until ? translate("ev_ignite_paused_until", line, fmtDateTime(until)) : translate("ev_ignite_paused_note", line);
        else view.detail = p.reason === "rate_limited" && until ? translate("ev_limited_until", line, fmtDateTime(until)) : line;
        // The tooltip adds the full error of the pause in the row.
        view.tip = [[codes, p.summary].filter(Boolean).join(" · ") || line, p.appeal_url || "", paused && (paused.params || {}).error || ""]
          .filter(Boolean).join("\n");
        if (/^https?:\/\//i.test(p.appeal_url || "")) view.link = p.appeal_url;
        break;
      case "credential_restored":
        view.detail = p.source === "ignite" || p.source === "query" ? translate("ev_source_" + p.source) : "";
        break;
      case "error":
        view.detail = errorSummary(ev.detail || ev.message || "");
        view.tip = ev.detail || ev.message || "";
        break;
      default:
        view.detail = ev.detail || ev.message || "";
    }
    return view;
  }

  // The label and detail of an event, for the chart's tooltips.
  function eventParts(ev) {
    var view = eventView(ev);
    return { label: eventTitle(ev, view.label) || "—", detail: view.detail };
  }

  // The label and detail of an event as nodes, with the account names
  // marked SENSITIVE by what the fields mean: the label names the quota
  // group of an account, so it is sensitive as a whole, and so is its text
  // within the detail, such as in a notification title, and an error text
  // from the provider.
  function eventMasked(ev, view) {
    view = view || eventView(ev);
    var label = view.label ? [el("span", { class: SENSITIVE, text: eventTitle(ev, view.label) })] : [document.createTextNode("—")];
    var detail = view.whole ? [el("span", { class: SENSITIVE, text: view.detail })] : maskedNodes(view.detail, [view.label].concat(view.secrets));
    return { label: label, detail: detail };
  }

  // A mark after the detail for the notifications of an event: sent, or
  // failed with the errors in its tooltip.
  function pushMarks(pushes) {
    var marks = [];
    var sent = pushes.filter(function (ev) { return ev.kind === "notify"; });
    var failed = pushes.filter(function (ev) { return ev.kind === "notify_failed"; });
    if (sent.length) {
      marks.push(el("span", { class: "event-mark", title: masked ? null : sent.map(function (ev) { return (ev.params || {}).title || ev.detail || ""; }).join("\n"),
        text: translate("ev_notify") }));
    }
    if (failed.length) {
      marks.push(el("span", { class: "event-mark failed", title: masked ? null : failed.map(function (ev) {
        var p = ev.params || {};
        return p.title ? translate("ev_notify_failed_detail", p.title, p.error || "") : ev.detail || ev.message || "";
      }).join("\n"), text: translate("ev_notify_failed") }));
    }
    return marks;
  }

  function renderEvents() {
    var list = $("events");
    clear(list);
    var more = $("events-more");
    var filter = $("events-5h"), filterText = $("events-5h-label");
    var built = eventRows(status.events || [], showFiveHourResets);
    if (filter) {
      filter.checked = showFiveHourResets;
      filterText.textContent = translate(built.fiveHour ? "ev_show_5h_count" : "ev_show_5h", built.fiveHour);
    }
    if (!built.rows.length) {
      list.appendChild(el("p", { class: "empty", text: translate("no_events") }));
      more.hidden = true;
      return;
    }
    var total = Math.min(built.rows.length, EVENTS_MAX);
    built.rows.slice(0, eventsExpanded ? EVENTS_MAX : EVENTS_COLLAPSED).forEach(function (row) {
      var ev = row.event, view = eventView(ev, row.notifyKind, row.paused), nodes = eventMasked(ev, view);
      var cell = [el("span", { class: "event-text", title: masked ? null : view.tip || view.detail || null }, nodes.detail)];
      if (view.link) cell.push(outsideLink({ class: "event-link", text: translate("ev_appeal") }, view.link, translate("link_appeal")));
      list.appendChild(el("div", { class: "event-row", role: "row" }, [
        el("span", { class: "event-time", role: "cell", text: fmtDateTime(parseTime(ev.time)) }),
        el("span", { class: "event-type", role: "cell" }, [badge(view.style, view.badge)]),
        el("span", { class: "event-label", role: "cell" }, nodes.label),
        el("span", { class: "event-detail", role: "cell" }, cell.concat(pushMarks(row.pushes)))
      ]));
    });
    more.hidden = total <= EVENTS_COLLAPSED;
    more.textContent = eventsExpanded ? translate("collapse") : translate("show_more", total);
  }

  // The provider settings, built when the provider list changes: a monitor
  // switch per provider under Services, with the quota groups to show (see
  // renderShownGroups), and a row per provider in the ignition table. Providers that cannot be ignited show no ignition switch
  // or model; those with quota groups to choose get a row of group
  // checkboxes.
  function renderServiceRows() {
    var body = $("ignition-rows"), monitors = $("monitor-rows");
    var list = providerList();
    var ids = list.map(function (p) { return p.id; }).join(",");
    if (body.getAttribute("data-ids") === ids) return false;
    clear(body);
    clear(monitors);
    list.forEach(function (p) {
      monitors.appendChild(el("div", { class: "service-monitor" }, [
        el("label", { class: "inline" }, [
          el("input", { name: "providers." + p.id + ".monitor", type: "checkbox", "data-service": p.id, "data-label": "monitor_service" }),
          el("span", { text: p.title })
        ]),
        el("div", { class: "shown-groups", "data-shown-groups": p.id, hidden: true }, [
          el("span", { class: "field-label", "data-i18n": "shown_groups", text: translate("shown_groups") }),
          el("div", { class: "checks" })
        ])
      ]));
      var cells = [el("td", { class: "strong", text: p.title })];
      if (p.ignite) {
        cells.push(el("td", {}, [el("input", { name: "providers." + p.id + ".ignite", type: "checkbox", "data-service": p.id, "data-label": "ignite_service" })]));
        cells.push(el("td", {}, [el("input", { name: "providers." + p.id + ".model", type: "text", "data-service": p.id })]));
      } else {
        cells.push(el("td", { class: "muted", text: "—" }));
        cells.push(el("td", { class: "muted", "data-i18n": "no_ignition", text: translate("no_ignition") }));
      }
      body.appendChild(el("tr", {}, cells));
      if (p.groups && p.groups.length) {
        body.appendChild(el("tr", { class: "sub-row" }, [el("td"), el("td", { colspan: "2" }, [
          el("span", { class: "field-label", "data-groups-label": p.id }),
          el("div", { class: "checks", "data-groups": p.id }),
          el("span", { class: "hint", "data-groups-hint": p.id })
        ])]));
      }
    });
    body.setAttribute("data-ids", ids);
    return true;
  }

  // Labels of the service rows in the current language. The model inputs
  // show, after the automatic rule, the model the next ignition starts with.
  function renderServiceText() {
    var next = status.next_models || {};
    var form = $("settings");
    providerList().forEach(function (p) {
      Array.prototype.forEach.call(form.querySelectorAll("[data-service='" + p.id + "'][data-label]"), function (input) {
        input.setAttribute("aria-label", translate(input.getAttribute("data-label"), p.title));
      });
      var model = form.elements["providers." + p.id + ".model"];
      if (model) {
        var text = translate("model_auto", localized(p.model_hint));
        if (next[p.id]) text += " - " + next[p.id];
        model.setAttribute("placeholder", text);
      }
      var label = form.querySelector("[data-groups-label='" + p.id + "']");
      if (label) label.textContent = translate("ignite_groups", p.title);
      var hint = form.querySelector("[data-groups-hint='" + p.id + "']");
      if (hint) hint.textContent = localized(p.groups_hint);
    });
  }

  // The time zone the page shows times in: the configured one with its
  // offset, or the browser's. A change redraws the chart and the times in
  // the settings from the data the page has.
  function zoneKey(st) {
    return zoneSet(st) ? st.timezone + "|" + st.utc_offset_seconds : "browser";
  }

  var shownZone = null;
  function renderZoneChange() {
    var zone = zoneKey(status), changed = shownZone !== null && zone !== shownZone;
    shownZone = zone;
    if (!changed) return;
    $("timezone-note").textContent = timezoneText();
    if (!history) return;
    if (chartPointer || chartCard.contains(document.activeElement)) chartBehind = true;
    else renderChart();
  }

  // A change of hidden_groups redraws the chart from the loaded history;
  // the cards are drawn with every status anyway.
  var shownHidden = null;
  function renderHiddenChange() {
    var key = Object.keys(hiddenGroups()).sort().join("\n"), changed = shownHidden !== null && key !== shownHidden;
    shownHidden = key;
    if (!changed || !history) return;
    if (chartPointer || chartCard.contains(document.activeElement)) chartBehind = true;
    else renderChart();
  }

  function renderStatus() {
    // Account names wait for the masking of a valid config.
    if (!contentReady) return;
    sortAccounts();
    checkApplied();
    renderHeader();
    renderNotices();
    // Rows built after the first fill need their values.
    if (renderServiceRows() && settingsFilled) fillSettings({ providers: true, hidden_groups: true });
    // Accounts can report quota groups the checkboxes do not list yet.
    if (settingsFilled) renderAllShownGroups(effectiveConfig(), document.activeElement);
    renderServiceText();
    // Before the first load starts, start() loads the chosen window.
    if (renderWindowButtons() && (history || historyLoading)) loadHistory();
    renderAccounts();
    renderTargets();
    renderEvents();
    renderLanguageNote();
    renderHiddenChange();
    renderMcp();
    renderZoneChange();
  }

  function fetchStatus(timeout, signal) {
    return api("GET", API + "/status", undefined, timeout, signal).then(function (data) {
      if (Array.isArray(data)) throw new Error(translate("bad_response"));
      return data;
    });
  }

  function useStatus(data) {
    status = data;
    renderStatus();
    reportLanguage();
  }

  function loadStatus(timeout) {
    return fetchStatus(timeout).then(useStatus);
  }

  function runAction(button, path, body, success) {
    button.disabled = true;
    return api("POST", path, body).then(function (data) {
      if (data.status) { status = data.status; renderStatus(); }
      toast(data.ok ? success : translate("failed", data.error));
      loadHistory(true);
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("failed", err.message));
    }).then(function () { button.disabled = false; });
  }

  // Account names are saved in account_names of the config, keyed by the
  // account's pin (auth_index), since Magpie has no API for plugins to rename
  // an account. An empty name removes the entry, and the account shows its
  // Magpie label again. The account is then refreshed so that the status
  // shows the new name at once. The quota query of the refresh can fail for
  // reasons unrelated to the name, so its result only updates the page.
  function saveAccountName(account, name) {
    var latest = rawConfig.account_names;
    var names = latest && typeof latest === "object" ? JSON.parse(JSON.stringify(latest)) : {};
    if (name) names[account.auth_index] = name; else delete names[account.auth_index];
    return patchConfig({ account_names: Object.keys(names).length ? names : null }).then(rereadConfig).then(function () {
      (status.accounts || []).forEach(function (a) { if (a.auth_index === account.auth_index) a.note = name; });
      return api("POST", API + "/refresh", { auth_index: account.auth_index }).then(function (data) {
        if (data.status) { status = data.status; renderStatus(); }
      }, function () { /* the name shows after the next poll */ }).then(function () { loadHistory(true); });
    });
  }

  function fmtWeight(weight) { return String(Math.round(weight * 100) / 100); }

  function accountFieldHost(input) { return saveHost(input); }

  // Account fields have no config input name: names and weights patch the
  // shared maps account_names and account_weights, built from the preceding
  // queued save.
  // Dirty or pending fields survive polling, sorting and earlier saves.
  // Each field owns its result; the heading summarizes failures and progress.
  function saveAccountField(input, row) {
    var host = accountFieldHost(input), binding = saveEntry(host, input);
    var account = row.account, kind = input.getAttribute("data-account-field"), weight = kind === "weight";
    var answer = weight ? input.value.trim() : input.value.replace(/\s+/g, " ").trim();
    var value = Number(answer), gen = input.accountEdit || 0;
    if (weight && (input.validity.badInput || (answer !== "" && (!isFinite(value) || value <= 0)))) {
      setSaveState(host, "failed", translate("weight_invalid"), input);
      return;
    }
    if (answer === input.accountSaved && !input.accountPending && !input.hasAttribute("aria-invalid")) {
      input.value = answer;
      input.accountDirty = false;
      return;
    }
    var say = claimState(host, input);
    input.accountPending = (input.accountPending || 0) + 1;
    saveQueue = saveQueue.then(function () {
      if (!weight) return saveAccountName(account, answer).then(function () { say("saved", translate("save_saved")); });
      var latest = rawConfig.account_weights;
      var weights = latest && typeof latest === "object" ? JSON.parse(JSON.stringify(latest)) : {};
      if (answer === "") delete weights[account.name]; else weights[account.name] = value;
      return patchConfig({ account_weights: Object.keys(weights).length ? weights : null }).then(function (token) {
        showApplied(say, token);
        return rereadConfig();
      });
    }).then(function () {
      input = binding.input;
      if ((input.accountEdit || 0) === gen) {
        input.value = weight && answer !== "" ? fmtWeight(value) : answer;
        input.accountSaved = input.value;
        input.accountDirty = false;
      }
    }).catch(function (err) {
      input = binding.input;
      if ((input.accountEdit || 0) === gen) input.accountDirty = true;
      say("failed", err.message);
    }).then(function () {
      input = binding.input;
      input.accountPending--;
      renderAccounts();
      if (history) renderChart();
    });
  }

  // ---- Chart ----

  // Time ranges follow the quota window, in hours for the 5-hour quota and
  // days for the 7-day quota: one unit; half of a window plus one (3h, 4d);
  // a window plus one (6h, 8d); half a day or month, which fits two whole
  // windows (12h, 15d); a day or a month; five windows plus one (26h, 36d).
  // Keys are the history endpoint's range values.
  // Each entry is the range value, the count and the unit of its label.
  var RANGES = {
    "5h": [["1h", 1, "h"], ["3h", 3, "h"], ["6h", 6, "h"], ["12h", 12, "h"], ["24h", 24, "h"], ["26h", 26, "h"]],
    "7d": [["24h", 1, "d"], ["4d", 4, "d"], ["8d", 8, "d"], ["15d", 15, "d"], ["1mo", 1, "mo"], ["36d", 36, "d"]]
  };
  // The default ranges are the shortest that cover one whole window.
  var DEFAULT_RANGE = { "5h": "6h", "7d": "8d" };
  // From a window plus one on, the axis ends at the reset of the selected
  // row, so the rest of its window lies ahead of now. "24h" extends as a
  // range of the 5-hour window, not as the 1d range of the 7-day window.
  function extendsAhead(kind, name) {
    return (RANGES[kind] || []).some(function (r, i) { return i >= 2 && r[0] === name; });
  }
  // The default row order is the status API's group_order, by quota name;
  // other quotas follow in API order. The status API orders the quota groups
  // of an account the same way.
  var LANE = 22, LANE_GAP = 6, AXIS_H = 22, EVENT_H = 24;
  // A rise of this many points between two samples is a reset.
  var RESET_JUMP = 5;
  // Quotas report whole percents; steps this small are a steady decline.
  var QUANTUM = 2;
  // Passive samples are written up to a minute after the value changes. A
  // passive value unchanged for longer than this is held until now when the
  // rate of use is estimated, so a pause shows as one. Active samples come
  // once per poll, so an active value is held only after one and a half
  // sample steps; holding it sooner would level the projection in the
  // second half of every poll interval.
  var HOLD_AFTER = 120;
  // The width of a character of 11px text in the monospace chart font, and
  // the spacing of the values beside the detail chart.
  var CHAR_W = 6.6, LABEL_H = 16;

  // The geometry of the overview drawn last, shared by the detail chart and
  // hovering. The chart_view the page keeps is chartView.
  var chartGeom = null;
  var hoverSource = "";
  var hoverListeners = [];
  // Samples of the row in the detail chart; hovering either chart snaps to them.
  var snapPoints = [];

  // The value of a color variable of page.css for an SVG attribute. Many
  // derive from Magpie's tokens with color-mix(), which is resolved to the
  // color it gives through a hidden element, so the chart and its
  // screenshots carry plain colors. The element sits in a parent of a
  // color no theme uses (PROBE_SENTINEL): a value the browser cannot parse
  // leaves the element that inherited color, which counts as a failure,
  // and the variable's <name>-static value of page.css is used instead.
  // applyTheme clears the cache.
  var PROBE_SENTINEL = "rgb(1, 2, 3)";
  var colorCache = {};
  function cssVar(name) {
    var root = getComputedStyle(document.documentElement);
    var value = root.getPropertyValue(name).trim();
    if (value.indexOf("(") < 0 || /^(rgba?|hsla?)\(/.test(value)) return value;
    if (!Object.prototype.hasOwnProperty.call(colorCache, value)) colorCache[value] = resolveColor(value);
    if (colorCache[value]) return colorCache[value];
    var fallback = root.getPropertyValue(name + "-static").trim();
    return fallback || root.getPropertyValue("--fg").trim() || "currentColor";
  }

  // The color value gives, or "" where the browser does not take it.
  function resolveColor(value) {
    var probe = el("i", {});
    probe.style.color = value;
    if (!probe.style.color) return "";
    var host = el("div", { hidden: true }, [probe]);
    host.style.color = PROBE_SENTINEL;
    document.body.appendChild(host);
    var color = getComputedStyle(probe).color;
    document.body.removeChild(host);
    return color && color !== PROBE_SENTINEL ? color : "";
  }

  var LEVEL_VARS = { empty: "--quota-empty-color", low: "--quota-low-color", medium: "--quota-medium-color", high: "--quota-high-color", full: "--quota-full-color" };

  // A rect in the color of a level; an empty one is also striped.
  function levelRect(root, attrs, level) {
    var fill = {};
    Object.keys(attrs).forEach(function (name) { fill[name] = attrs[name]; });
    fill.fill = cssVar(LEVEL_VARS[level]);
    root.appendChild(svg("rect", fill));
    if (level !== "empty") return;
    var stripes = {};
    Object.keys(attrs).forEach(function (name) { if (name !== "fill-opacity") stripes[name] = attrs[name]; });
    stripes.fill = "url(#chart-hatch-empty)";
    root.appendChild(svg("rect", stripes));
  }

  function pct(v) { return v == null ? "—" : Math.round(v) + "%"; }

  // name cut with an ellipsis to fit room, as width measures text. A name
  // that starts with the service (prefix, "Antigravity · ") takes its short
  // form (short, "AG · ") instead, and then loses characters at the end of
  // the group label: the service always stays in front, so "Claude / GPT"
  // of an Antigravity account never reads as a Claude account.
  //
  // Group labels have no length limit (Antigravity keeps an unknown one as
  // it is), so the cut is found by binary search: a label of n characters
  // takes about log2(n) measurements, not n. The search takes a longer
  // prefix to be at least as wide as a shorter one, as it is in real fonts.
  // Cuts fall between graphemes, so a letter with its accent or an emoji of
  // several code points stays whole.
  function fitName(name, prefix, room, short, width) {
    if (width(name) <= room) return name;
    var keep = 1;
    if (prefix && short && name.indexOf(prefix) === 0) {
      name = short + name.slice(prefix.length);
      if (width(name) <= room) return name;
      keep = graphemes(short).length + 1;
    }
    var chars = graphemes(name);
    var cut = function (n) { return chars.slice(0, n).join("") + "…"; };
    // The longest n in [keep, length - 1] whose cut fits; keep characters
    // stay even when they do not.
    var lo = keep, hi = Math.max(keep, chars.length - 1);
    while (lo < hi) {
      var mid = Math.ceil((lo + hi) / 2);
      if (width(cut(mid)) <= room) lo = mid;
      else hi = mid - 1;
    }
    // The candidate is measured once more to confirm that it fits, and
    // shortened if it does not.
    while (lo > keep && width(cut(lo)) > room) lo--;
    return cut(lo);
  }

  // The graphemes of text, as the reader sees its characters: by
  // Intl.Segmenter, or by code points where the browser has none.
  var graphemeSegmenter;
  function graphemes(text) {
    if (graphemeSegmenter === undefined) {
      try {
        graphemeSegmenter = typeof Intl !== "undefined" && Intl.Segmenter ? new Intl.Segmenter(undefined, { granularity: "grapheme" }) : null;
      } catch (err) { graphemeSegmenter = null; }
    }
    if (!graphemeSegmenter) return Array.from(text);
    return Array.from(graphemeSegmenter.segment(text), function (part) { return part.segment; });
  }

  // A function (text, size, weight) giving the width of text in the body
  // font, in the chart's units: measured on a canvas, or estimated per
  // character where there is none. The font family is read once, when the
  // function is made for a redraw; the canvas font is set only when the
  // size or weight changes.
  var measureContext = null;
  function textMeasure() {
    if (measureContext === null) {
      try { measureContext = document.createElement("canvas").getContext("2d") || false; } catch (err) { measureContext = false; }
    }
    var context = measureContext;
    if (!context) return function (text, size) { return Array.from(text).length * size * 0.54; };
    var family = getComputedStyle(document.body).fontFamily, font = "";
    return function (text, size, weight) {
      var want = weight + " " + size + "px " + family;
      if (want !== font) context.font = font = want;
      return context.measureText(text).width;
    };
  }

  function fmtDiff(d) { return d == null ? "—" : (d > 0 ? "+" : d < 0 ? "−" : "") + Math.abs(d) + "%"; }

  function unixDate(t) { return new Date(t * 1000); }

  // "5h", "7d" or empty for a window of another length, which the chart
  // does not show.
  function windowKind(series) { return series.kind || ""; }

  // The chart offers the window kinds the accounts report, and those whose
  // loaded history still has a sample in the range of an inactive window,
  // leaving out hidden quota groups; both while none are known, and none
  // while all that are known belong to hidden groups.
  function windowKinds() {
    var seen = {}, known = false, hidden = hiddenGroups();
    (status.accounts || []).forEach(function (a) {
      (a.groups || []).forEach(function (g) {
        (g.windows || []).forEach(function (w) {
          if (!w.kind) return;
          known = true;
          if (!groupHidden(hidden, a.provider, g.source_label)) seen[w.kind] = true;
        });
      });
    });
    ["5h", "7d"].forEach(function (kind) {
      var cached = historyCache[historyKey(kind, kind === chartWindow ? range : rangeFor(kind))];
      if (seen[kind] || !cached) return;
      if (recentSamples(cached.data, kind, hidden)) seen[kind] = true;
      else if (recentSamples(cached.data, kind, {})) known = true;
    });
    var kinds = ["5h", "7d"].filter(function (k) { return seen[k]; });
    return kinds.length || known ? kinds : ["5h", "7d"];
  }

  // Whether data has a sample in its range of a window of kind, leaving out
  // the quota groups of hidden.
  function recentSamples(data, kind, hidden) {
    var from = new Date(data.from).getTime() / 1000;
    return (data.series || []).some(function (s) {
      return windowKind(s) === kind && !groupHidden(hidden, s.provider, s.source_label) && s.points && s.points.length && s.points[s.points.length - 1][0] >= from;
    });
  }

  // Shows the window buttons of the reported kinds. When the chosen kind is
  // not among them, it switches to the first one and returns true; without
  // any, it keeps the chosen kind, whose chart has no rows.
  function renderWindowButtons() {
    var kinds = windowKinds();
    Array.prototype.forEach.call($("window-buttons").querySelectorAll("button"), function (b) {
      b.hidden = kinds.indexOf(b.getAttribute("data-value")) < 0;
    });
    if (!kinds.length || kinds.indexOf(chartWindow) >= 0) return false;
    chartWindow = kinds[0];
    range = rangeFor(chartWindow);
    pressSegment("window-buttons", chartWindow);
    renderRangeButtons();
    return true;
  }

  function valueAt(points, t) {
    var value = null;
    for (var i = 0; i < points.length; i++) {
      if (points[i][0] <= t) value = points[i];
      else break;
    }
    return value;
  }

  // Samples further apart than a few poll intervals leave a gap, which is
  // drawn as missing data rather than as a held value. Active queries are
  // skipped for up to passive_skip_max_minutes while response headers keep
  // the main windows fresh; windows without header data, such as Fable, then
  // wait that long plus one poll, which is not a gap.
  function gapLimit() {
    var cfg = status && status.config ? status.config : {};
    var poll = Number(cfg.poll_interval_seconds) || 300;
    var limit = sampleStep() * 3.5;
    if (Number(cfg.passive_skip_seconds) > 0) {
      limit = Math.max(limit, (Number(cfg.passive_skip_max_minutes) || 0) * 60 + poll * 1.5);
    }
    return limit;
  }

  // The usual time between two samples: the poll interval, or the bucket of
  // a long range.
  function sampleStep() {
    var cfg = status && status.config ? status.config : {};
    return Math.max(history.bucket_seconds || 0, Number(cfg.poll_interval_seconds) || 300);
  }

  // The weight of an account in a pooled row: the weight set on its card, or
  // the default of its plan.
  function accountWeight(account) {
    var saved = rawConfig.account_weights;
    var weight = saved && typeof saved === "object" ? Number(saved[account.name]) : NaN;
    if (isFinite(weight) && weight > 0) return weight;
    weight = Number(account.default_weight);
    return isFinite(weight) && weight > 0 ? weight : 1;
  }

  // Pooled remaining of several accounts: the mean percentage weighted by
  // the accounts' weights, which is the share of the total quota left when
  // the weights match the sizes of the plans. Each point also carries the
  // lowest and highest account for the range band, and 1 when an account's
  // share is estimated (gapValue). key is "points" or "floor".
  //
  // An account without a recent sample keeps its share as estimated, or the
  // total would jump to the mean of the others, such as to 100% for a pair
  // whose other account is full. Only accounts with no sample yet are left
  // out. The total has points only at the samples of its accounts, so it
  // covers only the times some account has data for; a reset within an
  // account's gap shows at the next sample of another account.
  function pooled(rows, limit, key) {
    var times = [];
    rows.forEach(function (r) { r[key].forEach(function (p) { times.push(p[0]); }); });
    times.sort(function (a, b) { return a - b; });
    var out = [];
    times.forEach(function (t, i) {
      if (i && t === times[i - 1]) return;
      var sum = 0, total = 0, lo = 101, hi = -1, estimated = 0, fresh = false;
      rows.forEach(function (r) {
        var v = gapValue(r[key], r.resets, t, limit);
        if (!v) return;
        if (v.estimated) estimated = 1;
        else fresh = true;
        sum += r.weight * v.value; total += r.weight; lo = Math.min(lo, v.value); hi = Math.max(hi, v.value);
      });
      // With every account estimated there is no data at all: a gap.
      if (fresh) out.push([t, sum / total, 0, lo, hi, estimated]);
    });
    return out;
  }

  // The reset an account's window was due at as of time t, from the resets
  // of its history ([sample time, reset]); 0 when unknown.
  function resetAsOf(resets, t) {
    var at = 0;
    (resets || []).forEach(function (r) { if (r[0] <= t) at = r[1]; });
    return at;
  }

  // The value of one account at time t for a pooled total: its last sample
  // when recent, otherwise an estimate { value, estimated: true }; null
  // before its first sample.
  //
  // Within one cycle the remaining only falls, so across a gap between two
  // samples it lies between them: the estimate runs straight from one to
  // the other, never above the first. A response header reads a point above
  // the usage API, which is not a rise. A gap that spans the window's reset
  // keeps the last value until the reset, then runs from 100% to the next
  // sample. After the last sample the last value holds until the reset,
  // then 100%.
  function gapValue(points, resets, t, limit) {
    var p = valueAt(points, t);
    if (!p) return null;
    if (t - p[0] <= limit) return { value: p[1], estimated: false };
    var next = null;
    for (var i = 0; i < points.length; i++) {
      if (points[i][0] > t) { next = points[i]; break; }
    }
    var reset = resetAsOf(resets, p[0]);
    var resetIn = reset > p[0] && (!next || reset <= next[0]);
    var value;
    if (next && !resetIn && next[1] - p[1] < RESET_JUMP) {
      value = Math.min(p[1], p[1] + (next[1] - p[1]) * (t - p[0]) / (next[0] - p[0]));
    } else if (resetIn && t >= reset) {
      value = next ? 100 + (next[1] - 100) * (t - reset) / Math.max(1, next[0] - reset) : 100;
    } else {
      value = p[1];
    }
    return { value: value, estimated: true };
  }


  // The lowest remaining so far in each cycle. Response headers report a
  // point more than active queries (51 against 50), which near a threshold
  // flips the band's color for a moment; a rise by a reset starts a new
  // cycle. Unlike declining(), which averages 50 and 51 to 50.5, this keeps
  // the lower reading.
  function cycleFloor(points) {
    var low = null;
    return points.map(function (p) {
      low = low == null || p[1] - low >= RESET_JUMP ? p[1] : Math.min(low, p[1]);
      var q = p.slice();
      q[1] = low;
      return q;
    });
  }

  // One row per quota group label. A label with several accounts becomes a
  // pooled row that expands into one row per account. floor holds the
  // samples as cycleFloor() leaves them, for the band and the rate of use.
  //
  // A window shows while the status still reports it, or while it has a
  // sample in the chosen range. A window that only has samples is inactive,
  // such as the 5-hour window of an account whose plan no longer has one:
  // it gets a row of its own after the label's row, is left out of the
  // total and of "lowest first", and shows no projection or even pace.
  //
  // Rows follow the order of the cards: a label takes the place of its
  // first account, and labels of the same account keep the group_order of
  // the status API, so Gemini stays before Claude / GPT within Antigravity.
  //
  // Quota groups of hiddenGroups get no rows, so they count in no total
  // and not in "lowest first". A series without a current account is left
  // out too when the history still knows its provider and source label.
  function chartRows() {
    var info = {}, accountOf = {}, emailOf = {}, current = [], shared = sharedGroups(status.accounts), hidden = hiddenGroups();
    status.accounts.forEach(function (a, index) {
      accountOf["auth|" + a.auth_index] = index;
      emailOf[a.auth_index] = a.email || a.name;
      (a.groups || []).forEach(function (g) {
        accountOf["group|" + g.key] = index;
        (g.windows || []).forEach(function (w) {
          var reset = parseTime(w.reset);
          // A reported window without a reset has reset 0; see paceSegments.
          info[g.key + "|" + w.id] = { email: a.email || a.name, reset: reset ? reset.getTime() / 1000 : 0, weight: accountWeight(a), remaining: w.remaining };
          if (w.kind === historyWindow) current.push({ group: g.key, window: w.id, window_label: w.short, kind: w.kind, label: g.label,
            source_label: g.source_label, provider: a.provider, auth_index: a.auth_index, points: [], resets: [] });
        });
      });
    });
    var limit = gapLimit();
    var from = new Date(history.from).getTime() / 1000;
    var byLabel = {}, order = [], seen = {};
    // A window the status reports without samples in the history, such as
    // one not queried within the range, still gets its row, with the current
    // reading and no data on the time axis.
    var series = history.series.filter(function (s) { return s.points && s.points.length; });
    series.forEach(function (s) { seen[s.group + "|" + s.window] = true; });
    history.series.forEach(function (s) { if (!seen[s.group + "|" + s.window]) seen[s.group + "|" + s.window] = s; });
    series = series.concat(current.filter(function (s) { return seen[s.group + "|" + s.window] !== true; }).map(function (s) {
      var empty = seen[s.group + "|" + s.window];
      if (empty && Array.isArray(empty.resets)) s.resets = empty.resets;
      return s;
    }));
    series.forEach(function (s, seq) {
      if (windowKind(s) !== historyWindow || groupHidden(hidden, s.provider, s.source_label)) return;
      var meta = info[s.group + "|" + s.window];
      var lastPoint = s.points[s.points.length - 1];
      // Only the sample before the range can lie before it, so the last
      // sample tells whether any falls in the range.
      if (!meta && lastPoint[0] < from) return;
      // A window with its own label, such as "7 Day Opus" next to the
      // 7-day window of the same group label, gets a row of its own.
      var extra = s.window_label && s.window_label !== historyWindow ? " · " + s.window_label : "";
      var source = (s.source_label || s.label) + extra;
      var id = s.provider + "|" + source;
      var account = accountOf["group|" + s.group];
      if (account === undefined) account = accountOf["auth|" + s.auth_index];
      if (!byLabel[id]) { byLabel[id] = []; order.push(id); }
      var title = groupName(shared[s.group], s.label), total = groupName(shared[s.group], s.source_label || s.label);
      byLabel[id].push({ id: s.group + "|" + s.window, name: title.name + extra, prefix: title.prefix, short: title.short, source: source,
        total: { name: total.name + extra, prefix: total.prefix, short: total.short },
        sub: meta ? meta.email : emailOf[s.auth_index] || s.auth_index || "", group: s.group, inactive: !meta, lastAt: lastPoint ? lastPoint[0] : null,
        points: s.points, floor: cycleFloor(s.points), resets: Array.isArray(s.resets) ? s.resets : [],
        reset: meta ? meta.reset : null, weight: meta ? meta.weight : 1, last: lastPoint ? lastPoint[1] : meta.remaining,
        account: account === undefined ? status.accounts.length + seq : account, seq: seq,
        // No current account: the name, often the internal group key, is
        // sensitive as a whole.
        orphan: account === undefined });
    });
    var lowFirst = chartSort === "low";
    var groupOrder = status.group_order || [];
    var byAccount = function (a, b) { return (a.account - b.account) || (a.seq - b.seq); };
    var byLast = function (a, b) { return (a.last - b.last) || byAccount(a, b); };
    var units = order.map(function (id, index) {
      var kids = byLabel[id];
      var live = kids.filter(function (k) { return !k.inactive; }).sort(lowFirst ? byLast : byAccount);
      var gone = kids.filter(function (k) { return k.inactive; }).sort(byAccount);
      var rank = groupOrder.indexOf(kids[0].source);
      if (rank < 0) rank = groupOrder.length + index;
      var first = (live.length ? live : gone).reduce(function (min, k) { return Math.min(min, k.account); }, Infinity);
      var main = live.length === 1 ? live[0] : live.length ? pooledRow(id, live, limit) : null;
      return { main: main, gone: gone, account: first, rank: rank };
    });
    units.sort(function (a, b) {
      if (lowFirst && (a.main || b.main)) {
        if (!a.main || !b.main) return a.main ? -1 : 1;
        if (a.main.last !== b.main.last) return a.main.last - b.main.last;
      }
      return (a.account - b.account) || (a.rank - b.rank);
    });
    var rows = [];
    units.forEach(function (u) {
      if (u.main) rows.push(u.main);
      rows.push.apply(rows, u.gone);
    });
    return rows;
  }

  // The total of the active accounts of a label.
  function pooledRow(id, kids, limit) {
    // The next reset of the accounts; a reset that has passed, of a window
    // not opened again or not queried since, is not the next one.
    var now = Date.now() / 1000;
    var resets = kids.map(function (k) { return k.reset; }).filter(function (t) { return t > now; });
    var weights = kids.reduce(function (sum, k) { return sum + k.weight; }, 0);
    // The title of the total, as groupName gives it for the label the
    // accounts share; an account that names its service decides.
    var title = (kids.filter(function (k) { return k.total.prefix; })[0] || kids[0]).total;
    return { id: "group:" + id, name: title.name, prefix: title.prefix, short: title.short, children: kids, points: pooled(kids, limit, "points"), floor: pooled(kids, limit, "floor"),
      last: kids.reduce(function (sum, k) { return sum + k.weight * k.last; }, 0) / weights,
      reset: resets.length ? Math.min.apply(null, resets) : null, groups: kids.map(function (k) { return k.group; }) };
  }

  function findRow(rows, id) {
    var hit = null;
    rows.forEach(function (r) {
      if (r.id === id) hit = r;
      (r.children || []).forEach(function (c) { if (c.id === id) hit = c; });
    });
    return hit;
  }

  // The right end of the time axis: for a range that extends ahead, the
  // reset of the selected row's window (the earliest account's for a pooled
  // row), otherwise now. Without a reset still to come, such as a 5-hour
  // window no request has opened yet, it is now.
  function axisEnd(row) {
    var from = new Date(history.from).getTime() / 1000, now = new Date(history.to).getTime() / 1000;
    if (!extendsAhead(historyWindow, historyRange) || !row || !row.reset || row.reset <= now || row.reset - now >= now - from) return now;
    return row.reset;
  }

  // The axis spans the chosen range and ends at end. The history covers the
  // range up to now, so an axis that ends later leaves out its oldest part.
  function geometry(width, end) {
    var now = new Date(history.to).getTime() / 1000;
    var to = end || now, from = to - (now - new Date(history.from).getTime() / 1000);
    var labelW = width < 640 ? 132 : 190, rightW = width < 640 ? 112 : 136;
    var plotW = width - labelW - rightW;
    return { from: from, to: to, now: now, width: width, labelW: labelW, rightW: rightW, plotW: plotW,
      x: function (t) { return labelW + (t - from) / (to - from) * plotW; },
      t: function (px) { return from + (px - labelW) / plotW * (to - from); } };
  }

  // Tick steps of the time axis, in seconds. A step divides a day or is a
  // whole number of days, so ticks fall on the same clock times every day.
  var TICK_STEPS = [600, 1800, 3600, 2 * 3600, 3 * 3600, 4 * 3600, 6 * 3600, 12 * 3600, 86400, 2 * 86400, 5 * 86400, 7 * 86400, 14 * 86400];
  // Room for a tick label ("15:00" or "11/14", five characters), with
  // space between labels.
  var TICK_LABEL_W = 5 * CHAR_W + 10;

  // The ticks of the time axis: the step the range calls for, or a longer
  // one where the labels would overlap in a narrow chart.
  function chartTicks(g) {
    var span = g.to - g.from;
    var step = span <= 3600 ? 600 : span <= 3 * 3600 ? 1800 : span <= 6 * 3600 ? 3600 : span <= 12 * 3600 ? 2 * 3600 : span <= 2 * 86400 ? 4 * 3600 : span <= 8 * 86400 ? 86400 : span <= 15 * 86400 ? 2 * 86400 : 5 * 86400;
    for (var i = 0; i < TICK_STEPS.length && (TICK_STEPS[i] < step || g.plotW * TICK_STEPS[i] / span < TICK_LABEL_W); i++) {}
    if (i < TICK_STEPS.length) step = TICK_STEPS[i];
    var offset = offsetSeconds(), list = [];
    for (var t = Math.ceil((g.from + offset) / step) * step - offset; t <= g.to; t += step) list.push(t);
    return { list: list, label: function (t) { return span <= 2 * 86400 ? fmtClock(unixDate(t)) : fmtDay(unixDate(t)); } };
  }

  // Contiguous stretches of samples; each ends at its last sample plus one
  // gap limit, or at "now" when that is sooner.
  function runs(points, limit, end) {
    var out = [], cur = [];
    points.forEach(function (p) {
      if (cur.length && p[0] - cur[cur.length - 1][0] > limit) { out.push(cur); cur = []; }
      cur.push(p);
    });
    if (cur.length) out.push(cur);
    return out.map(function (r, i) {
      var last = r[r.length - 1][0];
      var stop = i === out.length - 1 ? Math.min(end, last + limit) : last + Math.min(limit, 300);
      return { pts: r, start: r[0][0], end: Math.max(stop, last) };
    });
  }

  function hatchPattern(root) {
    var defs = svg("defs", {});
    var pattern = svg("pattern", { id: "chart-hatch", width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
    pattern.appendChild(svg("rect", { width: 2, height: 6, fill: cssVar("--chart-hatch") }));
    defs.appendChild(pattern);
    var empty = svg("pattern", { id: "chart-hatch-empty", width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
    empty.appendChild(svg("rect", { width: 2.5, height: 6, fill: cssVar("--quota-empty-stripe") }));
    defs.appendChild(empty);
    var blur = svg("filter", { id: "mask-blur", x: "-10%", y: "-60%", width: "120%", height: "220%" });
    blur.appendChild(svg("feGaussianBlur", { stdDeviation: 3.5 }));
    defs.appendChild(blur);
    root.appendChild(defs);
    return defs;
  }

  var SVG_NS = "http://www.w3.org/2000/svg";
  function svg(tag, attrs) {
    var node = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (name) { node.setAttribute(name, attrs[name]); });
    return node;
  }

  function svgText(attrs, text) {
    var node = svg("text", attrs);
    node.textContent = text;
    return node;
  }

  // Hatching from a to b, cut to the axis: missing data, and the time after
  // now, which has none yet.
  function hatchRect(root, g, a, b, y0, height) {
    a = Math.max(a, g.from);
    if (g.x(b) - g.x(a) > 1) root.appendChild(svg("rect", { x: g.x(a), y: y0, width: g.x(b) - g.x(a), height: height, fill: "url(#chart-hatch)" }));
  }

  // The start of the hatching after the last stretch: now, or the end of
  // the data when it stopped before.
  function dataEnd(g, cursor, limit) { return cursor < g.now - limit ? cursor : g.now; }

  // A band of solid color per quota level. Samples merge while the level
  // stays the same, and edges snap to whole pixels so neighbours leave no seam.
  // The band of an inactive window ends at its last sample, and the rest is
  // hatched as missing data.
  function drawBand(root, g, points, y0, limit, inactive) {
    var cursor = g.from;
    runs(points, limit, inactive ? points[points.length - 1][0] : g.now).forEach(function (run) {
      if (run.start > cursor + limit) hatchRect(root, g, cursor, run.start, y0, LANE);
      cursor = run.end;
      var segments = [];
      run.pts.forEach(function (p, i) {
        var end = i + 1 < run.pts.length ? run.pts[i + 1][0] : run.end;
        var level = quotaLevel(p[1]), last = segments[segments.length - 1];
        if (last && last.level === level) last.end = end;
        else segments.push({ start: Math.max(g.from, p[0]), end: end, level: level });
      });
      segments.forEach(function (seg) {
        if (seg.end <= g.from) return;
        var x0 = Math.round(g.x(seg.start)), x1 = Math.max(x0 + 1, Math.round(g.x(seg.end)));
        levelRect(root, { x: x0, y: y0, width: x1 - x0, height: LANE, "fill-opacity": seg.level === "empty" ? 0.95 : 0.8, "shape-rendering": "crispEdges" }, seg.level);
      });
    });
    hatchRect(root, g, inactive ? cursor : dataEnd(g, cursor, limit), g.to, y0, LANE);
  }

  function ignitionEvents(g) {
    return (history.events || []).map(function (ev) {
      var parts = eventParts(ev);
      return { t: new Date(ev.time).getTime() / 1000, failed: ev.kind === "ignite_failed" || ev.kind === "ignite_paused",
        event: ev, group: ev.group };
    }).filter(function (ev) { return ev.t >= g.from; });
  }

  // An ignition in a tooltip: its time, label and detail.
  function noteNodes(item) {
    var nodes = eventMasked(item.event), hasLabel = eventParts(item.event).label !== "—";
    return [document.createTextNode(fmtClock(unixDate(item.t)) + " ")].concat(
      hasLabel ? nodes.label.concat([document.createTextNode(" ")]) : [], nodes.detail);
  }

  function placeTip(tip, box, g, t, top) {
    var scale = box.clientWidth / g.width, left = g.x(t) * scale + 14;
    if (left + 280 > box.clientWidth) left = g.x(t) * scale - 294;
    tip.style.left = Math.max(0, left) + "px";
    tip.style.top = top + "px";
  }

  function tipRow(tip, text, value, strong) {
    tip.appendChild(el("div", { class: "row" + (strong ? " strong" : "") }, [
      el("span", { class: "key" + (value != null ? " fill-" + quotaLevel(value) : "") }),
      el("span", { class: "name" }, maskedNodes(text)),
      el("span", { class: "value", text: value != null ? pct(value) : translate("no_data") })
    ]));
  }

  // Overview: one band per row on a shared time axis, the ignition lane
  // below, and a crosshair shared with the detail chart.
  function renderOverview(rows, box, end) {
    var g = chartGeom = geometry(Math.max(480, box.clientWidth || 800), end);
    var limit = gapLimit();
    var flat = [];
    rows.forEach(function (r) {
      flat.push({ row: r, depth: 0 });
      if (r.children && chartExpanded[r.id]) r.children.forEach(function (c) { flat.push({ row: c, depth: 1 }); });
    });
    var height = AXIS_H + flat.length * (LANE + LANE_GAP) + EVENT_H + 6;
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + height, role: "img", "aria-label": translate("overview_aria") });
    hatchPattern(root);
    var ticks = chartTicks(g);
    ticks.list.forEach(function (t) {
      root.appendChild(svgText({ x: g.x(t), y: 13, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-tertiary") }, ticks.label(t)));
      root.appendChild(svg("line", { x1: g.x(t), x2: g.x(t), y1: AXIS_H - 4, y2: height - 4, stroke: cssVar("--border-color"), "stroke-dasharray": "2 4" }));
    });
    var rowY = {}, textWidth = textMeasure();
    flat.forEach(function (f, i) {
      var r = f.row, y0 = AXIS_H + i * (LANE + LANE_GAP);
      rowY[r.id] = y0;
      var selected = chartSelected === r.id;
      if (selected) root.appendChild(svg("rect", { x: 0, y: y0 - 3, width: g.width, height: LANE + 6, rx: 6, fill: cssVar("--chart-select") }));
      var lx = 4 + f.depth * 18;
      if (r.children) root.appendChild(svgText({ x: lx, y: y0 + 15, "font-size": 10, fill: cssVar("--text-tertiary") }, chartExpanded[r.id] ? "▾" : "▸"));
      var nameX = lx + (r.children ? 14 : 0);
      // Row names are semibold (.row-name in page.css); a pooled row keeps
      // room for its account count after the name.
      var size = f.depth ? 12 : 13, measure = function (text) { return textWidth(text, size, 600); };
      var count = r.children ? "×" + r.children.length : "", countW = count ? textWidth(count, 11, 400) + 6 : 0;
      var room = g.labelW - nameX - 6 - countW;
      var name = f.depth ? fitName(r.sub, "", room, "", measure) : fitName(r.name, r.prefix, room, r.short, measure);
      root.appendChild(svgSensitive(svgText({ x: nameX, y: y0 + 15, "font-size": f.depth ? 12 : 13, "font-weight": f.depth ? 400 : 600,
        fill: cssVar(f.depth ? "--text-secondary" : "--text-primary"), "class": "row-name" }, name), f.depth || r.orphan || hasSecret(f.depth ? r.sub : r.name)));
      if (count) root.appendChild(svgText({ x: nameX + measure(name) + 6, y: y0 + 15, "font-size": 11, fill: cssVar("--text-tertiary") }, count));
      root.appendChild(svg("rect", { x: g.labelW, y: y0, width: g.plotW, height: LANE, rx: 4, fill: cssVar("--chart-lane") }));
      drawBand(root, g, r.floor, y0, limit, r.inactive);
      var rx = g.width - g.rightW + 14;
      if (r.inactive) {
        root.appendChild(svgText({ x: rx, y: y0 + 15, "font-size": 12, fill: cssVar("--text-tertiary"), "class": "row-name" }, translate("inactive")));
      } else {
        levelRect(root, { x: rx, y: y0 + 7, width: 8, height: 8, rx: 2 }, quotaLevel(r.last));
        root.appendChild(svgText({ x: rx + 14, y: y0 + 15, "font-size": 13, "font-weight": 650, fill: cssVar("--text-primary") }, pct(r.last)));
      }
      if (r.reset && !r.inactive) root.appendChild(svgText({ x: g.width - 2, y: y0 + 15, "font-size": 11, "text-anchor": "end", fill: cssVar("--text-tertiary") },
        "↻" + fmtCountdown(r.reset * 1000 - Date.now())));
      var hit = svg("rect", { x: 0, y: y0 - 3, width: g.width, height: LANE + 6, fill: "transparent", style: "cursor:pointer" });
      hit.addEventListener("click", function () {
        if (r.children && chartSelected === r.id) {
          chartExpanded[r.id] = !chartExpanded[r.id];
          saveChartExpanded();
        }
        chartSelected = r.id;
        renderChart();
      });
      hit.addEventListener("pointermove", function (e) { hoverAt(e, root, r.id, "overview"); });
      root.appendChild(hit);
    });

    // Ignitions at the same moment merge into one mark with a count.
    var ey = AXIS_H + flat.length * (LANE + LANE_GAP) + 4, cy = ey + 9;
    root.appendChild(svgText({ x: 4, y: ey + 13, "font-size": 12, fill: cssVar("--text-tertiary"), "class": "row-name" }, translate("ignition_lane")));
    root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: cy, y2: cy, stroke: cssVar("--border-color") }));
    var events = ignitionEvents(g), clusters = [];
    events.slice().sort(function (a, b) { return a.t - b.t; }).forEach(function (ev) {
      var last = clusters[clusters.length - 1];
      if (last && g.x(ev.t) - g.x(last.t) < 26) { last.n++; last.failed = last.failed || ev.failed; }
      else clusters.push({ t: ev.t, n: 1, failed: ev.failed });
    });
    clusters.forEach(function (c) {
      var x = g.x(c.t);
      if (c.n > 1) root.appendChild(svgText({ x: x + 6, y: cy + 4, "font-size": 10, fill: cssVar("--text-tertiary") }, "×" + c.n));
      if (c.failed) root.appendChild(svg("path", { d: "M" + (x - 4) + " " + (cy - 4) + "L" + (x + 4) + " " + (cy + 4) + "M" + (x + 4) + " " + (cy - 4) + "L" + (x - 4) + " " + (cy + 4),
        stroke: cssVar("--viz-failure"), "stroke-width": 2.2, "stroke-linecap": "round" }));
      else root.appendChild(svg("circle", { cx: x, cy: cy, r: 3.5, fill: cssVar("--viz-success"), stroke: cssVar("--bg-primary"), "stroke-width": 1.5 }));
    });
    var failed = events.filter(function (ev) { return ev.failed; }).length;
    root.appendChild(svgText({ x: g.width - 2, y: ey + 13, "font-size": 11, "text-anchor": "end", fill: cssVar(failed ? "--failure-text" : "--text-tertiary") },
      translate("lane_ok", events.length - failed) + (failed ? translate("lane_failed", failed) : "")));
    var eventHit = svg("rect", { x: g.labelW, y: ey - 2, width: g.plotW, height: 22, fill: "transparent" });
    eventHit.addEventListener("pointermove", function (e) { hoverAt(e, root, null, "overview"); });
    root.appendChild(eventHit);

    var cross = svg("line", { y1: AXIS_H - 4, y2: height - 4, stroke: cssVar("--text-tertiary"), "stroke-width": 1, visibility: "hidden", "pointer-events": "none" });
    root.appendChild(cross);
    root.addEventListener("pointerleave", function () { setHover(null); });
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    hoverListeners.push(function (t, rowId) {
      if (t == null) { cross.setAttribute("visibility", "hidden"); tip.hidden = true; return; }
      cross.setAttribute("x1", g.x(t)); cross.setAttribute("x2", g.x(t)); cross.setAttribute("visibility", "visible");
      // After now only the detail chart has values, its projection.
      if (hoverSource !== "overview" || t > g.now) { tip.hidden = true; return; }
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
      function sample(row) {
        var p = valueAt(row.points, t);
        return p && t - p[0] <= limit && !(row.inactive && t > row.lastAt) ? p : null;
      }
      function at(row) { var p = sample(row); return p ? p[1] : null; }
      flat.forEach(function (f) {
        var r = f.row;
        if (f.depth) { tipRow(tip, "　" + r.sub, at(r), r.id === rowId); return; }
        var p = sample(r);
        var suffix = r.children ? translate("total_suffix") + (p && p[5] ? translate("estimated_suffix") : "") : r.inactive ? translate("inactive_suffix") : "";
        tipRow(tip, r.name + suffix, p ? p[1] : null, r.id === rowId);
        if (r.children && !chartExpanded[r.id]) {
          r.children.slice(0, 5).forEach(function (c) { tipRow(tip, "　" + c.sub, at(c), false); });
          if (r.children.length > 5) tip.appendChild(el("div", { class: "row muted", text: translate("more_accounts", r.children.length - 5) }));
        }
      });
      var near = (g.to - g.from) / g.plotW * 8;
      events.forEach(function (ev) {
        if (Math.abs(ev.t - t) <= near) tip.appendChild(el("div", { class: "note" + (ev.failed ? " failed" : "") }, noteNodes(ev)));
      });
      tip.hidden = false;
      var scale = box.clientWidth / g.width;
      placeTip(tip, box, g, t, ((rowY[rowId] != null ? rowY[rowId] : ey) + 28) * scale);
    });
  }

  // Monotone cubic (Fritsch-Carlson): smooth, but never overshoots the
  // samples, so a quota that only falls is never drawn rising.
  function monotonePath(xy) {
    var n = xy.length;
    if (n < 3) return xy.map(function (p, i) { return (i ? "L" : "M") + p[0] + " " + p[1]; }).join("");
    var dx = [], m = [], tg = [], i;
    for (i = 0; i < n - 1; i++) { dx[i] = xy[i + 1][0] - xy[i][0]; m[i] = dx[i] ? (xy[i + 1][1] - xy[i][1]) / dx[i] : 0; }
    tg[0] = m[0]; tg[n - 1] = m[n - 2];
    for (i = 1; i < n - 1; i++) tg[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
    for (i = 0; i < n - 1; i++) {
      if (!m[i]) { tg[i] = tg[i + 1] = 0; continue; }
      var a = tg[i] / m[i], b = tg[i + 1] / m[i], h = a * a + b * b;
      if (h > 9) { var k = 3 / Math.sqrt(h); tg[i] = k * a * m[i]; tg[i + 1] = k * b * m[i]; }
    }
    var d = "M" + xy[0][0] + " " + xy[0][1];
    for (i = 0; i < n - 1; i++) {
      var third = dx[i] / 3;
      d += "C" + (xy[i][0] + third) + " " + (xy[i][1] + tg[i] * third) + " " + (xy[i + 1][0] - third) + " " +
        (xy[i + 1][1] - tg[i + 1] * third) + " " + xy[i + 1][0] + " " + xy[i + 1][1];
    }
    return d;
  }

  // Within one window a quota only falls, but active queries and response
  // headers can disagree by a point and round differently, so samples step
  // back up for a moment. Pool adjacent violators to the closest
  // non-increasing sequence; a curve through the raw samples would show
  // each reversal as a notch.
  function declining(pts, key) {
    var blocks = [];
    pts.forEach(function (p) {
      blocks.push({ sum: p[key], n: 1 });
      while (blocks.length > 1) {
        var top = blocks[blocks.length - 1], prev = blocks[blocks.length - 2];
        if (top.sum / top.n <= prev.sum / prev.n) break;
        prev.sum += top.sum; prev.n += top.n;
        blocks.pop();
      }
    });
    var out = [], i = 0;
    blocks.forEach(function (b) {
      for (var j = 0; j < b.n; j++, i++) {
        var p = pts[i].slice();
        p[key] = b.sum / b.n;
        out.push(p);
      }
    });
    return out;
  }

  // Keep the first sample of every value, and the last sample of a plateau
  // only when a real drop follows it: 62, 61, 60 is a steady decline, while
  // 100 held for an hour and then 80 is a burst of use.
  function curvePoints(pts, key) {
    return pts.filter(function (p, i) {
      if (i === 0 || i === pts.length - 1 || p[key] !== pts[i - 1][key]) return true;
      return Math.abs(pts[i + 1][key] - p[key]) > QUANTUM;
    });
  }

  // Split a line into runs of measured and estimated stretches (index 5 of
  // a pooled point, see pooled). A stretch is estimated where either end is,
  // and neighbouring runs share their end point, so the line stays joined.
  function estimatedRuns(pts) {
    var runs = [];
    for (var i = 0; i + 1 < pts.length; i++) {
      var estimated = !!(pts[i][5] || pts[i + 1][5]), run = runs[runs.length - 1];
      if (run && run.estimated === estimated) run.pts.push(pts[i + 1]);
      else runs.push({ estimated: estimated, pts: [pts[i], pts[i + 1]] });
    }
    return runs.length ? runs : [{ estimated: !!(pts[0] && pts[0][5]), pts: pts }];
  }

  // Split samples where any of the keys rises by a reset. Resets one sample
  // apart (two accounts, or a reset seen late) count as one.
  function splitAtResets(pts, keys) {
    var parts = [[pts[0]]], resets = [];
    for (var i = 1; i < pts.length; i++) {
      var up = keys.some(function (k) { return pts[i][k] - pts[i - 1][k] >= RESET_JUMP; });
      if (up && parts.length > 1 && parts[parts.length - 1].length === 1) {
        parts[parts.length - 1] = [];
        resets[resets.length - 1] = pts[i][0];
      } else if (up) {
        parts.push([]);
        resets.push(pts[i][0]);
      }
      parts[parts.length - 1].push(pts[i]);
    }
    // Hold each piece's last value until the reset sample, so the line
    // reaches the reset instead of stopping a poll interval before it.
    for (var k = 0; k + 1 < parts.length; k++) {
      var tail = parts[k][parts[k].length - 1];
      parts[k].push([parts[k + 1][0][0]].concat(tail.slice(1)));
    }
    return { parts: parts, resets: resets };
  }

  var WINDOW_SECONDS = { "5h": 5 * 3600, "7d": 7 * 86400 };
  // A current reset this many seconds from the last one in the history is a
  // new reset the history does not hold yet.
  var RESET_DRIFT = 60;

  // The even pace of one account's window, as segments [a, b, x]: from a to
  // b the pace is 100 × (x − t) / length, falling to 0% at the reset x.
  // resets lists [t, x] in time order: from the sample at t on, the window
  // was due to reset at x, 0 when unknown. Each entry draws from t, or one
  // window length before x when that is later, until the next entry or x.
  // A window reset early gets a new entry, so the line of the old reset
  // stops there instead of being squeezed down to 0%; no line is drawn
  // where the reset is unknown. current is the reset in the status, 0 when
  // the status reports the window without a reset, or null when it does not
  // report the window. When it differs from the last entry, the history does
  // not hold it yet, and it applies from now: a new reset draws from now, an
  // unknown one ends the line now.
  function paceSegments(resets, length, current, now) {
    var list = (resets || []).filter(function (r) { return Array.isArray(r) && isFinite(r[0]) && isFinite(r[1]); });
    var last = list[list.length - 1];
    var lastKnown = !!last && last[1] > 0;
    if (current != null && isFinite(current) && (!last || now >= last[0])) {
      if (current > 0 ? !lastKnown || Math.abs(current - last[1]) > RESET_DRIFT : lastKnown) list = list.concat([[now, current > 0 ? current : 0]]);
    }
    var out = [];
    if (!(length > 0)) return out;
    // A segment cut by an unknown reset leaves out its end, the time from
    // which the reset is unknown, as the 4th element.
    list.forEach(function (r, i) {
      var x = r[1];
      if (!(x > 0)) return;
      var next = list[i + 1];
      var a = Math.max(r[0], x - length), b = next ? Math.min(next[0], x) : x;
      if (b > a) out.push([a, b, x, !!next && !(next[1] > 0) && next[0] <= x]);
    });
    return out;
  }

  // Where a line of the even pace starts after its cycle began, because
  // the reset was unknown before or moved within the cycle, it extends
  // back to the start of the cycle: the last rise of the samples by a reset
  // (RESET_JUMP) since one window length before its reset. Segments that
  // follow each other with resets within RESET_DRIFT count as one line, so
  // a reset that wavers by seconds extends nothing. The extensions are
  // [a, b, x] like the segments of paceSegments, and the line they extend
  // stays as it is.
  function paceExtensions(segments, points, length) {
    var out = [];
    points = points || [];
    segments.forEach(function (s, i) {
      var prev = segments[i - 1];
      if (prev && prev[1] === s[0] && !prev[3] && Math.abs(prev[2] - s[2]) <= RESET_DRIFT) return;
      if (s[0] <= s[2] - length + RESET_DRIFT) return;
      var start = null;
      for (var k = 1; k < points.length && points[k][0] <= s[0]; k++) {
        if (points[k][0] >= s[2] - length - RESET_DRIFT && points[k][1] - points[k - 1][1] >= RESET_JUMP) start = points[k][0];
      }
      if (start != null && start < s[0]) out.push([start, s[0], s[2]]);
    });
    return out;
  }

  // The even pace at t, or null outside the segments. Where two segments
  // meet, the later one counts, and where the reset becomes unknown there
  // is none.
  function paceAt(segments, length, t) {
    var value = null;
    segments.forEach(function (s) {
      if (t >= s[0] && (s[3] ? t < s[1] : t <= s[1])) value = 100 * (s[2] - t) / length;
    });
    return value;
  }

  // The even pace of a total: the mean of its accounts' even pace weighted
  // like the total, only while every account has one. parts are
  // {weight, segments}.
  function pooledPace(parts, length, t) {
    var sum = 0, total = 0;
    for (var i = 0; i < parts.length; i++) {
      var value = paceAt(parts[i].segments, length, t);
      if (value == null) return null;
      sum += parts[i].weight * value;
      total += parts[i].weight;
    }
    return total ? sum / total : null;
  }

  // The extended even pace of a total (paceExtensions): where any account
  // has an extension, the mean with the extensions in place of the lines
  // they extend, while every account has a value. parts are {weight,
  // segments, extensions}.
  function extendedPace(parts, length, t) {
    var sum = 0, total = 0, used = false;
    for (var i = 0; i < parts.length; i++) {
      var value = null;
      parts[i].extensions.forEach(function (e) { if (t >= e[0] && t <= e[1]) value = 100 * (e[2] - t) / length; });
      if (value != null) used = true;
      else value = paceAt(parts[i].segments, length, t);
      if (value == null) return null;
      sum += parts[i].weight * value;
      total += parts[i].weight;
    }
    return used && total ? sum / total : null;
  }

  // The even pace of a row: at(t) is its value, null where it has none,
  // extended(t) the value of its extensions back to the cycle start, and
  // breaks are the times where either can bend or jump. A pooled row
  // combines the even pace of its accounts, each from its own resets.
  function rowPace(row, now) {
    var length = WINDOW_SECONDS[historyWindow];
    var parts = !length ? [] : (row.children || [row]).map(function (r) {
      var segments = paceSegments(r.resets, length, r.reset, now);
      return { weight: row.children ? r.weight : 1, segments: segments, extensions: paceExtensions(segments, r.points, length) };
    });
    var breaks = [];
    parts.forEach(function (part) {
      part.segments.forEach(function (s) { breaks.push(s[0], s[1]); });
      part.extensions.forEach(function (e) { breaks.push(e[0], e[1]); });
    });
    return { breaks: breaks, at: function (t) { return parts.length ? pooledPace(parts, length, t) : null; },
      extended: function (t) { return parts.length ? extendedPace(parts, length, t) : null; } };
  }

  // The least-squares line through pts[a..b]: its slope per second and the
  // largest distance of a sample from it, in points.
  function lineFit(pts, a, b) {
    var n = b - a + 1, t0 = pts[a][0], st = 0, sv = 0, stt = 0, stv = 0, i, t;
    for (i = a; i <= b; i++) {
      t = pts[i][0] - t0;
      st += t; sv += pts[i][1]; stt += t * t; stv += t * pts[i][1];
    }
    var d = n * stt - st * st, slope = d ? (n * stv - st * sv) / d : 0, base = (sv - slope * st) / n, error = 0;
    for (i = a; i <= b; i++) error = Math.max(error, Math.abs(base + slope * (pts[i][0] - t0) - pts[i][1]));
    return { slope: slope, error: error };
  }

  // The current rate of use, in points per second, from a bottom-up
  // piecewise linear fit: neighbouring pieces merge, the closest fit first,
  // while one line stays within QUANTUM points of all their samples. Steps
  // of whole percents then fit as one line, while a pause or a change in the
  // rate of use starts a new piece, so how far back the rate looks depends
  // on the samples. The slope between two samples of a staircase would be
  // zero or a whole point in a moment.
  function recentSlope(pts) {
    if (pts.length < 2) return 0;
    var pieces = [], costs = [], i, j;
    for (i = 0; i + 1 < pts.length; i++) pieces.push([i, i + 1]);
    function cost(k) { return lineFit(pts, pieces[k][0], pieces[k + 1][1]).error; }
    for (i = 0; i + 1 < pieces.length; i++) costs.push(cost(i));
    while (costs.length) {
      for (j = 0, i = 1; i < costs.length; i++) if (costs[i] < costs[j]) j = i;
      if (costs[j] > QUANTUM) break;
      pieces.splice(j, 2, [pieces[j][0], pieces[j + 1][1]]);
      costs.splice(j, 1);
      if (j < costs.length) costs[j] = cost(j);
      if (j > 0) costs[j - 1] = cost(j - 1);
    }
    var last = pieces[pieces.length - 1];
    return lineFit(pts, last[0], last[1]).slope;
  }

  // The current rate of a 5-hour window, in points per second (rate): the
  // recent slope of this cycle's lowest readings so far (cycleFloor). held
  // tells whether the latest value counts until now, as a pause.
  function currentRate(row, now) {
    var floor = row.floor, start = 0;
    for (var i = 1; i < floor.length; i++) if (floor[i][1] > floor[i - 1][1]) start = i;
    var pts = floor.slice(start), tail = pts[pts.length - 1];
    var held = now - tail[0] > (tail[2] === 1 ? HOLD_AFTER : Math.max(HOLD_AFTER, sampleStep() * 1.5));
    if (held) pts.push([now, tail[1]]);
    return { rate: recentSlope(pts), held: held };
  }

  // The rate of a 7-day window, in points per second: what this cycle has
  // used so far, from its lowest reading (cycleFloor), over the time since it
  // began, its reset less the window. The hours without use count too, so
  // the rate follows the daily rhythm of use rather than its latest hours.
  // A pooled row averages the rates of its accounts by their weights, each
  // from its own reset, over the accounts with a cycle under way and a
  // sample in it. Null when there is none.
  function cycleRate(row, now) {
    var length = WINDOW_SECONDS[historyWindow], sum = 0, total = 0;
    (row.children || [row]).forEach(function (r) {
      var start = r.reset - length, low = r.floor[r.floor.length - 1];
      if (!low || !r.reset || start >= now || r.reset <= now || low[0] < start) return;
      sum += r.weight * (100 - low[1]) / (now - start);
      total += r.weight;
    });
    return total ? -sum / total : null;
  }

  // The projection: from the latest value along the rate of use until the
  // quota runs out (out) or the window resets, with what is left then
  // (left). A 5-hour window keeps its current rate; a 7-day window, whose
  // use comes in daily bursts, takes the rate of its cycle so far. A rate
  // that does not fall stays level. It starts (t0) where the line of samples
  // ends, now; for a 5-hour window whose rate does not yet hold the latest
  // value until now, at the latest sample, so the line does not level off
  // before a projection that still falls. Null without a reset ahead on the
  // axis, without current data, with nothing left, without a rate, or for
  // a total whose latest value is estimated.
  function projection(row, g, lastRun, limit) {
    var last = row.points[row.points.length - 1];
    if (g.to <= g.now || !lastRun || lastRun.end < g.now - limit || !(last[1] > 0)) return null;
    if (last[5]) return null;
    var rate, t0 = lastRun.end;
    if (historyWindow === "7d") rate = cycleRate(row, g.now);
    else {
      var current = currentRate(row, g.now);
      rate = current.rate;
      if (!current.held) t0 = last[0];
    }
    if (rate == null) return null;
    var slope = Math.min(0, rate), v0 = last[1];
    var out = slope < 0 ? t0 + v0 / -slope : Infinity;
    return { t0: t0, v0: v0, end: Math.min(out, g.to), out: out <= g.to ? out : null, left: out <= g.to ? 0 : v0 + slope * (g.to - t0),
      at: function (t) { return t < t0 || t > g.to ? null : Math.max(0, v0 + slope * (t - t0)); } };
  }

  // Detail: the selected row as a smoothed line that breaks at resets, the
  // even pace by the scheduled resets (rowPace) and, when the axis reaches ahead, the
  // projection at the current rate. A pooled row adds a band from its
  // lowest to its highest account.
  function renderDetail(row, head, box) {
    var g = chartGeom, limit = gapLimit();
    snapPoints = row.points;
    var group = !!row.children;
    // An inactive window ends at its last sample, without projection or
    // even pace.
    var stretches = runs(row.points, limit, row.inactive ? row.lastAt : g.now);
    var lastRun = stretches[stretches.length - 1];
    var ahead = row.inactive ? null : projection(row, g, lastRun, limit);
    // Hold the last value to the end of each stretch so the line reaches it;
    // the last stretch only to the start of the projection. One account
    // resetting raises a pooled total by its share; the total line breaks
    // only when that share is a visible jump.
    var pieces = stretches.map(function (run) {
      var pts = run.pts.slice(), tail = pts[pts.length - 1], end = ahead && run === lastRun ? ahead.t0 : run.end;
      if (end > tail[0]) pts.push([end].concat(tail.slice(1)));
      return { pts: pts, split: splitAtResets(pts, [1]) };
    });
    var resets = [];
    pieces.forEach(function (piece) { resets = resets.concat(piece.split.resets); });
    var even = row.inactive ? { breaks: [], at: function () { return null; }, extended: function () { return null; } } : rowPace(row, g.now);

    head.appendChild(el("span", { class: "detail-name" }, (row.orphan ? [el("span", { class: SENSITIVE, text: row.name })] : maskedNodes(row.name)).concat([" · " + translate(historyWindow === "5h" ? "quota_5h" : "quota_7d")])));
    if (row.inactive) head.appendChild(badge("muted", translate("inactive")));
    head.appendChild(el("span", { class: "muted" + (group ? "" : " " + SENSITIVE), text: group ? translate("pooled_note", row.children.length) : row.sub }));
    head.appendChild(el("span", { class: "detail-tools" }, [maskButton(), snapshotButton("snapshot_detail", function () { return [$("chart-detail-wrap")]; })]));
    if (row.inactive) {
      head.appendChild(el("span", { class: "detail-stat" }, [document.createTextNode(translate("stat_last")),
        el("strong", { text: pct(row.last) + " · " + fmtDateTime(unixDate(row.lastAt)) })]));
    }
    // The difference from the even pace: green with more left than the pace
    // leaves, red with less.
    var pace = even.at(g.now), diff = pace == null ? null : Math.round(row.last - pace);
    (row.inactive ? [] : [[translate(group ? "stat_total" : "stat_current"), pct(row.last)], [translate("stat_pace"), pct(pace)],
      [translate("stat_diff"), fmtDiff(diff), diff > 0 ? "ahead" : diff < 0 ? "behind" : ""],
      [translate("stat_to_reset"), row.reset ? fmtCountdown(row.reset * 1000 - Date.now()) : "—"]]).forEach(function (stat) {
      head.appendChild(el("span", { class: "detail-stat" }, [document.createTextNode(stat[0]), el("strong", { class: stat[2] || null, text: stat[1] })]));
    });

    var H = 206, top = 22, bottom = 24, plotH = H - top - bottom;
    function y(v) { return top + (1 - v / 100) * plotH; }
    var spoken = masked && (row.orphan || hasSecret(row.name)) ? accountAlias(row.account) : row.name;
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + H, role: "img", "aria-label": translate("detail_aria", spoken) });
    var defs = hatchPattern(root);
    var fade = svg("linearGradient", { id: "chart-fade", x1: 0, x2: 0, y1: 0, y2: 1 });
    fade.appendChild(svg("stop", { offset: "0%", "stop-color": cssVar("--text-secondary"), "stop-opacity": 0.18 }));
    fade.appendChild(svg("stop", { offset: "100%", "stop-color": cssVar("--text-secondary"), "stop-opacity": 0 }));
    defs.appendChild(fade);
    // Samples before the start of an axis that ends ahead of now lie left of
    // the plot; the lines are cut at its edge.
    var clip = svg("clipPath", { id: "chart-plot" });
    clip.appendChild(svg("rect", { x: g.labelW, y: 0, width: g.plotW, height: H }));
    defs.appendChild(clip);
    [0, 50, 100].forEach(function (v) {
      root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: y(v), y2: y(v), stroke: cssVar(v ? "--border-color" : "--border-primary"), "stroke-dasharray": v ? "2 4" : "none" }));
      root.appendChild(svgText({ x: g.labelW - 8, y: y(v) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--text-tertiary") }, v + "%"));
    });
    var low = status.config ? Number(status.config.low_threshold) : 0;
    if (low > 0 && low < 100) {
      root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: y(low), y2: y(low), stroke: cssVar("--viz-failure"), "stroke-opacity": 0.5, "stroke-dasharray": "4 4" }));
      root.appendChild(svgText({ x: g.labelW - 8, y: y(low) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--failure-text") }, axisPercent(low)));
    }
    // The time the projection runs out goes on the time axis, in place of the
    // tick labels it would cover.
    var outLabel = null;
    if (ahead && ahead.out != null) {
      var outText = historyWindow === "7d" ? fmtDateTime(unixDate(ahead.out)) : fmtClock(unixDate(ahead.out));
      var outW = outText.length * CHAR_W;
      outLabel = { text: outText, w: outW, x: Math.max(g.labelW + outW / 2, Math.min(g.width - outW / 2, g.x(ahead.out))) };
    }
    // A tick at the edge of the plot keeps its label but not its grid line,
    // which would run beside the edge line.
    var ticks = chartTicks(g);
    ticks.list.forEach(function (t) {
      var tx = g.x(t);
      if (tx > g.labelW + 4 && tx < g.labelW + g.plotW - 4) {
        root.appendChild(svg("line", { x1: tx, x2: tx, y1: top, y2: top + plotH, stroke: cssVar("--border-color"), "stroke-dasharray": "2 4" }));
      }
      var text = ticks.label(t);
      if (outLabel && Math.abs(g.x(t) - outLabel.x) < (outLabel.w + text.length * CHAR_W) / 2 + 6) return;
      root.appendChild(svgText({ x: g.x(t), y: H - 6, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-tertiary") }, text));
    });

    var cursor = g.from;
    stretches.forEach(function (run) {
      if (run.start > cursor + limit) hatchRect(root, g, cursor, run.start, top, plotH);
      cursor = run.end;
    });
    // With a projection, the hatching starts where it does, the end of the line.
    hatchRect(root, g, ahead ? ahead.t0 : row.inactive ? cursor : dataEnd(g, cursor, limit), g.to, top, plotH);
    // The first stretch on the axis: a sample before it, from before the
    // range, can form a stretch of its own that ends left of the axis.
    // A window without samples on the axis has no data up to now.
    var firstIn = stretches.filter(function (run) { return run.end > g.from; })[0];
    if (!firstIn || firstIn.start > g.from + limit) {
      root.appendChild(svgText({ x: (g.x(g.from) + g.x(firstIn ? firstIn.start : Math.min(g.now, g.to))) / 2, y: top + plotH / 2 + 4, "text-anchor": "middle", "font-size": 12,
        fill: cssVar("--text-tertiary"), "class": "row-name" }, translate("no_data")));
    }

    // The edges of the plot, solid like the 0% line, over the hatching and
    // beneath the lines and their marks.
    [g.labelW, g.labelW + g.plotW].forEach(function (x) {
      root.appendChild(svg("line", { x1: x, x2: x, y1: top, y2: top + plotH, stroke: cssVar("--border-primary") }));
    });

    var plot = svg("g", { "clip-path": "url(#chart-plot)" });
    root.appendChild(plot);
    // Drawn first, so the even-pace line lies beneath the curves. It is
    // straight between two breaks, and its value just inside each end
    // decides whether it continues from the previous piece or jumps.
    var times = [g.from, g.to].concat(even.breaks.filter(function (t) { return t > g.from && t < g.to; })).sort(function (a, b) { return a - b; });
    function pacePath(at) {
      var d = "", end = null;
      for (var k = 0; k + 1 < times.length; k++) {
        var a = times[k], b = times[k + 1], inset = Math.min(1, (b - a) / 4);
        if (b - a <= 0) continue;
        var va = at(a + inset), vb = at(b - inset);
        if (va == null || vb == null) { end = null; continue; }
        if (end == null || Math.abs(end - va) > 0.5) d += "M" + g.x(a) + " " + y(va);
        d += "L" + g.x(b) + " " + y(vb);
        end = vb;
      }
      return d;
    }
    var paceD = pacePath(even.at), extendedD = pacePath(even.extended);
    if (paceD) plot.appendChild(svg("path", { d: paceD, fill: "none", stroke: cssVar("--text-tertiary"), "stroke-opacity": 0.8, "stroke-width": 1.5, "stroke-dasharray": "6 4" }));
    // The extension back to the cycle start: dash-dot, apart from the
    // dashes of the even pace and the dots of the projection.
    if (extendedD) plot.appendChild(svg("path", { d: extendedD, fill: "none", stroke: cssVar("--text-tertiary"), "stroke-opacity": 0.8, "stroke-width": 1.5, "stroke-dasharray": "8 3 1.5 3" }));
    $("legend-pace-extended").hidden = !extendedD;
    var estimatedShown = false;
    pieces.forEach(function (piece) {
      if (group) splitAtResets(piece.pts, [3, 4]).parts.forEach(function (part) {
        if (part.length < 2) return;
        var upper = curvePoints(declining(part, 4), 4).map(function (p) { return [g.x(p[0]), y(p[4])]; });
        var lower = curvePoints(declining(part, 3), 3).map(function (p) { return [g.x(p[0]), y(p[3])]; }).reverse();
        plot.appendChild(svg("path", { d: monotonePath(upper) + monotonePath(lower).replace(/^M/, "L") + "Z", fill: cssVar("--text-secondary"), "fill-opacity": 0.13 }));
      });
      // Each piece starts at its reset sample, so the return to full is a
      // break in the line rather than a slope that never happened.
      // A total with an account's share estimated (pooled) is dashed there.
      piece.split.parts.forEach(function (part) {
        estimatedRuns(declining(part, 1)).forEach(function (run) {
          var xy = curvePoints(run.pts, 1).map(function (p) { return [g.x(p[0]), y(p[1])]; });
          if (xy.length === 1) xy.push([xy[0][0] + 1, xy[0][1]]);
          var d = monotonePath(xy);
          if (!group) plot.appendChild(svg("path", { d: d + "L" + xy[xy.length - 1][0] + " " + y(0) + "L" + xy[0][0] + " " + y(0) + "Z", fill: "url(#chart-fade)" }));
          var line = { d: d, fill: "none", stroke: cssVar("--text-primary"), "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" };
          if (run.estimated) { line["stroke-dasharray"] = "5 4"; line["stroke-opacity"] = 0.7; }
          plot.appendChild(svg("path", line));
          if (run.estimated) estimatedShown = true;
        });
      });
    });
    // Like the extension's, the legend explains the dashes only when drawn.
    $("legend-estimated").hidden = !estimatedShown;
    var lastLabel = -99;
    resets.forEach(function (t) {
      if (t < g.from) return;
      var x = g.x(t);
      root.appendChild(svg("line", { x1: x, x2: x, y1: top - 4, y2: top + plotH, stroke: cssVar("--text-tertiary"), "stroke-opacity": 0.7, "stroke-dasharray": "3 3" }));
      if (x - lastLabel > 52) {
        root.appendChild(svgText({ x: x, y: top - 8, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-tertiary") }, "↻ " + fmtClock(unixDate(t))));
        lastLabel = x;
      }
    });
    var groups = group ? row.groups : [row.group];
    ignitionEvents(g).forEach(function (ev) {
      if (groups.indexOf(ev.group) < 0) return;
      var x = g.x(ev.t), base = top + plotH;
      if (ev.failed) root.appendChild(svg("path", { d: "M" + (x - 4) + " " + (base - 8) + "L" + (x + 4) + " " + base + "M" + (x + 4) + " " + (base - 8) + "L" + (x - 4) + " " + base,
        stroke: cssVar("--viz-failure"), "stroke-width": 2.2, "stroke-linecap": "round" }));
      else root.appendChild(svg("path", { d: "M" + x + " " + (base - 7) + "L" + (x + 4) + " " + base + "L" + (x - 4) + " " + base + "Z", fill: cssVar("--viz-success") }));
    });
    // The projection ends where the quota runs out, with the time on the
    // time axis, or at the reset, the right end of the plot, with what is
    // left beside it.
    var rightX = g.labelW + g.plotW + 10;
    if (ahead) {
      var px = g.x(ahead.end), py = y(ahead.out != null ? 0 : ahead.left);
      root.appendChild(svg("line", { x1: g.x(ahead.t0), y1: y(ahead.v0), x2: px, y2: py, stroke: cssVar("--text-primary"), "stroke-opacity": 0.75,
        "stroke-width": 2, "stroke-dasharray": "1 4", "stroke-linecap": "round" }));
      root.appendChild(svg("circle", { cx: px, cy: py, r: 3, fill: cssVar("--text-primary"), stroke: cssVar("--bg-primary"), "stroke-width": 1.5 }));
      if (outLabel) root.appendChild(svgText({ x: outLabel.x, y: H - 6, "text-anchor": "middle", "font-size": 11, "font-weight": 650, fill: cssVar("--text-primary") }, outLabel.text));
      else root.appendChild(svgText({ x: rightX, y: py + 4, "font-size": 12, "font-weight": 650, fill: cssVar("--text-secondary") }, pct(ahead.left)));
    }
    var last = row.points[row.points.length - 1];
    if (last && !row.inactive && lastRun.end >= g.now - limit) {
      root.appendChild(svg("circle", { cx: g.x(ahead ? ahead.t0 : lastRun.end), cy: y(last[1]), r: 4, fill: cssVar("--text-primary"), stroke: cssVar("--bg-primary"), "stroke-width": 2 }));
    }
    // An axis that ends now shows the current value and the even pace at the
    // ends of their lines, with the difference between them. The difference
    // sits halfway, and values too close to it move apart.
    if (g.to <= g.now && !row.inactive) {
      var yNow = y(row.last), yPace = pace == null ? null : y(pace);
      if (yPace == null) root.appendChild(svgText({ x: rightX, y: yNow + 4, "font-size": 13, "font-weight": 650, fill: cssVar("--text-primary") }, pct(row.last)));
      else {
        var mid = (yNow + yPace) / 2, upper = yNow <= yPace;
        var yA = Math.min(upper ? yNow : yPace, mid - LABEL_H), yB = Math.max(upper ? yPace : yNow, mid + LABEL_H);
        var shift = Math.max(0, top - yA) - Math.max(0, yB - (top + plotH));
        root.appendChild(svgText({ x: rightX, y: (upper ? yA : yB) + shift + 4, "font-size": 13, "font-weight": 650, fill: cssVar("--text-primary") }, pct(row.last)));
        root.appendChild(svgText({ x: rightX, y: (upper ? yB : yA) + shift + 4, "font-size": 12, fill: cssVar("--text-tertiary") }, pct(pace)));
        root.appendChild(svgText({ x: rightX, y: mid + shift + 4, "font-size": 12, "font-weight": 650,
          fill: cssVar(diff > 0 ? "--success-text" : diff < 0 ? "--failure-text" : "--text-secondary") }, fmtDiff(diff)));
      }
    }

    var cross = svg("line", { y1: top, y2: top + plotH, stroke: cssVar("--text-tertiary"), visibility: "hidden", "pointer-events": "none" });
    root.appendChild(cross);
    var dot = svg("circle", { r: 4, fill: cssVar("--text-primary"), stroke: cssVar("--bg-primary"), "stroke-width": 2, visibility: "hidden", "pointer-events": "none" });
    root.appendChild(dot);
    var hit = svg("rect", { x: g.labelW, y: top, width: g.plotW, height: plotH, fill: "transparent", tabindex: "0",
      "aria-label": translate("detail_keys") });
    hit.addEventListener("pointermove", function (e) { hoverAt(e, root, row.id, "detail"); });
    root.appendChild(hit);
    root.addEventListener("pointerleave", function () { setHover(null); });
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    // Keyboard: step through the samples on the axis.
    var keyIndex = -1, first = 0;
    while (first < row.points.length - 1 && row.points[first][0] < g.from) first++;
    function showSample(i) {
      if (i < first || i >= row.points.length) return;
      keyIndex = i;
      hoverSource = "detail";
      setHover(row.points[i][0], row.id);
    }
    hit.addEventListener("focus", function () { showSample(keyIndex < 0 ? row.points.length - 1 : keyIndex); });
    hit.addEventListener("blur", function () { setHover(null); });
    hit.addEventListener("keydown", function (e) {
      if (e.key === "ArrowLeft") { showSample(Math.max(first, keyIndex - 1)); e.preventDefault(); }
      if (e.key === "ArrowRight") { showSample(Math.min(row.points.length - 1, keyIndex + 1)); e.preventDefault(); }
    });

    function paceRow(t) {
      var value = even.at(t), extended = even.extended(t);
      if (value != null) tip.appendChild(el("div", { class: "row pace" }, [el("span", { class: "key" }),
        el("span", { class: "name", text: translate("pace_reference") }), el("span", { class: "value", text: pct(value) })]));
      if (extended != null) tip.appendChild(el("div", { class: "row pace extended" }, [el("span", { class: "key" }),
        el("span", { class: "name", text: translate("pace_extended") }), el("span", { class: "value", text: pct(extended) })]));
    }

    hoverListeners.push(function (t) {
      if (t == null) { cross.setAttribute("visibility", "hidden"); dot.setAttribute("visibility", "hidden"); tip.hidden = true; return; }
      cross.setAttribute("x1", g.x(t)); cross.setAttribute("x2", g.x(t)); cross.setAttribute("visibility", "visible");
      // After now: the projection and the even pace at the pointer's time.
      if (t > g.now) {
        var projected = ahead ? ahead.at(t) : null;
        if (projected != null) { dot.setAttribute("cx", g.x(t)); dot.setAttribute("cy", y(projected)); dot.setAttribute("visibility", "visible"); }
        else dot.setAttribute("visibility", "hidden");
        if (hoverSource !== "detail") { tip.hidden = true; return; }
        clear(tip);
        tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
        tip.appendChild(el("div", { class: "row projection strong" }, [el("span", { class: "key" }),
          el("span", { class: "name", text: translate("projected_" + historyWindow) }), el("span", { class: "value", text: pct(projected) })]));
        paceRow(t);
        tip.hidden = false;
        placeTip(tip, box, g, t, 6);
        return;
      }
      var p = valueAt(row.points, t);
      if (p && (t - p[0] > limit || (row.inactive && t > row.lastAt))) p = null;
      if (p) { dot.setAttribute("cx", g.x(p[0])); dot.setAttribute("cy", y(p[1])); dot.setAttribute("visibility", "visible"); }
      else dot.setAttribute("visibility", "hidden");
      if (hoverSource !== "detail") { tip.hidden = true; return; }
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
      if (group) {
        tipRow(tip, translate("total") + (p && p[5] ? translate("estimated_suffix") : ""), p ? p[1] : null, true);
        row.children.slice(0, 6).forEach(function (c) {
          var q = valueAt(c.points, t);
          tipRow(tip, "　" + c.sub, q && t - q[0] <= limit ? q[1] : null, false);
        });
        if (row.children.length > 6) tip.appendChild(el("div", { class: "row muted", text: translate("more_accounts", row.children.length - 6) }));
      } else {
        tipRow(tip, row.name + (p ? translate(p[2] === 1 ? "passive_sample" : "active_sample") : ""), p ? p[1] : null, true);
      }
      paceRow(t);
      var near = (g.to - g.from) / g.plotW * 8;
      resets.forEach(function (r) { if (r >= g.from && Math.abs(r - t) <= near) tip.appendChild(el("div", { class: "note", text: fmtClock(unixDate(r)) + " " + translate("quota_reset") })); });
      tip.hidden = false;
      placeTip(tip, box, g, t, 6);
    });
  }

  // A y-axis label of the detail chart, such as the low threshold, with at
  // most one decimal: "12.5%" fits the SIDE_GAP pixels left of the plot
  // beside the heading, which "12.345%" would cross into.
  function axisPercent(v) {
    return Math.round(v * 10) / 10 + "%";
  }

  // The detail chart's heading goes, as a column (.side), into the blank
  // left of the plot that lines the plot up with the overview; the plot
  // stays where it is. The y-axis labels keep the right SIDE_GAP pixels of
  // the blank. Where the blank is too narrow, in a chart below 640 pixels
  // wide (labelW 132), or too short for the heading, such as with a long
  // name, the heading stays above the chart.
  var SIDE_GAP = 46;
  function placeDetailHead(wrap, head, box, g) {
    wrap.classList.remove("side");
    if (g.labelW < 190 || g.width !== box.clientWidth) return;
    wrap.style.setProperty("--detail-side", (g.labelW - SIDE_GAP) + "px");
    wrap.classList.add("side");
    if (head.offsetHeight > box.offsetHeight) wrap.classList.remove("side");
  }

  function hoverAt(e, root, rowId, source) {
    var rect = root.getBoundingClientRect();
    var t = chartGeom.t((e.clientX - rect.left) / rect.width * chartGeom.width);
    if (t < chartGeom.from || t > chartGeom.to) { setHover(null); return; }
    hoverSource = source;
    setHover(snapTime(t), rowId);
  }

  // The nearest sample of the detail row, so the crosshair and tooltip show a
  // measured time. Inside a data gap and after now the pointer's own time is
  // kept.
  function snapTime(t) {
    if (t > chartGeom.now) return t;
    var best = null;
    snapPoints.forEach(function (p) {
      if (p[0] >= chartGeom.from && p[0] <= chartGeom.to && (best == null || Math.abs(p[0] - t) < Math.abs(best - t))) best = p[0];
    });
    return best != null && Math.abs(best - t) <= gapLimit() ? best : t;
  }

  function setHover(t, rowId) {
    hoverListeners.forEach(function (fn) { fn(t, rowId); });
  }

  // The table covers the requested range, without the sample before it.
  function renderChartTable(rows) {
    var table = $("chart-table");
    var from = new Date(history.from).getTime() / 1000;
    clear(table);
    rows.forEach(function (r) {
      (r.children || [r]).forEach(function (c) {
        var values = c.points.filter(function (p) { return p[0] >= from; }).map(function (p) { return p[1]; });
        table.appendChild(el("tr", {}, [
          c.orphan ? el("td", { class: SENSITIVE, text: c.name }) : el("td", {}, maskedNodes(c.name)),
          el("td", { class: SENSITIVE, text: c.sub }),
          el("td", { class: "num", text: pct(c.last) }),
          el("td", { class: "num", text: pct(values.length ? Math.min.apply(null, values) : null) }),
          el("td", { class: "num", text: pct(values.length ? Math.max.apply(null, values) : null) }),
          el("td", { class: "num", text: String(values.length) }),
          el("td", { class: "num", text: c.inactive ? translate("inactive") : c.reset ? fmtCountdown(c.reset * 1000 - Date.now()) : "—" })
        ]));
      });
    });
  }

  // The level swatches of the legend, with the thresholds in use.
  function renderLegend() {
    var t = quotaThresholds();
    $("legend-full").textContent = translate("level_full") + " 100%";
    $("legend-high").textContent = translate("level_high") + " > " + t.notice + "%";
    $("legend-medium").textContent = translate("level_medium") + " " + t.low + "–" + t.notice + "%";
    $("legend-low").textContent = translate("level_low") + " ≤ " + t.low + "%";
    $("legend-empty").textContent = translate("level_empty") + " 0%";
  }

  // The chart is cleared and drawn again, and drawing it measures its width,
  // which lays the page out while the chart is empty. The card keeps its
  // height meanwhile, or the shorter page would pull the scroll position up.
  function renderChart() {
    if (!contentReady) return;
    renderLegend();
    // The chart measures its width, so a hidden chart is drawn when its view
    // shows.
    if ($("view-quota").hidden) return;
    var card = $("chart").parentNode;
    card.style.minHeight = card.offsetHeight + "px";
    try {
      drawChart();
    } finally {
      card.style.minHeight = "";
    }
  }

  function drawChart() {
    var overview = $("chart"), head = $("chart-detail-head"), detail = $("chart-detail");
    chartBehind = false;
    $("legend-estimated").hidden = true;
    overview.classList.toggle("loading", historyLoading !== "" && historyLoading !== historyKey(historyWindow, historyRange));
    clear(overview); clear(head); clear(detail);
    clear($("chart-table"));
    hoverListeners = [];
    snapPoints = [];
    $("chart-detail-wrap").hidden = true;
    if (!history || !status) {
      overview.appendChild(el("div", { class: "empty", text: historyFailed ? translate("history_failed", historyFailed) : translate("loading") }));
      return;
    }
    $("legend-projection").textContent = translate("legend_projection_" + historyWindow);
    var rows = chartRows();
    if (!rows.length) {
      overview.appendChild(el("div", { class: "empty", text: translate("no_history") }));
      return;
    }
    if (!findRow(rows, chartSelected)) chartSelected = rows[0].id;
    var selected = findRow(rows, chartSelected);
    renderOverview(rows, overview, axisEnd(selected));
    $("chart-detail-wrap").hidden = false;
    renderDetail(selected, head, detail);
    placeDetailHead($("chart-detail-wrap"), head, detail, chartGeom);
    renderChartTable(rows);
  }

  // The buttons are rebuilt only when the window or the language changes;
  // otherwise the chosen range is marked in place, so the thumb stays on its
  // buttons. Clicks are handled by bindSegmented.
  function renderRangeButtons() {
    var box = $("range-buttons");
    var shown = chartWindow + " " + lang;
    if (box.getAttribute("data-shown") !== shown) {
      box.setAttribute("data-shown", shown);
      Array.prototype.forEach.call(box.querySelectorAll(".seg"), function (b) {
        thumbObserver.unobserve(b);
        box.removeChild(b);
      });
      RANGES[chartWindow].forEach(function (r) {
        box.appendChild(el("button", { type: "button", class: "seg", "data-value": r[0], text: translate("range_" + r[2], r[1]) }));
      });
      observeOptions(box);
    }
    pressSegment("range-buttons", range);
  }

  // ---- Page state in the config ----

  // Magpie shows the page in a sandboxed iframe without browser storage,
  // so the choices the page remembers are kept in config.json and shared by
  // every device: chart_view and chart_expanded with the switches that keep
  // them, mask_accounts and events_show_5h_resets. They are restored once,
  // from the first valid config the page reads (restorePageState,
  // restoreChartState); the app stays hidden until then (see loadPage), so
  // no choice can be made before it arrives. The plugin only saves a patch
  // of these keys, without applying it or a config_token (see patchConfig).
  var pageStateReady = false;
  var chartRestored = false;
  var pageStateWrites = {};
  var pageStateFailed = {};
  // The settings switch that keeps the state, keyed by the state.
  var PAGE_STATE_SWITCH = { chart_view: "remember_chart_filters", chart_expanded: "remember_chart_expanded" };

  // Saves patch, a null value deleting its key, as a write of group, the
  // state it changes. One write per group is under way at a time; changes
  // made meanwhile merge into the next write, later values winning, so the
  // order of the changes is the order of the saves and the last value is
  // the one saved. A switch writes with its state in the same patch. say,
  // from claimState, reports the result of the write that carries it.
  function writePageState(group, patch, say) {
    var w = pageStateWrites[group] || (pageStateWrites[group] = { running: null, next: null });
    if (!w.next) w.next = { patch: {}, says: [] };
    Object.keys(patch).forEach(function (name) { w.next.patch[name] = patch[name]; });
    if (say) w.next.says.push(say);
    if (!w.running) w.running = sendPageState(group, w);
    return w.running;
  }

  function sendPageState(group, w) {
    var next = w.next;
    w.next = null;
    return patchConfig(next.patch).then(function () {
      next.says.forEach(function (say) { say("saved", translate("save_saved")); });
      pageStateSaved(group, null, next.says.length > 0);
    }, function (err) {
      next.says.forEach(function (say) { say("failed", err.message); });
      pageStateSaved(group, err, next.says.length > 0);
    }).then(function () {
      if (w.next) return sendPageState(group, w);
      w.running = null;
    });
  }

  // A write made without a switch shows only a failure: the first one in a
  // row in a toast and, for the chart, at the legend of its switch until
  // the state saves again. A switch shows its own result.
  function pageStateSaved(group, err, switched) {
    var failed = pageStateFailed[group];
    pageStateFailed[group] = !!err;
    if (err && !failed && !switched && err.message !== "unauthorized") toast(translate("state_save_failed", err.message));
    var input = PAGE_STATE_SWITCH[group] && $("settings").elements[PAGE_STATE_SWITCH[group]];
    var host = input && saveHost(input);
    if (!host) return;
    if (err && !switched) {
      var entry = saveEntry(host, input, group);
      entry.owner = null;
      entry.order = ++saveSerial;
      updateSaveEntry(entry, "failed", err.message);
      return;
    }
    var done = !err && host.saveEntries && host.saveEntries.get(group);
    if (done && done.failure) {
      done.failure = null;
      done.state = "";
      renderSaveState(host);
    }
  }

  // An earlier version kept the page state in the browser's localStorage.
  // Only the masking moves to the config, so that a browser that blurred
  // the accounts does not show them after the upgrade; the other keys are
  // dropped.
  var OLD_MASK_STORE = "lamplighter-mask-accounts";
  var OLD_STORES = ["lamplighter-events-5h", "lamplighter-chart-filters", "lamplighter-mcp-url"];

  function forgetStores(names) {
    names.forEach(function (name) {
      try { localStorage.removeItem(name); } catch (err) {}
    });
  }

  // The old masking switch is read as the script loads, before any
  // request: another tab of this browser may move it and delete its key
  // while this one waits for its config.
  function readOldMask() {
    try { return localStorage.getItem(OLD_MASK_STORE); } catch (err) { return null; }
  }
  var oldMask = readOldMask();

  // Applies mask_accounts and events_show_5h_resets of cfg, the first
  // valid config. Without mask_accounts in cfg, the old masking switch of
  // this browser, when on, masks and is saved to the config; its key goes
  // once saved, and at once when cfg has mask_accounts.
  function restorePageState(cfg) {
    if (pageStateReady) return;
    pageStateReady = true;
    var migrate = cfg.mask_accounts == null && oldMask === "1";
    forgetStores(migrate ? OLD_STORES : OLD_STORES.concat(OLD_MASK_STORE));
    setMasked(migrate || cfg.mask_accounts === true);
    if (migrate) {
      writePageState("mask_accounts", { mask_accounts: true }, function (state) {
        if (state === "saved") forgetStores([OLD_MASK_STORE]);
      });
    }
    showFiveHourResets = cfg.events_show_5h_resets === true;
  }

  // With remember_chart_filters on, chart_view keeps the chosen window,
  // order and the range of each window as {window, sort, ranges: {"5h":
  // ..., "7d": ...}}; each window keeps its own range because the two
  // windows offer different ranges. chartView is the page's copy of it.
  // With remember_chart_expanded on, chart_expanded lists the expanded
  // totals by the id of their row without "group:", the provider and the
  // label, as in "claude|Claude"; one list serves both windows. The
  // switches read the settings form, so a change applies before it is
  // saved.
  var chartView = null;

  function rememberChart() {
    var input = $("settings").elements["remember_chart_filters"];
    return !!(input && input.checked);
  }

  function rememberExpanded() {
    var input = $("settings").elements["remember_chart_expanded"];
    return !!(input && input.checked);
  }

  // The chosen window, order and ranges, with the range kept for the other
  // window.
  function currentChartView() {
    var ranges = {}, saved = chartView && chartView.ranges;
    if (saved) Object.keys(saved).forEach(function (kind) { ranges[kind] = saved[kind]; });
    ranges[chartWindow] = range;
    return { window: chartWindow, sort: chartSort, ranges: ranges };
  }

  function saveChartFilters() {
    if (!chartRestored || !rememberChart()) return;
    chartView = currentChartView();
    writePageState("chart_view", { chart_view: chartView });
  }

  // The expanded groups, or null for none, which deletes the key. Groups
  // the chart does not show, such as those of the other window, stay in
  // the list.
  function expandedGroups() {
    var names = Object.keys(chartExpanded).filter(function (id) {
      return chartExpanded[id] && id.indexOf("group:") === 0;
    }).map(function (id) { return id.slice(6); });
    return names.length ? names : null;
  }

  function saveChartExpanded() {
    if (chartRestored && rememberExpanded()) writePageState("chart_expanded", { chart_expanded: expandedGroups() });
  }

  // Turning a chart switch on saves it with the current state; turning it
  // off deletes both. The switch shows the result like a setting.
  function saveChartSwitch(input) {
    var group = input.name === "remember_chart_filters" ? "chart_view" : "chart_expanded";
    var on = input.checked, patch = {};
    if (group === "chart_view") chartView = on ? currentChartView() : null;
    patch[input.name] = on || null;
    patch[group] = on ? (group === "chart_view" ? chartView : expandedGroups()) : null;
    var say = claimState(saveHost(input), input);
    writePageState(group, patch, function (state, text) {
      if (say(state, text) && state === "saved") input.settingsDirty = false;
    });
  }

  // The remembered range of a window, or its default when nothing valid is saved.
  function rangeFor(kind) {
    var ranges = rememberChart() && chartView ? chartView.ranges : null;
    var saved = ranges && typeof ranges === "object" ? ranges[kind] : null;
    return RANGES[kind].some(function (r) { return r[0] === saved; }) ? saved : DEFAULT_RANGE[kind];
  }

  // Restores the chart switches, chart_view and chart_expanded of cfg, the
  // first valid config, while the app is hidden (see preparePage). The
  // switches are set from cfg too, so they and the state come from one
  // config. A saved window that no account reports, the default one
  // included, is offered while its
  // history has samples in the range (windowKinds), which is not loaded
  // yet: its history is read first, for LOAD_TIMEOUT at most, and the
  // promise returned resolves once the window is chosen: the saved window
  // or, without samples, or when the history cannot be read, the one
  // offered. Returns null when nothing is to be read.
  function restoreChartState(cfg) {
    if (chartRestored) return null;
    chartRestored = true;
    ["remember_chart_filters", "remember_chart_expanded"].forEach(function (name) {
      var input = $("settings").elements[name];
      if (input) input.checked = cfg[name] === true;
    });
    var saved = cfg.chart_view, unknown = null;
    chartView = null;
    if (rememberChart() && saved && typeof saved === "object" && !Array.isArray(saved)) {
      var ranges = {};
      Object.keys(RANGES).forEach(function (kind) {
        var value = saved.ranges && typeof saved.ranges === "object" ? saved.ranges[kind] : null;
        if (RANGES[kind].some(function (r) { return r[0] === value; })) ranges[kind] = value;
      });
      if (windowKinds().indexOf(saved.window) >= 0) chartWindow = saved.window;
      else if (Object.keys(RANGES).indexOf(saved.window) >= 0) unknown = saved.window;
      if (saved.sort === "default" || saved.sort === "low") chartSort = saved.sort;
      chartView = { window: chartWindow, sort: chartSort, ranges: ranges };
      range = rangeFor(chartWindow);
      pressSegment("window-buttons", chartWindow);
      pressSegment("sort-buttons", chartSort);
      renderRangeButtons();
    }
    if (rememberExpanded() && Array.isArray(cfg.chart_expanded)) {
      cfg.chart_expanded.forEach(function (id) {
        if (typeof id === "string") chartExpanded["group:" + id] = true;
      });
    }
    if (!unknown) return null;
    return fetchHistory(unknown, rangeFor(unknown), false, LOAD_TIMEOUT).catch(function () {}).then(function () {
      if (windowKinds().indexOf(unknown) < 0) return;
      chartWindow = chartView.window = unknown;
      range = rangeFor(unknown);
      renderWindowButtons();
      pressSegment("window-buttons", chartWindow);
      renderRangeButtons();
    });
  }

  // History responses by window and range. A chosen chart that is cached is
  // drawn at once and redrawn when the new response arrives. historyLoading
  // is the window and range being loaded for the chosen chart, and the chart
  // is grayed out while it differs from the one drawn. Each loadHistory call
  // draws only its own response, and only while it is the latest call.
  var historyCache = {};
  var historyPending = {};
  var historySeq = 0;
  var historyTicket = 0;
  var historyRange = "";
  var historyLoading = "";
  var historyFailed = "";

  function historyKey(kind, name) { return kind + "|" + name; }

  // Requests a history, or joins the request for it that is under way unless
  // fresh is set; timeout is that of api. The cache keeps the response of
  // the latest request, and the promise resolves to it.
  function fetchHistory(kind, name, fresh, timeout) {
    var key = historyKey(kind, name);
    if (historyPending[key] && !fresh) return historyPending[key].promise;
    var seq = ++historySeq;
    var entry = { seq: seq };
    var done = function () { if (historyPending[key] === entry) delete historyPending[key]; };
    entry.promise = api("GET", API + "/history?range=" + name + "&window=" + kind, undefined, timeout).then(function (data) {
      var cached = historyCache[key];
      if (!cached || cached.seq < seq) historyCache[key] = { seq: seq, data: data };
      done();
      return historyCache[key].data;
    }, function (err) {
      done();
      throw err;
    });
    historyPending[key] = entry;
    return entry.promise;
  }

  function showHistory(kind, name, data) {
    history = data;
    historyWindow = kind;
    historyRange = name;
    historyFailed = "";
    renderChart();
  }

  // Loads the chosen chart; fresh skips a request already under way, for
  // data that an action has just changed. Resolves to true when the chart
  // was drawn from the response.
  function loadHistory(fresh) {
    var kind = chartWindow, name = range, key = historyKey(kind, name), ticket = ++historyTicket;
    historyLoading = key;
    if (historyCache[key]) showHistory(kind, name, historyCache[key].data);
    else if (historyFailed) { historyFailed = ""; renderChart(); }
    else $("chart").classList.add("loading");
    return fetchHistory(kind, name, fresh).then(function (data) {
      if (ticket !== historyTicket) return false;
      historyLoading = "";
      showHistory(kind, name, data);
      return true;
    }).catch(function (err) {
      if (ticket !== historyTicket) return false;
      historyLoading = "";
      if (err.message !== "unauthorized") toast(translate("history_failed", err.message));
      // Without data of the chosen chart, the old chart gives way to the error.
      if (err.message !== "unauthorized" && historyKey(historyWindow, historyRange) !== key) {
        history = null;
        historyFailed = err.message;
      }
      renderChart();
      return false;
    });
  }

  // The status shows a reading the drawn chart lacks: an active query after
  // the last sample of its window, or a remaining that differs from it.
  // Passive readings that repeat the value are not written to the history,
  // so they do not count.
  function historyBehind() {
    var last = {};
    history.series.forEach(function (s) { if (s.points && s.points.length) last[s.group + "|" + s.window] = s.points[s.points.length - 1]; });
    var loaded = new Date(history.to).getTime() / 1000;
    // Without new readings the range still moves on, and an inactive
    // window hides once its last sample leaves it.
    if (Date.now() / 1000 - loaded > Math.max(300, sampleStep())) return true;
    return (status.accounts || []).some(function (a) {
      return (a.groups || []).some(function (g) {
        return (g.windows || []).some(function (w) {
          var seen = parseTime(w.observed_at);
          if (w.kind !== historyWindow || !seen) return false;
          var t = seen.getTime() / 1000, p = last[g.key + "|" + w.id];
          if (!p) return t > loaded;
          return t >= p[0] + 1 && (w.source === "active" || w.remaining !== p[1]);
        });
      });
    });
  }

  // Loads the drawn chart again when the status shows newer readings. While
  // the pointer is over the chart or a sample has the keyboard focus, a
  // redraw would replace the crosshair and tooltip, or the focused element,
  // so the new data waits until the pointer and the focus leave.
  var chartCard = $("chart").parentNode;
  var chartPointer = false;
  var chartBehind = false;

  function refreshHistory() {
    if (!history || !status || historyLoading || historyWindow !== chartWindow || historyRange !== range || !historyBehind()) return;
    var ticket = historyTicket;
    fetchHistory(historyWindow, historyRange, true).then(function (data) {
      if (ticket !== historyTicket || historyWindow !== chartWindow || historyRange !== range) return;
      history = data;
      if (chartPointer || chartCard.contains(document.activeElement)) chartBehind = true;
      else renderChart();
    }).catch(function () { /* tried again after the next status */ });
  }

  // After the first chart loads, the other window's chart is fetched so that
  // switching to it does not wait. Both kinds are fetched, since a kind no
  // account reports any more is offered while its history has samples in
  // the range.
  function prefetchHistory() {
    ["5h", "7d"].forEach(function (kind) {
      var name = rangeFor(kind);
      if (kind !== chartWindow && !historyCache[historyKey(kind, name)]) {
        fetchHistory(kind, name).then(function () { if (status && renderWindowButtons()) loadHistory(); }).catch(function () {});
      }
    });
  }

  // ---- Settings ----

  function getPath(obj, path) {
    return path.split(".").reduce(function (value, part) { return value && typeof value === "object" ? value[part] : undefined; }, obj);
  }

  function setPath(obj, path, value) {
    var parts = path.split(".");
    var target = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!target[parts[i]] || typeof target[parts[i]] !== "object") target[parts[i]] = {};
      target = target[parts[i]];
    }
    target[parts[parts.length - 1]] = value;
  }

  // Quota groups offered as ignition checkboxes: the provider's groups, the
  // groups seen on its accounts and any configured ones, in a stable order.
  function groupChoices(p, selected) {
    var names = p.groups.slice();
    (status.accounts || []).forEach(function (a) {
      if (a.provider !== p.id) return;
      (a.groups || []).forEach(function (g) { if (names.indexOf(g.source_label) < 0) names.push(g.source_label); });
    });
    (selected || []).forEach(function (name) { if (names.indexOf(name) < 0) names.push(name); });
    return names;
  }

  function renderGroupChecks(p, selected) {
    var box = document.querySelector("[data-groups='" + p.id + "']");
    if (!box) return;
    var existing = new Map();
    Array.prototype.forEach.call(box.querySelectorAll("[data-group]"), function (input) { existing.set(input.getAttribute("data-group"), input); });
    clear(box);
    groupChoices(p, selected).forEach(function (name) {
      var input = existing.get(name) || el("input", { type: "checkbox", "data-group": name });
      if (!input.settingsDirty && !input.hasAttribute("aria-invalid")) input.checked = (selected || []).some(function (s) { return s.toLowerCase() === name.toLowerCase(); });
      box.appendChild(el("label", { class: "inline" }, [input, document.createTextNode(name)]));
    });
  }

  function providersWithGroups() {
    return providerList().filter(function (p) { return p.groups && p.groups.length; });
  }

  // Quota groups offered under "Quota groups shown": the provider's groups,
  // the groups seen on its accounts and the hidden ones, in a stable order.
  // hidden lists hidden_groups.
  function shownGroupChoices(p, hidden) {
    var names = (p.groups || []).slice(), prefix = p.id + "|";
    (status.accounts || []).forEach(function (a) {
      if (a.provider !== p.id) return;
      (a.groups || []).forEach(function (g) { if (g.source_label && names.indexOf(g.source_label) < 0) names.push(g.source_label); });
    });
    (hidden || []).forEach(function (id) {
      if (typeof id === "string" && id.indexOf(prefix) === 0 && names.indexOf(id.slice(prefix.length)) < 0) names.push(id.slice(prefix.length));
    });
    return names;
  }

  // One checkbox per quota group of p, checked while the group shows, with
  // the value "<provider>|<source label>" of hidden_groups. A service with
  // a single group shows no checkboxes unless that group is hidden.
  function renderShownGroups(p, hidden) {
    var box = document.querySelector("[data-shown-groups='" + p.id + "']");
    if (!box) return;
    var list = box.querySelector(".checks"), existing = new Map(), anyHidden = false;
    Array.prototype.forEach.call(list.querySelectorAll("[data-shown-group]"), function (input) { existing.set(input.getAttribute("data-shown-group"), input); });
    clear(list);
    var names = shownGroupChoices(p, hidden);
    names.forEach(function (name) {
      var id = p.id + "|" + name;
      var input = existing.get(id) || el("input", { type: "checkbox", "data-shown-group": id });
      if (!input.settingsDirty && !input.hasAttribute("aria-invalid")) input.checked = (hidden || []).indexOf(id) < 0;
      if (!input.checked) anyHidden = true;
      list.appendChild(el("label", { class: "inline" }, [input, document.createTextNode(name)]));
    });
    box.hidden = names.length < 2 && !anyHidden;
  }

  // Refills the checkboxes of renderShownGroups, except those of a service
  // where active, the focused element, is.
  function renderAllShownGroups(effective, active) {
    providerList().forEach(function (p) {
      var box = document.querySelector("[data-shown-groups='" + p.id + "']");
      if (!(box && active && box.contains(active))) renderShownGroups(p, effective.hidden_groups);
    });
  }

  // "UTC+8", "UTC-3.5": the offset of the plugin's time zone.
  function utcText(st) {
    var offset = (Number(st && st.utc_offset_seconds) || 0) / 3600;
    return "UTC" + (offset >= 0 ? "+" : "-") + Math.abs(offset);
  }

  function timezoneText() {
    if (!zoneSet(status)) return translate("tz_note_unset", utcText(status));
    var utc = utcText(status);
    var name = /^UTC[+-]/.test(status.timezone) ? "" : status.timezone;
    return translate("tz_note", name ? name + " (" + utc + ")" : utc, translate("tz_config"));
  }

  function topKey(name) { return name.split(".")[0]; }

  // Effective values come from the status config (defaults applied); saved
  // secrets come from the raw plugin config.
  function effectiveConfig() {
    var effective = JSON.parse(JSON.stringify(status.config || {}));
    effective.bark_url = rawConfig.bark_url || "";
    var webhook = rawConfig.webhook || {};
    effective.webhook = effective.webhook || {};
    effective.webhook.url = webhook.url || "";
    effective.webhook.body = webhook.body || "";
    effective.webhook.headers = webhook.headers || {};
    // Plugins without MCP report neither field.
    if (effective.mcp_enabled === undefined) effective.mcp_enabled = !!rawConfig.mcp_enabled;
    if (effective.web_enabled === undefined) effective.web_enabled = !!rawConfig.web_enabled;
    if (!effective.public_listen) effective.public_listen = rawConfig.public_listen || MCP_LISTEN;
    effective.mcp_public_url = typeof rawConfig.mcp_public_url === "string" ? rawConfig.mcp_public_url : "";
    // The page writes its switches without the plugin applying them, so
    // they come from the saved config, like the state they keep.
    effective.remember_chart_filters = rawConfig.remember_chart_filters === true;
    effective.remember_chart_expanded = rawConfig.remember_chart_expanded === true;
    // The saved time zone and ignition switch come from GET /api/config,
    // which the page reads after each save; a key the file lacks takes the
    // plugin's value from the status, so the page assumes no default.
    effective.timezone = typeof rawConfig.timezone === "string" ? rawConfig.timezone : "";
    var ignition = rawConfig.ignition;
    if (ignition && typeof ignition.enabled === "boolean") {
      effective.ignition = effective.ignition || {};
      effective.ignition.enabled = ignition.enabled;
    }
    return effective;
  }

  function browserTimezone() {
    try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch (err) { return ""; }
  }

  // The IANA names the browser knows, for the datalist of the time zone
  // field; none where the browser cannot list them, leaving the field to
  // typing. The plugin checks the name when it is saved.
  function timezoneOptions() {
    var names = [];
    try { if (typeof Intl.supportedValuesOf === "function") names = Intl.supportedValuesOf("timeZone").slice(); } catch (err) { names = []; }
    if (names.length && names.indexOf("UTC") < 0) names.push("UTC");
    return names;
  }

  // Without a saved time zone the field shows the browser's, marked as not
  // saved. Only the user saves it, by editing the field or with the button
  // in the mark, so the page never writes a time zone on its own. A draft,
  // a failed value or the field being edited stays as it is.
  function renderTimezone() {
    var input = $("settings").elements.timezone;
    var saved = typeof rawConfig.timezone === "string" && rawConfig.timezone.trim() !== "";
    if (!saved && !input.value && !input.settingsDirty && !input.hasAttribute("aria-invalid") && document.activeElement !== input) {
      input.value = browserTimezone();
    }
    $("timezone-unsaved").hidden = saved || !input.value.trim();
  }

  // Fields with data-format edit an object as text: "headers" as one
  // "Name: value" line per header, "json" as JSON, "hours" a list of
  // numbers as "24, 10".
  function formatValue(format, value) {
    if (format === "hours") return Array.isArray(value) ? value.join(", ") : "";
    if (!value || typeof value !== "object" || !Object.keys(value).length) return "";
    if (format === "headers") return Object.keys(value).map(function (name) { return name + ": " + value[name]; }).join("\n");
    return JSON.stringify(value);
  }

  // The lead times of the reset reminders, in hours, by window kind: the
  // range config.Parse keeps them in, and the message for a value outside it.
  var REMINDER_HOURS = {
    five_hour: { min: 0.5, max: 4.5, invalid: "hours_invalid_5h" },
    seven_day: { min: 1, max: 167, invalid: "hours_invalid_7d" }
  };
  var REMINDER_HOURS_MAX = 3;

  // The lead times in the text of an "hours" field, for the window kind
  // named by the last part of name: numbers separated by commas (English or
  // Chinese) or spaces, without duplicates, largest first. Throws with a
  // message for the field when the text is empty, holds anything but numbers
  // in range, or more than REMINDER_HOURS_MAX of them.
  function parseHours(text, name) {
    var range = REMINDER_HOURS[name.split(".").pop()];
    var parts = text.split(/[,，\s]+/).filter(Boolean);
    if (!parts.length) throw new Error(translate("hours_empty"));
    var hours = [];
    parts.forEach(function (part) {
      var value = /^(\d+(\.\d*)?|\.\d+)$/.test(part) ? Number(part) : NaN;
      if (!(value >= range.min && value <= range.max)) throw new Error(translate(range.invalid));
      if (hours.indexOf(value) < 0) hours.push(value);
    });
    if (hours.length > REMINDER_HOURS_MAX) throw new Error(translate("hours_too_many"));
    return hours.sort(function (a, b) { return b - a; });
  }

  // parseValue returns the object for the text of a data-format field named
  // name, null for empty text, or throws with a message for the field.
  function parseValue(format, text, name) {
    text = text.trim();
    if (format === "hours") return parseHours(text, name);
    if (!text) return null;
    if (format === "headers") {
      var headers = {};
      text.split(/\r?\n/).forEach(function (line) {
        if (!line.trim()) return;
        var at = line.indexOf(":");
        var name = at > 0 ? line.slice(0, at).trim() : "";
        if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name)) throw new Error(translate("headers_invalid"));
        headers[name] = line.slice(at + 1).trim();
      });
      return headers;
    }
    var value;
    try { value = JSON.parse(text); } catch (err) { throw new Error(translate("json_invalid")); }
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(translate("json_invalid"));
    return value;
  }

  function fillInput(input, effective) {
    var value = getPath(effective, input.name);
    var format = input.getAttribute("data-format");
    if (format) {
      input.value = formatValue(format, value);
      input.setCustomValidity("");
    } else if (input.type === "checkbox") input.checked = !!value;
    else if (input.type === "radio") input.checked = input.value === value;
    else input.value = value === undefined || value === null ? "" : value;
  }

  // keys, when given, limits the refill to those top-level keys and skips the
  // focused field, so a refill after a save leaves the field being edited alone.
  // The chart switches are not refilled: restoreChartState sets them with
  // the state they keep from the first valid config, and later only the
  // user changes them.
  function fillSettings(keys) {
    var effective = effectiveConfig();
    var active = document.activeElement;
    Array.prototype.forEach.call($("settings").elements, function (input) {
      if (!input.name || /^remember_chart_(filters|expanded)$/.test(input.name)) return;
      if (keys && (!keys[topKey(input.name)] || input === active)) return;
      if (input.settingsDirty || input.hasAttribute("aria-invalid")) return;
      if (input.type === "radio" && saveControls(input).some(function (peer) { return peer.settingsDirty || peer.hasAttribute("aria-invalid"); })) return;
      fillInput(input, effective);
    });
    Array.prototype.forEach.call(document.querySelectorAll(".mode-switch"), function (box) { placeThumb(box, false); });
    providersWithGroups().forEach(function (p) {
      var box = document.querySelector("[data-groups='" + p.id + "']");
      if (!keys || (keys.providers && !(box && box.contains(active)))) {
        renderGroupChecks(p, getPath(effective, "providers." + p.id + ".groups"));
      }
    });
    if (!keys) renderAllShownGroups(effective, null);
    else if (keys.hidden_groups) renderAllShownGroups(effective, active);
    settingsFilled = true;
    renderTimezone();
    $("timezone-note").textContent = timezoneText();
    renderIgnitionOff();
    renderMcp();
  }

  // Segmented controls (.segmented button groups and .mode-switch radio
  // groups): a thumb sits under the selected option. Options differ in width,
  // so the thumb is measured to the option. bounce slides it there and plays
  // the squash animation; any other placement jumps without animating.
  // Code that changes the selection places the thumb itself; thumbObserver
  // places it again whenever a control or one of its options changes size,
  // such as after a language change, when the window is resized, and when a
  // hidden control (the page before the key is entered, the closed settings)
  // becomes visible.
  var thumbObserver = new ResizeObserver(function (entries) {
    entries.forEach(function (entry) {
      var box = entry.target.closest(".segmented, .mode-switch");
      if (box) placeThumb(box, false);
    });
  });

  function selectedOption(box) {
    if (box.classList.contains("mode-switch")) {
      var input = box.querySelector("input:checked");
      return input && input.closest(".mode-option");
    }
    return box.querySelector('.seg[aria-pressed="true"]:not([hidden])');
  }

  function placeThumb(box, bounce) {
    var thumb = box.querySelector(".seg-thumb");
    var option = selectedOption(box);
    if (!option) { thumb.hidden = true; return; }
    if (!option.offsetWidth) return; // not displayed; the observer places it once shown
    if (thumb.hidden) bounce = false; // nothing to slide from
    thumb.hidden = false;
    if (!bounce) thumb.style.transition = "none";
    // Fractional sizes, unlike offsetWidth, so the thumb fills grid columns
    // exactly. A pressed button is scaled around its center; the size is
    // taken without the scale.
    var rect = option.getBoundingClientRect(), outer = box.getBoundingClientRect();
    var scale = parseFloat(getComputedStyle(option).scale) || 1;
    var w = rect.width / scale, h = rect.height / scale;
    thumb.style.setProperty("--thumb-x", (rect.left + rect.width / 2 - w / 2 - outer.left - box.clientLeft) + "px");
    thumb.style.setProperty("--thumb-y", (rect.top + rect.height / 2 - h / 2 - outer.top - box.clientTop) + "px");
    thumb.style.setProperty("--thumb-w", w + "px");
    thumb.style.setProperty("--thumb-h", h + "px");
    if (bounce) thumb.classList.remove("bounce");
    void thumb.offsetWidth; // apply the jump before the transition returns, or restart the animation
    if (bounce) thumb.classList.add("bounce");
    else thumb.style.transition = "";
  }

  // Observes the options of a control; called again after they are rebuilt.
  function observeOptions(box) {
    Array.prototype.forEach.call(box.querySelectorAll(".seg, .mode-option"), function (option) {
      thumbObserver.observe(option);
    });
  }

  Array.prototype.forEach.call(document.querySelectorAll(".segmented, .mode-switch"), function (box) {
    // Dropping bounce once it has played keeps the squash from replaying
    // when the thumb is displayed again.
    var thumb = el("span", { class: "seg-thumb", "aria-hidden": "true", hidden: true });
    thumb.addEventListener("animationend", function () { thumb.classList.remove("bounce"); });
    box.insertBefore(thumb, box.firstChild);
    thumbObserver.observe(box);
    observeOptions(box);
  });
  Array.prototype.forEach.call(document.querySelectorAll(".mode-switch"), function (box) {
    box.addEventListener("change", function () { placeThumb(box, true); });
  });

  // The top-level config key a settings input belongs to.
  function inputTop(input) {
    if (input.hasAttribute && input.hasAttribute("data-group")) return "providers";
    if (input.hasAttribute && input.hasAttribute("data-shown-group")) return "hidden_groups";
    return input.name ? topKey(input.name) : "";
  }

  // The values of the inputs of some top-level keys as the user left them,
  // taken when the change is made. A save that waits in the queue builds its
  // patch from these, so a refill of the fields in the meantime does not
  // change what it saves.
  function captureIntent(tops) {
    var intent = { inputs: [], states: [], submitted: [] };
    Array.prototype.forEach.call($("settings").elements, function (input) {
      if (tops.indexOf(inputTop(input)) < 0) return;
      intent.inputs.push(input);
      intent.states.push({ value: input.value, checked: input.checked, edit: input.settingsEdit || 0 });
    });
    return intent;
  }

  // The patch for one top-level key. PATCH /api/config replaces top-level
  // keys whole, so
  // objects are sent whole, starting from the saved object to keep keys the
  // page does not show. intent, from captureIntent, holds the values to
  // save; without it the inputs are read as they are now.
  function buildPatch(top, intent) {
    var patch = {};
    if (intent && !intent.submitted) intent.submitted = [];
    var base = rawConfig[top];
    var state = function (input) {
      var i = intent ? intent.inputs.indexOf(input) : -1;
      return i >= 0 ? intent.states[i] : { value: input.value, checked: input.checked };
    };
    // Without a saved object, start from the effective one, so a field whose
    // text does not parse keeps the value the plugin uses.
    if ((top === "ignition" || top === "reset_reminder_hours") && !base) base = status.config && status.config[top];
    if (base && typeof base === "object") patch[top] = JSON.parse(JSON.stringify(base));
    Array.prototype.forEach.call($("settings").elements, function (input) {
      if (!input.name || topKey(input.name) !== top) return;
      var value, saved = state(input);
      var format = input.getAttribute("data-format");
      if (format) {
        // Text that does not parse keeps the saved value.
        try { value = parseValue(format, saved.value, input.name); } catch (err) { return; }
        if (value === null) {
          var parts = input.name.split(".");
          var parent = getPath(patch, parts.slice(0, -1).join("."));
          if (parent) delete parent[parts[parts.length - 1]];
          if (intent) intent.submitted.push(input);
          return;
        }
      } else if (input.type === "checkbox") value = saved.checked;
      else if (input.type === "radio") {
        if (!saved.checked) return;
        value = saved.value;
        if (intent) intent.inputs.forEach(function (peer) { if (peer !== input && peer.name === input.name) intent.submitted.push(peer); });
      } else if (input.type === "number") {
        if (saved.value === "") return;
        value = Number(saved.value);
      } else value = saved.value.trim();
      setPath(patch, input.name, value);
      if (intent) intent.submitted.push(input);
    });
    if (top === "providers") {
      if (!patch.providers) patch.providers = {};
      providerList().forEach(function (p) { if (!patch.providers[p.id]) patch.providers[p.id] = {}; });
      providersWithGroups().forEach(function (p) {
        patch.providers[p.id].groups = Array.prototype.filter.call(
          document.querySelectorAll("[data-groups='" + p.id + "'] [data-group]"), function (input) {
            if (intent) intent.submitted.push(input);
            return state(input).checked;
          }
        ).map(function (input) { return input.getAttribute("data-group"); });
      });
    }
    // hidden_groups lists the unchecked groups and keeps the hidden groups
    // the page does not offer; an empty list deletes the key. The saved list
    // is read as the plugin reads it, so a checkbox removes an entry written
    // with spaces or twice. A save that waits in the queue takes the
    // checkboxes of its intent: one may have left the form meanwhile, when
    // the only account that reports its group went.
    if (top === "hidden_groups") {
      var hidden = hiddenList(base);
      Array.prototype.forEach.call(intent ? intent.inputs : $("settings").elements, function (input) {
        if (!input.hasAttribute || !input.hasAttribute("data-shown-group")) return;
        var id = input.getAttribute("data-shown-group"), at = hidden.indexOf(id);
        if (intent) intent.submitted.push(input);
        if (state(input).checked) { if (at >= 0) hidden.splice(at, 1); }
        else if (at < 0) hidden.push(id);
      });
      patch[top] = hidden.length ? hidden : null;
    }
    // Turning MCP on without a key also saves a new key, since the plugin
    // does not listen without one.
    if (top === "mcp_enabled" && patch.mcp_enabled && !mcpKey().trim()) patch.mcp_key = newMcpKey();
    // Likewise the page and API need an admin key on the public listener.
    if (top === "web_enabled" && patch.web_enabled && !adminKey().trim()) patch.admin_key = newAdminKey();
    // Save the same URL and listen defaults that config.Parse applies, so
    // status comparisons do not wait on a trailing slash or an empty field.
    if (top === "bark_url") patch[top] = patch[top].replace(/\/+$/, "");
    if (top === "public_listen" && !patch[top]) patch[top] = "127.0.0.1:8318";
    // An empty public address deletes the key.
    if (top === "mcp_public_url" && !patch[top]) patch[top] = null;
    return patch;
  }

  // Settings save serially. Each item owns its result and draft; section
  // headings and advanced legends summarize progress without hiding failures.
  var saveQueue = Promise.resolve();
  var saveSerial = 0;

  function saveHost(input) {
    var group = input.closest("fieldset") || input.closest(".settings-section");
    return group && group.querySelector(group.tagName === "FIELDSET" ? "legend" : ".card-title");
  }

  function saveControls(input) {
    var group = input.closest(".thresholds, .mode-switch");
    if (group) return Array.prototype.slice.call(group.querySelectorAll("input"));
    return /^(INPUT|SELECT|TEXTAREA|BUTTON)$/.test(input.tagName) ? [input] : [];
  }

  function saveItemKey(input) {
    var kind = input.getAttribute("data-account-field");
    if (kind) return input.closest(".account-setting").getAttribute("data-account") + ":" + kind;
    if (input.closest(".thresholds")) return "thresholds";
    if (input.hasAttribute("data-group")) return input.closest("[data-groups]").getAttribute("data-groups") + ":" + input.getAttribute("data-group");
    if (input.hasAttribute("data-shown-group")) return "shown:" + input.getAttribute("data-shown-group");
    return input.name || input.id;
  }

  function saveItemLabel(input) {
    if (input.id === "account-order-state") return translate("save_order");
    var kind = input.getAttribute("data-account-field");
    if (kind) return translate(kind === "name" ? "rename" : "weight");
    if (input.id === "mcp-key-new") return translate("mcp_key");
    if (input.id === "admin-key-new") return translate("admin_key");
    if (input.closest(".thresholds")) return translate("thresholds");
    var mode = input.closest(".mode-switch");
    if (mode) return mode.getAttribute("aria-label");
    var service = input.getAttribute("data-service");
    if (service) return providerTitle(service) + " · " + translate(input.getAttribute("data-label") || "col_ignition_model", providerTitle(service));
    if (input.hasAttribute("data-group")) return providerTitle(input.closest("[data-groups]").getAttribute("data-groups")) + " · " + input.getAttribute("data-group");
    if (input.hasAttribute("data-shown-group")) {
      var shown = input.getAttribute("data-shown-group"), at = shown.indexOf("|");
      return providerTitle(shown.slice(0, at)) + " · " + shown.slice(at + 1);
    }
    var label = input.closest("label"), text = label && label.querySelector("span");
    return text ? text.textContent : input.getAttribute("aria-label") || input.name || input.id;
  }

  // key, when given, names an item of its own that shares the control, such
  // as a page state saved for a switch (see pageStateSaved).
  function saveEntry(host, input, key) {
    if (!host) return null;
    input = input || host;
    key = key || saveItemKey(input);
    if (!host.saveEntries) host.saveEntries = new Map();
    var entry = host.saveEntries.get(key);
    if (!entry) {
      entry = { host: host };
      host.saveEntries.set(key, entry);
    }
    var old = entry.input;
    // Results outlive their DOM binding. Recreated account fields inherit
    // drafts and pending counts; queued completions use the current binding.
    if (old && old !== input && (old.accountDirty || old.accountPending || entry.failure) && input.hasAttribute("data-account-field")) {
      ["value", "accountSaved", "accountEdit", "accountDirty", "accountPending"].forEach(function (name) { input[name] = old[name]; });
    }
    entry.input = input;
    entry.controls = saveControls(input);
    entry.controls.forEach(function (control) {
      if (entry.failure) control.setAttribute("aria-invalid", "true");
      else control.removeAttribute("aria-invalid");
    });
    return entry;
  }

  // Re-render local status messages in the selected language; upstream text
  // and browser validation messages remain verbatim.
  function saveText(text) {
    var keys = ["save_saved", "save_saving", "save_waiting", "save_applied", "save_unconfirmed",
      "mcp_error", "public_error", "weight_invalid",
      "thresholds_order", "headers_invalid", "json_invalid", "mcp_public_url_invalid",
      "hours_empty", "hours_too_many", "hours_invalid_5h", "hours_invalid_7d"];
    for (var i = 0; i < keys.length; i++) {
      var key = keys[i], languages = Object.keys(I18N);
      for (var j = 0; j < languages.length; j++) {
        var template = I18N[languages[j]][key], at = template.indexOf("{0}");
        if (at < 0 && text === template) return translate(key);
        if (at >= 0) {
          var before = template.slice(0, at), after = template.slice(at + 3);
          if (text.slice(0, before.length) === before && text.slice(text.length - after.length) === after) {
            return translate(key, text.slice(before.length, text.length - after.length));
          }
        }
      }
    }
    return text;
  }

  function renderSaveState(host) {
    if (!host || !host.saveEntries) return;
    var entries = Array.from(host.saveEntries.values()), failures = entries.filter(function (entry) { return entry.failure; });
    var latest = entries.filter(function (entry) { return entry.state; }).sort(function (a, b) { return b.order - a.order; })[0];
    failures.sort(function (a, b) { return b.failure.order - a.failure.order; });
    var failed = failures[0], state = failed ? "failed" : latest ? latest.state : "";
    var text = failed ? translate("save_field_failed", saveItemLabel(failed.input)) : latest ? saveText(latest.text) : "";
    // Items can share a control, which stays marked while any of them failed.
    var invalid = new Set();
    failures.forEach(function (entry) { entry.controls.forEach(function (control) { invalid.add(control); }); });
    entries.forEach(function (entry) {
      entry.controls.forEach(function (control) {
        if (invalid.has(control)) control.setAttribute("aria-invalid", "true");
        else control.removeAttribute("aria-invalid");
      });
    });
    var node = host.querySelector(".save-state");
    if (!node) { node = el("span", { class: "save-state", role: "status" }); host.appendChild(node); }
    node.setAttribute("data-state", state);
    clear(node);
    if (text) {
      if (failed) {
        var compact = el("span", { "data-save-part": "compact" });
        compact.textContent = translate("save_failed_compact");
        node.appendChild(compact);
      }
      var message = el("span", { "data-save-part": failed ? "summary" : "progress" });
      if (host.id === "account-order-state") maskedNodes(text).forEach(function (part) { message.appendChild(part); });
      else message.textContent = text;
      node.appendChild(message);
      if (failed) {
        var reason = el("span", { "data-save-part": "reason" });
        var detail = translate("save_failure_reason", saveText(failed.failure.text));
        if (host.id === "account-order-state") maskedNodes(detail).forEach(function (part) { reason.appendChild(part); });
        else reason.textContent = detail;
        node.appendChild(reason);
      }
      if (failures.length > 1) {
        var count = el("span", { "data-save-part": "count" });
        count.textContent = translate("save_more_failed", failures.length - 1);
        node.appendChild(count);
      }
    }
  }

  function renderSaveStates() {
    Array.prototype.forEach.call(document.querySelectorAll(".settings-section > .card-title, fieldset > legend"), renderSaveState);
  }

  function updateSaveEntry(entry, state, text) {
    entry.state = state;
    entry.text = text;
    if (state === "failed") entry.failure = { text: text, order: ++saveSerial };
    else if (state === "waiting" || state === "saved" || state === "verified" || state === "unverified") entry.failure = null;
    renderSaveState(entry.host);
  }

  function setSaveState(host, state, text, input) {
    var entry = saveEntry(host, input);
    if (!entry) return null;
    entry.owner = null;
    entry.order = ++saveSerial;
    updateSaveEntry(entry, state, text);
    return entry;
  }

  // Edits invalidate late replies without clearing an unresolved failure.
  function editSaveState(host, input) {
    var entry = saveEntry(host, input);
    if (!entry) return;
    entry.owner = null;
    if (!entry.failure) entry.state = "";
    renderSaveState(host);
  }

  var THRESHOLDS = ["notice_threshold", "low_threshold", "critical_threshold"];

  // The three thresholds must not increase from first to third. They save
  // together, so a value held back by the order saves once the order holds.
  function thresholdsInOrder() {
    var form = $("settings");
    var values = THRESHOLDS.map(function (name) { return form.elements[name].value === "" ? NaN : Number(form.elements[name].value); });
    return values.every(isFinite) && values[0] >= values[1] && values[1] >= values[2];
  }

  // Each control owns an edit generation, including changes made while a
  // different control of the same top-level key is awaiting confirmation.
  function noteEdit(input) {
    var controls = input.type === "radio" ? saveControls(input) : [input];
    controls.forEach(function (control) { control.settingsEdit = (control.settingsEdit || 0) + 1; });
  }

  // Clear only drafts actually submitted by this save and unchanged since
  // its intent was captured. Independent failures remain owned by their item.
  function refillSaved(intent) {
    var keys = {}, any = false;
    intent.submitted.forEach(function (input) {
      var saved = intent.states[intent.inputs.indexOf(input)];
      if (!saved || (input.settingsEdit || 0) !== saved.edit || input.hasAttribute("aria-invalid")) return;
      input.settingsDirty = false;
      keys[inputTop(input)] = true;
      any = true;
    });
    if (any) fillSettings(keys);
    if (keys.ignition) renderIgnitionOff();
  }

  function saveField(event) {
    var input = event.target;
    var top = inputTop(input);
    // The chart switches save with their state; see saveChartSwitch.
    if (!top || top === "remember_chart_filters" || top === "remember_chart_expanded") return;
    var host = saveHost(input);
    var tops = [top];
    // Browsers fill the password saved for this site, usually the admin key,
    // into password fields. A value the browser fills in while the field is
    // not focused comes from autofill on page load; it is not saved, and the
    // field shows the saved value again.
    if (input.type === "password") {
      var value = input.value.trim();
      if (autofilled(input) && document.activeElement !== input) {
        fillInput(input, effectiveConfig());
        return;
      }
    }
    if (input.type === "number") {
      if (!input.checkValidity()) {
        setSaveState(host, "failed", input.validationMessage, input);
        input.reportValidity();
        return;
      }
      if (input.value === "") { fillInput(input, effectiveConfig()); return; }
    }
    if (THRESHOLDS.indexOf(top) >= 0) {
      if (!thresholdsInOrder()) { setSaveState(host, "failed", translate("thresholds_order"), input); return; }
      tops = THRESHOLDS;
    }
    var format = input.getAttribute("data-format");
    if (format) {
      try {
        parseValue(format, input.value, input.name);
        input.setCustomValidity("");
      } catch (err) {
        input.setCustomValidity(err.message);
        input.reportValidity();
        setSaveState(host, "failed", err.message, input);
        return;
      }
    }
    if (top === "mcp_public_url" && publicUrl(input.value) === null) {
      setSaveState(host, "failed", translate("mcp_public_url_invalid"), input);
      return;
    }
    if (input.name === "ignition.enabled") renderIgnitionOff();
    var say = claimState(host, input);
    noteEdit(input);
    var intent = captureIntent(tops);
    saveQueue = saveQueue.then(function () {
      var patch = {};
      tops.forEach(function (name) {
        var part = buildPatch(name, intent);
        Object.keys(part).forEach(function (k) { patch[k] = part[k]; });
      });
      return patchConfig(patch).then(function (token) {
        // Once applied, the fields show the values the plugin uses.
        showApplied(say, token, /^(mcp_enabled|mcp_key|public_listen|web_enabled|admin_key)$/.test(top)).then(function (applied) { if (applied) refillSaved(intent); });
        return rereadConfig();
      }, function (err) {
        say("failed", err.message);
      });
    });
  }

  // ---- Applied config ----

  // Every config write of the page carries a new config_token. The plugin
  // reports, as config_applied_token in the status, the token of the config
  // it has applied, MCP included. A save counts as applied when that token
  // is its own or one this page saved after it, since a later config
  // contains the earlier change. savedTokens lists the tokens of this
  // page's saves, oldest first.
  var savedTokens = [];
  // Saves still waiting to be applied: {token, since, done}.
  var applyWaits = [];
  var applyTimer = null;
  var APPLY_TIMEOUT = 15000;

  // 16 random hexadecimal digits.
  function newConfigToken() {
    var bytes = new Uint8Array(8);
    crypto.getRandomValues(bytes);
    var text = "";
    for (var i = 0; i < bytes.length; i++) text += (bytes[i] < 16 ? "0" : "") + bytes[i].toString(16);
    return text;
  }

  // Whether the applied token covers the save with token: it is that save
  // or a later one of this page.
  function tokenApplied(tokens, token, applied) {
    var saved = tokens.indexOf(token);
    return saved >= 0 && !!applied && tokens.indexOf(applied) >= saved;
  }

  // The keys of the page's own state, the same as config.IsPageState: the
  // plugin saves a patch of only these keys without applying it.
  var PAGE_STATE_KEYS = ["remember_chart_filters", "remember_chart_expanded", "chart_view", "chart_expanded",
    "mask_accounts", "events_show_5h_resets", "mcp_public_url"];

  // Writes a patch of the plugin config with a new token, and resolves to
  // the token once it is saved. A patch of only page state goes without a
  // token, which the plugin would not apply, and resolves to "".
  // Before it resolves, rawConfig takes the saved values of the patched keys
  // (keepSaved), so a field refilled after the save shows what was saved.
  function patchConfig(patch) {
    var token = newConfigToken(), body = {}, names = Object.keys(patch);
    names.forEach(function (name) { body[name] = patch[name]; });
    if (names.length && names.every(function (name) { return PAGE_STATE_KEYS.indexOf(name) >= 0; })) {
      return api("PATCH", CONFIG_API, body).then(function (saved) { keepSaved(names, body, saved); return ""; });
    }
    body.config_token = token;
    return api("PATCH", CONFIG_API, body).then(function (saved) {
      keepSaved(names, body, saved);
      savedTokens.push(token);
      if (savedTokens.length > 200) savedTokens.shift();
      return token;
    });
  }

  // Copies the keys names of a saved patch into rawConfig: from saved, the
  // config the plugin answers with, which holds them as it saved them
  // (trimmed, or dropped like the state of a remember switch that is off),
  // or from sent when the answer is not a config. Only these keys change, so
  // an answer that arrives after a later one leaves the other keys alone.
  function keepSaved(names, sent, saved) {
    var from = saved && typeof saved === "object" && !Array.isArray(saved) ? saved : sent;
    names.forEach(function (name) {
      var value = from[name];
      if (value === undefined || value === null) delete rawConfig[name];
      else rawConfig[name] = value;
    });
    if (pendingOrder) rawConfig.account_order = pendingOrder.slice();
  }

  function lastToken() { return savedTokens.length ? savedTokens[savedTokens.length - 1] : ""; }

  // Resolves to true once the plugin has applied the save with token, or
  // to false at APPLY_TIMEOUT. The deadline is a timer of its own, so a
  // status request that hangs does not delay it, and a status that reports
  // the token after it no longer counts. The status is read every second
  // while a save waits.
  function whenApplied(token) {
    if (tokenApplied(savedTokens, token, status && status.config_applied_token)) return Promise.resolve(true);
    return new Promise(function (resolve) {
      var wait = { token: token };
      wait.done = function (applied) {
        clearTimeout(wait.timer);
        applyWaits = applyWaits.filter(function (w) { return w !== wait; });
        resolve(applied);
      };
      wait.timer = setTimeout(function () { wait.done(false); }, APPLY_TIMEOUT);
      applyWaits.push(wait);
      pollApplied();
    });
  }

  // Settles the waiting saves that the status shows as applied. Called with
  // every status.
  function checkApplied() {
    var applied = status && status.config_applied_token;
    applyWaits.slice().forEach(function (w) {
      if (tokenApplied(savedTokens, w.token, applied)) w.done(true);
    });
  }

  // A status request of the poll gives up after this long, so the next one
  // can start.
  var POLL_TIMEOUT = 4000;

  function pollApplied() {
    if (applyTimer || !applyWaits.length) return;
    applyTimer = setTimeout(function () {
      loadStatus(POLL_TIMEOUT).catch(function () {}).then(function () {
        applyTimer = null;
        checkApplied();
        pollApplied();
      });
    }, 1000);
  }

  // Ownership is per item, so a late reply cannot label a newer edit and a
  // different item's success cannot clear this item's failure.
  function claimState(host, input) {
    var entry = setSaveState(host, "saving", translate("save_saving"), input), owner = {};
    if (entry) entry.owner = owner;
    return function (state, text) {
      if (!entry || entry.owner !== owner) return false;
      updateSaveEntry(entry, state, text);
      return true;
    };
  }

  // The save state of a field after it is saved, through say from
  // claimState: waiting until the plugin applies it, then applied, or
  // unconfirmed after the timeout. A change of MCP settings shows the MCP
  // error instead when the endpoint does not start. The save waits for its
  // token even when a newer save owns the field's state.
  // listener is set for the settings of the public listener, whose error
  // after the save is a failure.
  // A save without a token, of page state only, is done once it is saved.
  function showApplied(say, token, listener) {
    if (!token) {
      say("saved", translate("save_saved"));
      return Promise.resolve(true);
    }
    say("waiting", translate("save_waiting"));
    return whenApplied(token).then(function (applied) {
      var pub = status && status.public, error = "";
      if (listener && pub) error = !pub.running && pub.error;
      else if (listener) error = status && status.mcp && status.mcp.enabled && status.mcp.error;
      if (!applied) say("unverified", translate("save_unconfirmed"));
      else if (error) say("failed", translate(pub ? "public_error" : "mcp_error", error));
      else say("saved", translate("save_applied"));
      return applied;
    });
  }

  // ---- Ignition and MCP settings ----

  // With ignition off, its settings stay editable but are grayed out, with
  // a note that they do not apply.
  function renderIgnitionOff() {
    var input = $("settings").elements["ignition.enabled"];
    var off = !(input && input.checked);
    $("ignition-params").classList.toggle("off", off);
    $("ignition-off").hidden = !off;
  }

  var MCP_LISTEN = "127.0.0.1:8318";
  var mcpKeyShown = false;

  var adminKeyShown = false;

  // 32 random bytes in base64url after the prefix.
  function newSecret(prefix) {
    var bytes = new Uint8Array(32);
    crypto.getRandomValues(bytes);
    var text = "";
    for (var i = 0; i < bytes.length; i++) text += String.fromCharCode(bytes[i]);
    return prefix + btoa(text).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function newMcpKey() { return newSecret("llmcp_"); }

  function newAdminKey() { return newSecret("lladm_"); }

  function mcpKey() { return typeof rawConfig.mcp_key === "string" ? rawConfig.mcp_key : ""; }

  function adminKey() { return typeof rawConfig.admin_key === "string" ? rawConfig.admin_key : ""; }

  // The public listener's state: status.public, else the MCP state, which
  // shares the listener, with what it serves unknown.
  function publicState(st) {
    if (!st) return null;
    if (st.public) return st.public;
    if (!st.mcp) return null;
    return { listen: st.mcp.listen || "", running: !!st.mcp.running, error: st.mcp.error || "", web: null, mcp: !!st.mcp.running };
  }

  // The admin key and the state of the public listener.
  function renderPublic() {
    if (!$("admin-key-view")) return;
    var secret = adminKey();
    var view = $("admin-key-view");
    view.textContent = secret ? (adminKeyShown ? secret : maskedKey(secret)) : translate("mcp_key_unset");
    view.classList.toggle("muted", !secret);
    $("admin-key-show").hidden = !secret;
    $("admin-key-show").textContent = translate(adminKeyShown ? "mcp_hide" : "mcp_show");
    $("admin-key-copy").hidden = !secret;

    var box = $("public-status");
    clear(box);
    var state = publicState(status);
    if (!state) box.appendChild(badge("muted", translate("mcp_unknown")));
    else if (state.running) {
      box.appendChild(badge("success", translate("mcp_running")));
      var serves = [];
      if (state.web) serves.push(translate("public_web"));
      if (state.mcp) serves.push(translate("public_mcp"));
      box.appendChild(el("span", { class: "mono", text: state.listen || "" }));
      if (serves.length) box.appendChild(el("span", { text: serves.join(" · ") }));
    } else {
      box.appendChild(badge(state.error ? "failure" : "muted", translate(state.error ? "mcp_stopped" : "public_off")));
      if (state.error) box.appendChild(el("span", { class: "error-text", text: state.error }));
    }
  }

  function maskedKey(value) { return value ? value.slice(0, 6) + "••••••••••••" + value.slice(-4) : ""; }

  // mcp_public_url as clients use it: https:// in front of an address
  // without a scheme. "" when value is empty, null when it is not an http or
  // https address. PUBLIC_URL_FORM is the expression of
  // config.MCPPublicURLValid, and the ends are trimmed of the white space strings.TrimSpace trims, so
  // the page and the plugin accept the same addresses; both are tested with
  // internal/config/testdata/public_urls.json.
  var PUBLIC_URL_TRIM = /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g;
  var PUBLIC_URL_FORM = /^[Hh][Tt][Tt][Pp][Ss]?:\/\/[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*(?::([0-9]{1,5}))?(\/[\x21\x22\x24-\x3e\x40-\x5b\x5d-\x7e]*)?$/;
  function publicUrl(value) {
    var text = String(value || "").replace(PUBLIC_URL_TRIM, "");
    if (!text) return "";
    var full = /^[A-Za-z][A-Za-z0-9+.-]*:\/\//.test(text) ? text : "https://" + text;
    var m = PUBLIC_URL_FORM.exec(full);
    return m && !(m[1] && Number(m[1]) > 65535) ? full : null;
  }

  // The address clients use, without the path /mcp: the public address in
  // the field (mcp_public_url, as typed, before it is saved) while it is
  // valid, with /mcp and trailing slashes taken off its path, or http:// and
  // the listen address. A listen address without a host or on all interfaces is
  // reached through the host of this page.
  function mcpBaseUrl() {
    var field = $("settings").elements["mcp_public_url"];
    var stored = publicUrl(field && field.value);
    if (stored) {
      var path = PUBLIC_URL_FORM.exec(stored)[2] || "";
      return stored.slice(0, stored.length - path.length) + path.replace(/\/+$/, "").replace(/\/mcp$/, "");
    }
    var input = $("settings").elements["public_listen"];
    var state = publicState(status);
    var listen = (input && input.value.trim()) || (state && state.listen) || MCP_LISTEN;
    var at = listen.lastIndexOf(":");
    var host = at >= 0 ? listen.slice(0, at) : listen, port = at >= 0 ? listen.slice(at) : "";
    if (!host || host === "0.0.0.0" || host === "[::]" || host === "::") host = location.hostname || "127.0.0.1";
    return "http://" + host + port;
  }

  // A POSIX shell word for text: single quotes, with ' written as '\''.
  function shellQuote(text) {
    return "'" + String(text).replace(/'/g, "'\\''") + "'";
  }

  // The commands quote the address and the header for the shell; the JSON
  // is built by JSON.stringify.
  function mcpSnippets(url, secret) {
    var auth = shellQuote("Authorization: Bearer " + secret);
    return [
      ["mcp_claude", "claude mcp add --transport http lamplighter " + shellQuote(url) + " --header " + auth],
      ["mcp_json", JSON.stringify({ mcpServers: { lamplighter: { type: "http", url: url, headers: { Authorization: "Bearer " + secret } } } }, null, 2)],
      ["mcp_stdio", "npx mcp-remote " + shellQuote(url) + " --header " + auth]
    ];
  }

  // The key, the state the plugin reports and the client configs. The
  // configs show the key masked until it is shown; copying copies it whole.
  function renderMcp() {
    if (!$("mcp-key-view")) return;
    var secret = mcpKey();
    var view = $("mcp-key-view");
    view.textContent = secret ? (mcpKeyShown ? secret : maskedKey(secret)) : translate("mcp_key_unset");
    view.classList.toggle("muted", !secret);
    $("mcp-key-show").hidden = !secret;
    $("mcp-key-show").textContent = translate(mcpKeyShown ? "mcp_hide" : "mcp_show");
    $("mcp-key-copy").hidden = !secret;

    var box = $("mcp-status");
    clear(box);
    var mcp = status && status.mcp;
    if (!mcp) box.appendChild(badge("muted", translate("mcp_unknown")));
    else if (mcp.running) {
      box.appendChild(badge("success", translate("mcp_running")));
      box.appendChild(el("span", { class: "mono", text: (mcp.listen || "") + (mcp.path || "/mcp") }));
    } else {
      box.appendChild(badge(mcp.enabled && mcp.error ? "failure" : "muted", translate("mcp_stopped")));
      if (mcp.error) box.appendChild(el("span", { class: "error-text", text: mcp.error }));
    }

    var url = mcpBaseUrl() + "/mcp";
    var snippets = $("mcp-snippets");
    clear(snippets);
    var shown = secret ? (mcpKeyShown ? secret : maskedKey(secret)) : "<key>";
    var full = mcpSnippets(url, secret || "<key>");
    mcpSnippets(url, shown).forEach(function (s, i) {
      var copy = el("button", { type: "button", class: "btn btn-ghost btn-sm", text: translate("copy"), onclick: function () { copyText(full[i][1], copy); } });
      snippets.appendChild(el("div", { class: "snippet" }, [
        el("div", { class: "snippet-head" }, [el("span", { class: "field-label", text: translate(s[0]) }), copy]),
        el("pre", { class: "mono" }, [el("code", { text: s[1] })])
      ]));
    });
    if (!secret) snippets.appendChild(el("p", { class: "hint", text: translate("mcp_no_key") }));
    renderPublic();
  }

  // Copies text, through a selected text area where the Clipboard API is
  // not available, as in an iframe without the permission.
  function copyText(text, button) {
    var done = function () { toast(translate("copied")); };
    var fallback = function () {
      var area = el("textarea", { class: "copy-buffer", readonly: true });
      area.value = text;
      document.body.appendChild(area);
      area.select();
      var ok = false;
      try { ok = document.execCommand("copy"); } catch (err) {}
      document.body.removeChild(area);
      if (ok) done(); else toast(translate(SANDBOXED ? "copy_failed_sandbox" : "copy_failed"));
    };
    if (!SANDBOXED && navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, fallback);
    else fallback();
  }

  // A new key replaces the old one at once; replacing a key asks first.
  function generateMcpKey() {
    var button = $("mcp-key-new");
    if (mcpKey() && !confirmed(button, translate("mcp_regenerate_confirm"))) return;
    var say = claimState(saveHost(button), button), secret = newMcpKey();
    saveQueue = saveQueue.then(function () {
      return patchConfig({ mcp_key: secret }).then(function (token) {
        showApplied(say, token, true);
        return rereadConfig();
      }, function (err) {
        say("failed", err.message);
      });
    });
  }

  // A page opened through the public listener sends the admin key; after
  // replacing it, the page sends the new one.
  function generateAdminKey() {
    var button = $("admin-key-new");
    if (adminKey() && !confirmed(button, translate("admin_regenerate_confirm"))) return;
    var say = claimState(saveHost(button), button), secret = newAdminKey();
    saveQueue = saveQueue.then(function () {
      return patchConfig({ admin_key: secret }).then(function (token) {
        useKey(secret);
        showApplied(say, token, true);
        return rereadConfig();
      }, function (err) {
        say("failed", err.message);
      });
    });
  }

  // autofilled reports whether the browser filled in the value. Firefox and
  // recent Chrome know :autofill; Safari and older Chrome only the prefixed
  // name.
  function autofilled(input) {
    try { if (input.matches(":autofill")) return true; } catch (err) {}
    try { return input.matches(":-webkit-autofill"); } catch (err) { return false; }
  }

  // One line per channel; a webhook also shows the start of its response and,
  // without a success condition, that only the HTTP status was checked.
  function renderTestResults(results) {
    var box = $("test-results");
    clear(box);
    (results || []).forEach(function (r) {
      var children = [el("span", { text: r.ok ? translate("test_ok", r.channel) : translate("test_failed", r.channel, r.error) })];
      if (r.ok && r.response) children.push(el("span", { class: "hint", text: translate("test_response", r.response) }));
      if (r.ok && r.unchecked) children.push(el("span", { class: "hint", text: translate("test_unchecked") }));
      box.appendChild(el("div", { class: "test-result " + (r.ok ? "ok" : "failed") }, children));
    });
  }

  // Resolves to the config read, which must be an object: an answer that
  // is not, such as the page of a proxy, fails like a request.
  function fetchConfig(timeout, signal) {
    return api("GET", CONFIG_API, undefined, timeout, signal).then(function (data) {
      if (Array.isArray(data)) throw new Error(translate("bad_response"));
      return data;
    });
  }

  function useConfig(data) {
    rawConfig = data;
    if (pendingOrder) rawConfig.account_order = pendingOrder.slice();
    renderMcp();
    if (settingsFilled) renderTimezone();
  }

  function loadConfig(timeout) {
    return fetchConfig(timeout).then(function (data) {
      useConfig(data);
      return data;
    });
  }

  // Reads the config again after a save. The save stands when this read
  // fails: rawConfig already holds what was saved (see patchConfig), and
  // the next read catches up.
  function rereadConfig() {
    return loadConfig().catch(function () {});
  }

  // Opening the page also checks for a newer release, after the first status
  // so that this status does not replace the result. The plugin reads GitHub
  // at most once a minute; failures stay silent like the periodic check.
  function checkUpdate() {
    api("POST", API + "/check-update", {}).then(function (data) {
      if (data.status) { status = data.status; renderStatus(); }
    }).catch(function () {});
  }

  // ---- Startup ----

  // The app shows once a valid config has set the masking and the chart
  // state is restored (contentReady); until then a card says the page is
  // loading or why it failed, with a button to try again. The card shows
  // only after LOAD_CARD_DELAY, so a quick start does not flash it. Once a
  // load has its answers, the page takes them (pagePrepared), and a request
  // that asks for the admin key afterwards reloads the page (askForKey).
  var contentReady = false;
  var pagePrepared = false;
  // The load under way: {cancel, promise}; see loadPage.
  var pageLoad = null;
  var loadState = { state: "", message: "" };
  var loadCardTimer = null;
  var LOAD_TIMEOUT = 15000;
  var LOAD_CARD_DELAY = 300;

  function hideContent() {
    contentReady = false;
    $("app").hidden = true;
  }

  function showContent() {
    contentReady = true;
    setLoadState("");
    $("app").hidden = false;
  }

  // state is "loading", "failed" with the reason in message, or "".
  function setLoadState(state, message) {
    loadState = { state: state, message: message || "" };
    clearTimeout(loadCardTimer);
    if (state === "loading" && $("load-section").hidden) loadCardTimer = setTimeout(renderLoadCard, LOAD_CARD_DELAY);
    else renderLoadCard();
  }

  function renderLoadCard() {
    $("load-section").hidden = !loadState.state;
    $("load-text").textContent = loadState.state === "failed" ? translate("load_failed", loadState.message) : translate("loading");
    $("load-retry").hidden = loadState.state !== "failed";
  }

  function keyAsked() { return !$("key-section").hidden; }

  // Cancels the load under way; it then ends without changing the page.
  function cancelLoad() {
    if (pageLoad) pageLoad.cancel();
    pageLoad = null;
  }

  // Starts with the stored key. The internal listener needs no key; the
  // public one answers 401 without the admin key, which asks for it. A
  // stored key a header cannot carry is asked for again at once, since the
  // browser would fail its requests without a 401.
  function begin() {
    try { key = localStorage.getItem(KEY_STORE) || ""; } catch (err) {}
    if (key && !validKey(key)) askForKey(translate("key_invalid"));
    else start();
  }

  function start() {
    $("key-section").hidden = true;
    loadPage();
    clearInterval(refreshTimer);
    refreshTimer = setInterval(function () {
      if (document.hidden || !magpie.visible || keyAsked()) return;
      if (!contentReady) loadPage();
      else loadStatus().then(refreshHistory).catch(function () {});
    }, 30000);
  }

  // Reads the status and the config, restores the page state from them
  // while the app is still hidden (preparePage), then shows the page
  // (showPage). One load runs at a time: it holds pageLoad until it ends.
  // Each request gives up after LOAD_TIMEOUT, and the first failure cancels
  // the other. A load that asks for the admin key before it has its
  // answers is cancelled (cancelLoad) and ends without changing the page.
  // Resolves to whether the page was shown.
  function loadPage() {
    if (pageLoad) return pageLoad.promise;
    var abort = window.AbortController ? new AbortController() : null;
    var round = { failure: null, answers: [] };
    var current = function () { return pageLoad === round; };
    round.cancel = function () { if (abort) abort.abort(); };
    pageLoad = round;
    setLoadState("loading");
    var request = function (read, at) {
      return read(LOAD_TIMEOUT, abort ? abort.signal : undefined).then(function (data) {
        round.answers[at] = data;
      }, function (err) {
        if (round.failure) return;
        round.failure = err;
        round.cancel();
      });
    };
    round.promise = Promise.all([request(fetchStatus, 0), request(fetchConfig, 1)]).then(function () {
      if (!current()) return false;
      if (round.failure) {
        pageLoad = null;
        setLoadState("failed", round.failure.message);
        return false;
      }
      return preparePage(round.answers[0], round.answers[1]).then(function () {
        if (!current()) return false;
        pageLoad = null;
        showPage();
        return true;
      });
    });
    return round.promise;
  }

  // Takes the status and the config cfg of a load and restores the page
  // state of cfg, the first valid config, while the app is hidden: the
  // masking applies before any account name shows, and the chart state,
  // with the history of a saved window it may need, before the first chart
  // and before anything can be clicked. Resolves once the chart state is
  // restored.
  function preparePage(data, cfg) {
    pagePrepared = true;
    useStatus(data);
    useConfig(cfg);
    restorePageState(cfg);
    return Promise.resolve(restoreChartState(cfg));
  }

  // Shows the page, then loads the chosen chart.
  function showPage() {
    checkUpdate();
    // Shown before drawing, since the chart measures its width; nothing
    // is painted before the drawing below.
    showContent();
    renderStatus();
    fillSettings();
    loadHistory().then(function (loaded) {
      if (loaded) prefetchHistory();
    });
  }

  // Magpie's sandbox blocks form submission without a submit event, so the
  // key is also taken on Enter and on a click of the button. The key is
  // stored and the page loads anew with it. Without localStorage the page
  // loads with the key, which lasts until the page reloads: the key form
  // shows only on a page that has taken nothing (see askForKey). A key a
  // header cannot carry is not taken.
  function submitKey(e) {
    e.preventDefault();
    var entered = $("key-input").value.trim();
    if (!entered) return;
    if (!validKey(entered)) {
      $("key-error").hidden = false;
      $("key-error").textContent = translate("key_format");
      return;
    }
    try {
      localStorage.setItem(KEY_STORE, entered);
      location.reload();
      return;
    } catch (err) {}
    key = entered;
    start();
  }
  $("key-form").addEventListener("submit", submitKey);
  $("key-submit").addEventListener("click", submitKey);
  $("key-input").addEventListener("keydown", function (e) { if (e.key === "Enter") submitKey(e); });
  $("refresh-all").addEventListener("click", function (e) {
    runAction(e.currentTarget, API + "/refresh", {}, translate("refreshed_all"));
  });
  $("test-notify").addEventListener("click", function (e) {
    // The test waits for the saves under way and for the plugin to apply
    // the latest of them. When it is not applied in time, the test is not
    // sent, since it would use the settings before the change.
    var button = e.currentTarget;
    button.disabled = true;
    var results = $("test-results");
    clear(results);
    saveQueue.then(function () {
      var token = lastToken();
      if (!token || tokenApplied(savedTokens, token, status && status.config_applied_token)) return true;
      results.appendChild(el("div", { class: "test-result hint", text: translate("test_waiting") }));
      return whenApplied(token);
    }).then(function (applied) {
      if (!applied) {
        clear(results);
        results.appendChild(el("div", { class: "test-result failed", text: translate("test_not_applied") }));
        return null;
      }
      return api("POST", API + "/test-notify", {});
    }).then(function (data) {
      if (!data) return;
      if (data.status) { status = data.status; renderStatus(); }
      renderTestResults(data.results);
      if (data.ok) toast(translate("test_sent"));
      else if (!data.results) toast(translate("failed", data.error));
      else toast(translate(data.results.some(function (r) { return r.ok; }) ? "test_some_failed" : "test_all_failed"));
    }).catch(function (err) {
      clear(results);
      if (err.message !== "unauthorized") toast(translate("failed", err.message));
    }).then(function () { button.disabled = false; });
  });
  $("settings").addEventListener("change", saveField);
  // Keep failed drafts visible while invalidating responses for older edits.
  $("settings").addEventListener("input", function (e) {
    var accountField = e.target.hasAttribute("data-account-field");
    if (accountField) {
      e.target.accountDirty = true;
      e.target.accountEdit = (e.target.accountEdit || 0) + 1;
    } else e.target.settingsDirty = true;
    var top = inputTop(e.target);
    if (top) noteEdit(e.target);
    var host = accountField ? accountFieldHost(e.target) : top ? saveHost(e.target) : null;
    if (host) editSaveState(host, e.target);
    // The client configs follow the addresses as they are typed.
    if (e.target.name === "public_listen" || e.target.name === "mcp_public_url") renderMcp();
  });
  timezoneOptions().forEach(function (name) { $("timezone-options").appendChild(el("option", { value: name })); });
  // Saves the time zone the field shows, such as the browser's one filled in.
  $("timezone-save").addEventListener("click", function () { saveField({ target: $("settings").elements.timezone }); });
  $("mask-toggle").addEventListener("click", toggleMask);
  $("mcp-key-new").addEventListener("click", generateMcpKey);
  $("mcp-key-show").addEventListener("click", function () { mcpKeyShown = !mcpKeyShown; renderMcp(); });
  $("mcp-key-copy").addEventListener("click", function (e) { copyText(mcpKey(), e.currentTarget); });
  $("admin-key-new").addEventListener("click", generateAdminKey);
  $("admin-key-show").addEventListener("click", function () { adminKeyShown = !adminKeyShown; renderPublic(); });
  $("admin-key-copy").addEventListener("click", function (e) { copyText(adminKey(), e.currentTarget); });

  // The quota and settings views; the last one shown is kept in this
  // browser. The chart is drawn when its view shows, since it measures its
  // width.
  var VIEW_STORE = "lamplighter-view";
  function showView(name, bounce) {
    var settings = name === "settings";
    $("view-quota").hidden = settings;
    $("view-settings").hidden = !settings;
    pressSegment("view-buttons", settings ? "settings" : "quota", bounce);
    try { localStorage.setItem(VIEW_STORE, settings ? "settings" : "quota"); } catch (err) {}
    if (!settings && history && status) renderChart();
  }
  $("view-buttons").addEventListener("click", function (e) {
    var button = e.target.closest(".seg");
    if (button && button.getAttribute("aria-pressed") !== "true") showView(button.getAttribute("data-value"), true);
  });
  $("settings").elements["remember_chart_filters"].addEventListener("change", function (e) { saveChartSwitch(e.target); });
  $("settings").elements["remember_chart_expanded"].addEventListener("change", function (e) { saveChartSwitch(e.target); });
  $("load-retry").addEventListener("click", function () { loadPage(); });
  $("settings").addEventListener("submit", function (e) { e.preventDefault(); });
  // Enter commits a text or number field; leaving it fires the change that saves it.
  $("settings").addEventListener("keydown", function (e) {
    if (e.key === "Enter" && e.target.tagName === "INPUT" && /^(text|number|password)$/.test(e.target.type)) {
      e.preventDefault();
      e.target.blur();
    }
  });
  $("events-more").addEventListener("click", function () { eventsExpanded = !eventsExpanded; renderEvents(); });
  $("events-5h").addEventListener("change", function (e) {
    showFiveHourResets = e.target.checked;
    writePageState("events_show_5h_resets", { events_show_5h_resets: showFiveHourResets || null });
    if (status && contentReady) renderEvents();
  });
  // bounce is set when the user picks the value, so the thumb slides there.
  function pressSegment(id, value, bounce) {
    var box = $(id);
    Array.prototype.forEach.call(box.querySelectorAll(".seg"), function (b) {
      b.setAttribute("aria-pressed", b.getAttribute("data-value") === value ? "true" : "false");
    });
    placeThumb(box, !!bounce);
  }
  // Delegated, so it also covers buttons added later. Clicking the pressed
  // button only retries a chart that failed to load.
  function bindSegmented(id, onPick) {
    $(id).addEventListener("click", function (e) {
      var button = e.target.closest(".seg");
      if (!button) return;
      if (button.getAttribute("aria-pressed") === "true") {
        if (historyFailed) loadHistory();
        return;
      }
      var value = button.getAttribute("data-value");
      pressSegment(id, value, true);
      onPick(value);
    });
  }
  bindSegmented("range-buttons", function (value) {
    range = value;
    saveChartFilters();
    loadHistory();
  });
  bindSegmented("window-buttons", function (value) {
    chartWindow = value;
    range = rangeFor(value);
    saveChartFilters();
    renderRangeButtons();
    loadHistory();
  });
  bindSegmented("sort-buttons", function (value) {
    chartSort = value;
    saveChartFilters();
    renderChart();
  });
  renderRangeButtons();
  chartCard.addEventListener("pointerenter", function () { chartPointer = true; });
  chartCard.addEventListener("pointerleave", function () {
    chartPointer = false;
    if (chartBehind && !chartCard.contains(document.activeElement)) renderChart();
  });
  chartCard.addEventListener("focusout", function (e) {
    if (chartBehind && !chartPointer && !chartCard.contains(e.relatedTarget)) renderChart();
  });
  window.addEventListener("resize", function () { if (history) renderChart(); });
  window.addEventListener("languagechange", applyLanguage);
  if (window.matchMedia) {
    var media = window.matchMedia("(prefers-color-scheme: dark)");
    if (media.addEventListener) media.addEventListener("change", function () { applyTheme(); if (status && history) renderChart(); });
  }
  setupHome();
  applyTheme();
  applyLanguage();
  magpieHello();
  var storedView = "quota";
  try { storedView = localStorage.getItem(VIEW_STORE) || "quota"; } catch (err) {}
  showView(storedView);
  begin();
})();
