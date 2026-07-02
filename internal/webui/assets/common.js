// metalkit shared UI helpers used by all pages introduced in M2.3-9.
// Loaded BEFORE page-specific scripts. Exposes window.MK for cross-script use.
//
// Helpers:
//   MK.API_BASE                  → resolved from <meta name="metalkit-api-base">
//   MK.escapeHTML(s)
//   MK.fmtRelative(unixSec)
//   MK.fmtAbsolute(unixSec)
//   MK.fmtISO(iso)               → ISO string → "YYYY-MM-DD HH:MM:SSZ"
//   MK.fmtDuration(seconds)
//   MK.truncate(s, n)
//   MK.apiGet(path)              → resolved JSON or throws Error
//   MK.apiSend(method, path, body) → resolved JSON / null (204) or throws
//   MK.openModal(html, {onClose}) → returns the <dialog>
//   MK.closeModal()
//   MK.flashSuccess(msg)
//   MK.flashError(msg)           → sets the page's #error-banner if present
//   MK.copyText(text, flashEl)
//   MK.basicAuthUser()           → "admin" (best effort from URL credentials)
//
// Page-specific scripts may shadow these inside an IIFE; that's fine.

(function () {
  "use strict";

  const API_BASE = (function () {
    const m = document.querySelector('meta[name="metalkit-api-base"]');
    return (m && m.getAttribute("content")) || "/api/v1";
  })();

  function escapeHTML(s) {
    if (s == null) return "";
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function fmtRelative(unixSec) {
    if (unixSec == null) return "-";
    const n = Number(unixSec);
    if (!isFinite(n) || n <= 0) return "-";
    const diff = Math.max(0, Math.floor(Date.now() / 1000 - n));
    if (diff < 5) return "刚刚";
    if (diff < 60) return diff + " 秒前";
    if (diff < 3600) return Math.floor(diff / 60) + " 分钟前";
    if (diff < 86400) return Math.floor(diff / 3600) + " 小时前";
    if (diff < 86400 * 30) return Math.floor(diff / 86400) + " 天前";
    if (diff < 86400 * 365) return Math.floor(diff / 86400 / 30) + " 月前";
    return Math.floor(diff / 86400 / 365) + " 年前";
  }

  function fmtAbsolute(unixSec) {
    if (unixSec == null) return "-";
    const n = Number(unixSec);
    if (!isFinite(n) || n <= 0) return "-";
    return new Date(n * 1000).toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
  }

  function fmtISO(iso) {
    if (!iso) return "-";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
  }

  function fmtBytes(n) {
    if (n == null || n === 0) return n === 0 ? "0 B" : "-";
    const v = Number(n);
    if (!isFinite(v)) return "-";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    let i = 0;
    let cur = v;
    while (cur >= 1024 && i < units.length - 1) {
      cur /= 1024;
      i++;
    }
    return (cur >= 100 ? cur.toFixed(0) : cur.toFixed(1)) + " " + units[i];
  }

  const POLL = {
    LIST_MS: 30000,
    JOBS_MS: 5000,
    JOB_RUNNING_MS: 1000,
    JOB_TERMINAL_MS: 5000,
  };

  async function withBusy(btn, busyText, fn) {
    if (!btn) return fn();
    const orig = btn.textContent;
    const wasDisabled = btn.disabled;
    btn.disabled = true;
    if (busyText) btn.textContent = busyText;
    try { return await fn(); }
    finally {
      btn.disabled = wasDisabled;
      if (busyText) btn.textContent = orig;
    }
  }

  function wireCopyables(root) {
    (root || document).querySelectorAll(".copyable").forEach(function (el) {
      if (el.dataset.copyWired === "1") return;
      el.dataset.copyWired = "1";
      el.addEventListener("click", function () {
        const txt = el.dataset.copy || el.textContent;
        if (txt && txt !== "-") copyText(txt, el);
      });
    });
  }

  function fmtDuration(seconds) {
    if (seconds == null) return "-";
    const n = Number(seconds);
    if (!isFinite(n) || n < 0) return "-";
    if (n < 60) return Math.round(n) + "s";
    if (n < 3600) return Math.floor(n / 60) + "m " + Math.round(n % 60) + "s";
    const h = Math.floor(n / 3600);
    const m = Math.floor((n % 3600) / 60);
    return h + "h " + m + "m";
  }

  function truncate(s, n) {
    if (s == null) return "";
    const str = String(s);
    if (str.length <= n) return str;
    return str.slice(0, n) + "…";
  }

  function basicAuthUser() {
    // Best effort: not available from the browser after Basic Auth has been
    // negotiated. Used only for display fallbacks.
    return "admin";
  }

  async function _doRequest(method, path, body) {
    const url = API_BASE + path;
    const opts = {
      method,
      headers: { "Accept": "application/json" },
      credentials: "same-origin",
    };
    if (body !== undefined && body !== null) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    let resp;
    try {
      resp = await fetch(url, opts);
    } catch (e) {
      throw new Error("network error contacting " + url + ": " + e.message);
    }
    if (resp.status === 204) return null;
    const ct = resp.headers.get("Content-Type") || "";
    let payload = null;
    if (ct.indexOf("application/json") !== -1) {
      try { payload = await resp.json(); } catch (_) { /* tolerate */ }
    } else {
      try { payload = await resp.text(); } catch (_) { /* ignore */ }
    }
    if (!resp.ok) {
      if (resp.status === 401) {
        // session expired — bounce to login, preserving where we came from
        const next = encodeURIComponent(window.location.pathname + window.location.search);
        window.location.href = "/ui/login?next=" + next;
        // throw so callers don't proceed (the redirect will preempt anyway)
        throw new Error("unauthorized — redirecting to login");
      }
      let msg = "HTTP " + resp.status;
      if (payload && typeof payload === "object" && payload.error) {
        msg += ": " + payload.error;
      } else if (typeof payload === "string" && payload) {
        msg += ": " + payload.slice(0, 200);
      }
      const err = new Error(msg);
      err.status = resp.status;
      err.payload = payload;
      throw err;
    }
    return payload;
  }

  function apiGet(path) { return _doRequest("GET", path, null); }
  function apiSend(method, path, body) { return _doRequest(method, path, body); }

  // --- modal -----------------------------------------------------------

  // Modal stack. openModal pushes a new <dialog> on top; closeModal pops
  // only the top one. This lets a child modal (e.g. NIC picker) overlay a
  // parent modal (e.g. profile form) without destroying it.
  const modalStack = [];

  // openModal(html, {onClose}) creates a <dialog class="modal"> on top of
  // any existing modals and inserts `html` into its body. Returns the
  // dialog element. The caller is responsible for wiring submit/cancel
  // buttons inside; closeModal() will close the topmost modal only.
  function openModal(innerHTML, opts) {
    const dlg = document.createElement("dialog");
    dlg.className = "modal";
    dlg.innerHTML = innerHTML;
    document.body.appendChild(dlg);
    modalStack.push(dlg);
    dlg.addEventListener("close", function () {
      const i = modalStack.indexOf(dlg);
      if (i >= 0) modalStack.splice(i, 1);
      if (opts && typeof opts.onClose === "function") {
        try { opts.onClose(); } catch (_) { /* ignore */ }
      }
      if (dlg.parentNode) dlg.parentNode.removeChild(dlg);
    });
    // ESC works natively on <dialog>; we also wire .modal-close buttons.
    dlg.querySelectorAll("[data-modal-close]").forEach(function (el) {
      el.addEventListener("click", function (e) {
        e.preventDefault();
        dlg.close();
      });
    });
    dlg.showModal();
    return dlg;
  }

  // closeModal closes only the topmost modal. Callers that mean "close
  // everything" should loop, but in practice we always close the one we
  // just opened.
  function closeModal() {
    const top = modalStack[modalStack.length - 1];
    if (top) top.close();
  }

  function modalShell(title, bodyHTML, footerHTML) {
    return (
      '<form method="dialog" class="modal-form">' +
      '<div class="modal-header">' +
      '<h3>' + escapeHTML(title) + '</h3>' +
      '<button type="button" class="modal-close" data-modal-close aria-label="关闭">×</button>' +
      '</div>' +
      '<div class="modal-body">' + bodyHTML + '</div>' +
      '<div class="modal-footer">' + (footerHTML || '') + '</div>' +
      '</form>'
    );
  }

  // --- flash -----------------------------------------------------------

  let flashTimer = null;
  function flashSuccess(msg) {
    if (flashTimer) clearTimeout(flashTimer);
    document.querySelectorAll(".flash").forEach(function (n) { n.remove(); });
    const div = document.createElement("div");
    div.className = "flash";
    div.textContent = msg;
    document.body.appendChild(div);
    flashTimer = setTimeout(function () { div.remove(); }, 2500);
  }

  function flashError(msg) {
    const b = document.getElementById("error-banner");
    if (b) {
      b.textContent = msg;
      b.hidden = false;
    } else {
      // No banner on the page → fall back to a transient red flash.
      const div = document.createElement("div");
      div.className = "flash";
      div.style.background = "var(--danger)";
      div.textContent = msg;
      document.body.appendChild(div);
      setTimeout(function () { div.remove(); }, 4000);
    }
  }

  function clearError() {
    const b = document.getElementById("error-banner");
    if (b) { b.textContent = ""; b.hidden = true; }
  }

  // --- clipboard -------------------------------------------------------

  function copyText(text, flashEl) {
    if (!navigator.clipboard || !navigator.clipboard.writeText) {
      flashError("浏览器不支持 clipboard API");
      return;
    }
    navigator.clipboard.writeText(text).then(function () {
      if (flashEl) {
        flashEl.classList.add("copy-flash");
        setTimeout(function () { flashEl.classList.remove("copy-flash"); }, 600);
      }
    }).catch(function (e) {
      flashError("复制失败：" + e.message);
    });
  }

  // --- nav active-state ------------------------------------------------

  // Pages all ship the same <nav class="topnav"> markup; this function
  // sets .active on the entry that matches the current path.
  function markNavActive() {
    const p = window.location.pathname;
    document.querySelectorAll(".topnav a").forEach(function (a) {
      const href = a.getAttribute("href") || "";
      a.classList.remove("active");
      if (href === "/ui/" && (p === "/ui/" || p === "/ui")) a.classList.add("active");
      else if (href === "/ui/m" && p.indexOf("/ui/m/") === 0) a.classList.add("active");
      else if (href !== "/ui/" && href !== "" && p.indexOf(href) === 0) a.classList.add("active");
    });
  }
  document.addEventListener("DOMContentLoaded", markNavActive);

  // --- logout ----------------------------------------------------------

  async function logout() {
    if (!confirm("确认退出登录？")) return;
    try {
      await fetch(API_BASE + "/auth/logout", {
        method: "POST",
        credentials: "same-origin",
        cache: "no-store",
      });
    } catch (_) { /* best-effort */ }
    window.location.href = "/ui/login?logout=1";
  }

  document.addEventListener("DOMContentLoaded", function () {
    const btn = document.getElementById("logout-btn");
    if (btn) btn.addEventListener("click", logout);
  });

  // --- current user (right of nav) -------------------------------------

  async function loadCurrentUser() {
    const el = document.getElementById("current-user");
    if (!el) return;
    try {
      const r = await fetch(API_BASE + "/auth/me", {
        credentials: "same-origin",
        cache: "no-store",
      });
      if (r.ok) {
        const j = await r.json();
        if (j && j.username) el.textContent = j.username;
      }
    } catch (_) { /* tolerate */ }
  }

  document.addEventListener("DOMContentLoaded", loadCurrentUser);

  // --- install modal (shared between detail page + machine list) -------

  // openInstallModal(uuid, opts) — 完整 binding + 装机一体表单。
  //
  // opts:
  //   defaults:    { image_id, profile_id, hostname, static_address,
  //                  desired_state }  预填值；通常是 GET /bindings/{uuid} 的结果
  //   suggestState: "install" | "reinstall"  默认主按钮文案；未传时按 defaults
  //                  推断（已有 succeeded 历史 → reinstall，否则 install）
  //   onSuccess:   function(updatedBinding)  PUT 成功后回调（刷新调用方 UI）
  //
  // 表单字段：镜像 / 配置 / 子网 / 主机名 / 静态地址 / Root 密码 / 目标盘 / Bond / 期望状态 radio。
  // 提交时直接 PUT /bindings/{uuid} —— orchestrator 下一 tick 拾起。
  async function openInstallModal(uuid, opts) {
    opts = opts || {};
    const defaults = opts.defaults || {};
    let suggestState = opts.suggestState ||
      (defaults.desired_state === "reinstall" ? "reinstall" : "install");

    // 把目录预拉一遍。失败时退化成只显示 ID。机器报告失败时退化成"自动"。
    const [imgs, profs, report, subs] = await Promise.all([
      apiGet("/images").catch(function () { return []; }),
      apiGet("/profiles").catch(function () { return []; }),
      apiGet("/machines/" + encodeURIComponent(uuid)).catch(function () { return null; }),
      apiGet("/subnets").catch(function () { return []; }),
    ]);
    const images = (Array.isArray(imgs) ? imgs : []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    const profiles = (Array.isArray(profs) ? profs : []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    const subnets = (Array.isArray(subs) ? subs : []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    const subnetByID = Object.create(null);
    subnets.forEach(function (s) { if (s && s.id) subnetByID[s.id] = s; });
    const profileByID = Object.create(null);
    profiles.forEach(function (p) { if (p && p.id) profileByID[p.id] = p; });
    // 仅保留 type=disk 且非 removable 的物理盘，agent 在装机时也是按这个口径过滤。
    const allDisks = (report && Array.isArray(report.disks)) ? report.disks : [];
    const disks = allDisks.filter(function (d) {
      return d && d.type === "disk" && !d.removable;
    });
    // 用于 bond slaves 选择器；过滤掉假 MAC / lo / virtual。
    const allNICs = (report && Array.isArray(report.nics)) ? report.nics : [];
    const macRE = /^[0-9a-f]{2}(:[0-9a-f]{2}){5}$/;
    const nics = allNICs.filter(function (n) {
      return n && macRE.test(String(n.mac || "").toLowerCase());
    }).slice().sort(function (a, b) {
      if (!!b.link !== !!a.link) return b.link ? 1 : -1;
      return String(a.name || "").localeCompare(String(b.name || ""));
    });

    const imgOpts = ['<option value="">— 选择镜像 —</option>'].concat(
      images.map(function (i) {
        const label = (i.name || i.id) +
          (i.version ? " " + i.version : "") +
          (i.family ? " (" + i.family + ")" : "");
        const sel = i.id === defaults.image_id ? " selected" : "";
        return '<option value="' + escapeHTML(i.id) + '"' + sel + ">" +
          escapeHTML(label) + "</option>";
      })
    ).join("");

    // 装机弹窗里允许操作员选 profile（配置模板）。defaults.profile_id 是 binding
    // 现有值；没传时默认挑第一个 profile。一个 profile 都没有则展示提示让操作员
    // 先去 /ui/profiles 创建。
    const autoProfileID = defaults.profile_id ||
      (profiles.length > 0 ? profiles[0].id : "");
    const profOpts = ['<option value="">— 选择配置 —</option>'].concat(
      profiles.map(function (p) {
        const label = (p.name || p.id) +
          (p.os_family && p.os_family !== "any" ? " [" + p.os_family + "]" : "");
        const sel = p.id === autoProfileID ? " selected" : "";
        return '<option value="' + escapeHTML(p.id) + '"' + sel + ">" +
          escapeHTML(label) + "</option>";
      })
    ).join("");
    const profileMissingBanner = profiles.length > 0
      ? ""
      : '<div class="error-banner" style="margin-bottom:.5em">尚未配置任何 profile。' +
        '请先在 <a href="/ui/profiles">配置页</a> 创建一个 profile（含 root 密码 hash、target_disk 等模板）。</div>';

    // 子网 select：空值 = 不绑定 subnet（沿用 profile.network）。
    // 初始选中策略：binding 已有 subnet_id 优先；否则回退到 profile.subnet_id
    // （profile 表里挂的默认子网）。后者通过 wireInstallProfile 实现联动，profile
    // 一改就跟着切——除非用户已手动改过子网下拉。
    const initialSubnetID = defaults.subnet_id ||
      (autoProfileID && profileByID[autoProfileID] && profileByID[autoProfileID].subnet_id) ||
      "";
    const snOpts = ['<option value="">— 不绑定（沿用 profile.network） —</option>'].concat(
      subnets.map(function (s) {
        const label = (s.name || s.id) + " (" + s.cidr + ", gw " + s.gateway +
          (s.vlan_id ? ", vlan " + s.vlan_id : "") + ")";
        const sel = s.id === initialSubnetID ? " selected" : "";
        return '<option value="' + escapeHTML(s.id) + '"' + sel + ">" +
          escapeHTML(label) + "</option>";
      })
    ).join("");

    const stateOpts = [
      { v: "install",   l: "装机（首次或全擦重装）" },
      { v: "reinstall", l: "重装（保留 BMC 链路，强制 PXE）" },
    ];
    const radios = stateOpts.map(function (s) {
      const checked = s.v === suggestState ? " checked" : "";
      return '<label class="kv"><input type="radio" name="mk-install-state" value="' +
        s.v + '"' + checked + "> " + escapeHTML(s.l) + "</label>";
    }).join(" ");

    // 目标盘下拉：
    //   ""         → 沿用 profile 默认（默认选项）
    //   "smallest" → 强制最小一块
    //   "by-wwn:<wwn>"   → 锁某块（标识用 WWN）
    //   "by-path:<path>" → 锁某块（标识用 /dev/disk/by-path）
    // 提交时 by-* 项拆成 {mode,value}；smallest 单独发；空字符串不带字段（保持现有 override）。
    const td = defaults.target_disk || null;
    let preselect = "";  // 「沿用 profile / 不改 override」
    if (td && td.mode === "smallest") preselect = "smallest";
    else if (td && td.mode === "by-wwn" && td.value) preselect = "by-wwn:" + td.value;
    else if (td && td.mode === "by-path" && td.value) preselect = "by-path:" + td.value;
    const diskOptsParts = [
      '<option value="">— 沿用 profile 默认（不改 override）—</option>',
      '<option value="__clear__"' + (defaults.target_disk === null ? "" : "") + '>— 清除 override（恢复 profile 默认）—</option>',
      '<option value="smallest"' + (preselect === "smallest" ? " selected" : "") + '>最小非 removable 盘（agent 自动选）</option>',
    ];
    if (disks.length === 0) {
      diskOptsParts.push('<option disabled>— 这台机器还没有最新硬件报告 —</option>');
    } else {
      disks.forEach(function (d) {
        const sizeGB = (d.size_bytes / (1024 * 1024 * 1024));
        const sizeStr = sizeGB >= 100 ? sizeGB.toFixed(0) + " GiB" : sizeGB.toFixed(1) + " GiB";
        const model = (d.model || d.vendor || "未知型号").trim();
        const tx = (d.transport || "").toUpperCase();
        const rotLabel = d.rotational ? "HDD" : "SSD";
        const labelPrefix = [model, tx, rotLabel, sizeStr].filter(Boolean).join(" · ");
        if (d.wwn) {
          const val = "by-wwn:" + d.wwn;
          const sel = preselect === val ? " selected" : "";
          diskOptsParts.push('<option value="' + escapeHTML(val) + '"' + sel + ">" +
            escapeHTML(labelPrefix + " · wwn=" + d.wwn) + "</option>");
        } else if (d.path && d.path.indexOf("/dev/disk/by-path/") === 0) {
          // 极少：lsblk 返回 by-path 形式
          const val = "by-path:" + d.path;
          const sel = preselect === val ? " selected" : "";
          diskOptsParts.push('<option value="' + escapeHTML(val) + '"' + sel + ">" +
            escapeHTML(labelPrefix + " · " + d.path) + "</option>");
        } else {
          // 缺 WWN，标灰不让选——靠 smallest 兜底
          diskOptsParts.push('<option disabled>' +
            escapeHTML(labelPrefix + " · " + (d.path || d.kname || "?") + " · (缺 wwn，无法稳定标识)") +
            "</option>");
        }
      });
    }
    const diskSelect = '<select id="mk-inst-disk">' + diskOptsParts.join("") + "</select>";

    const bondCur = defaults.bond || null;
    const bondOn = !!bondCur;
    const bondMode = (bondCur && bondCur.mode) || "active-backup";
    const bondMiimon = (bondCur && bondCur.miimon) || 100;
    const bondPrimary = (bondCur && bondCur.primary) || "";
    const bondLACP = (bondCur && bondCur.lacp_rate) || "fast";
    const bondXmit = (bondCur && bondCur.xmit_hash_policy) || "layer3+4";
    const bondSlavesSet = {};
    if (bondCur && Array.isArray(bondCur.slaves)) {
      bondCur.slaves.forEach(function (n) { bondSlavesSet[n] = true; });
    }

    // NIC selector override：装机弹窗是 override 真相源。三态:
    //   ""               → 沿用 profile.network.nic_selector（一般是 auto）
    //   "by-mac:<MAC>"   → 指定某块网卡（弹窗里点 radio 选中）
    //   bond 启用时       → 这个字段被忽略（spec 端用 bond.slaves 覆盖）
    // defaults.nic_selector_override 由调用方填入当前 binding 的值（detail-prov.js）。
    const nicSelCur = (defaults.nic_selector_override || "").toLowerCase();
    const nicSelMACCur = nicSelCur.indexOf("by-mac:") === 0
      ? nicSelCur.slice("by-mac:".length).toLowerCase()
      : "";
    const nicSelModeCur = nicSelMACCur ? "pick" : "auto";
    let nicSelRows = "";
    if (nics.length === 0) {
      nicSelRows = '<tr><td colspan="5" class="muted">这台机器还没有 NIC 数据（先让 agent 上报一次）。</td></tr>';
    } else {
      // 默认选中规则：当前 override 优先；否则挑第一块 link=up 的；都没 up 就第一块。
      let defaultPickMAC = nicSelMACCur;
      if (!defaultPickMAC) {
        const upNIC = nics.find(function (n) { return n.link; });
        defaultPickMAC = String((upNIC || nics[0]).mac || "").toLowerCase();
      }
      nicSelRows = nics.map(function (n) {
        const name = String(n.name || "").trim();
        const mac = String(n.mac || "").toLowerCase();
        const speed = n.speed_mbps
          ? (n.speed_mbps >= 1000 ? (n.speed_mbps / 1000) + " Gbps" : n.speed_mbps + " Mbps")
          : "-";
        if (!mac) return "";
        const checked = mac === defaultPickMAC ? " checked" : "";
        const linkBadge = n.link ? '<span class="badge badge-ok">up</span>' : '<span class="badge">down</span>';
        const model = [n.driver, n.firmware_version].filter(Boolean).join(" ");
        return '<tr>' +
          '<td><input type="radio" name="mk-inst-nic-pick" class="mk-inst-nic-pick" value="' + escapeHTML(mac) + '"' + checked + '></td>' +
          '<td class="mono">' + escapeHTML(name) + '</td>' +
          '<td class="mono">' + escapeHTML(mac) + '</td>' +
          '<td>' + escapeHTML(model || "-") + '</td>' +
          '<td>' + linkBadge + ' ' + escapeHTML(speed) + '</td>' +
        '</tr>';
      }).join("");
    }
    const nicFieldset =
      '<fieldset class="form-fieldset" id="mk-inst-nic-fieldset"><legend>承载 IP 的物理网卡</legend>' +
      '<div class="form-row"><label><input type="radio" name="mk-inst-nic-mode" value="auto"' +
        (nicSelModeCur === "auto" ? " checked" : "") + '> 自动（agent 选第一个 link=up 网卡）</label></div>' +
      '<div class="form-row"><label><input type="radio" name="mk-inst-nic-mode" value="pick"' +
        (nicSelModeCur === "pick" ? " checked" : "") + '> 指定某块网卡（按 MAC 锁定，跨发行版稳定）</label></div>' +
      '<div id="mk-inst-nic-table"' + (nicSelModeCur === "pick" ? '' : ' hidden') + '>' +
      '<table class="data-table" style="margin-top:.25em">' +
      '<thead><tr><th></th><th>名称</th><th>MAC</th><th>型号 / 驱动</th><th>Link / 速度</th></tr></thead>' +
      '<tbody>' + nicSelRows + '</tbody></table></div>' +
      '<div class="muted">启用 Bond 后此处忽略 —— bond.slaves 自己列了承载的 NIC。</div>' +
      '</fieldset>';

    let bondNICRows = "";
    if (nics.length === 0) {
      bondNICRows = '<tr><td colspan="5" class="muted">这台机器还没有 NIC 数据（先让 agent 上报一次）。</td></tr>';
    } else {
      bondNICRows = nics.map(function (n) {
        const name = String(n.name || "").trim();
        const mac = String(n.mac || "").toLowerCase();
        const speed = n.speed_mbps
          ? (n.speed_mbps >= 1000 ? (n.speed_mbps / 1000) + " Gbps" : n.speed_mbps + " Mbps")
          : "-";
        if (!name) return "";
        const checked = bondSlavesSet[name] ? " checked" : "";
        const linkBadge = n.link ? '<span class="badge badge-ok">up</span>' : '<span class="badge">down</span>';
        const model = [n.driver, n.firmware_version].filter(Boolean).join(" ");
        return '<tr>' +
          '<td><input type="checkbox" class="mk-inst-bond-slave" value="' + escapeHTML(name) + '"' + checked + '></td>' +
          '<td class="mono">' + escapeHTML(name) + '</td>' +
          '<td class="mono">' + escapeHTML(mac) + '</td>' +
          '<td>' + escapeHTML(model || "-") + '</td>' +
          '<td>' + linkBadge + ' ' + escapeHTML(speed) + '</td>' +
        '</tr>';
      }).join("");
    }
    const bondFieldset =
      '<fieldset class="form-fieldset"><legend>网卡 Bond 覆盖（可选）</legend>' +
      '<div class="form-row"><label><input type="checkbox" id="mk-inst-bond-enable"' +
        (bondOn ? " checked" : "") + '> 启用 Bond（覆盖 profile.network.bond）</label>' +
      '<div class="muted">802.3ad 需要上联交换机先配好 port-channel。关闭后留空 = 沿用 profile；之前设过的话会显式清除。</div></div>' +
      '<div id="mk-inst-bond-fields"' + (bondOn ? '' : ' hidden') + '>' +
      '<div class="form-row"><label for="mk-inst-bond-mode">模式</label>' +
      '<select id="mk-inst-bond-mode">' +
        '<option value="active-backup"' + (bondMode === "active-backup" ? " selected" : "") + '>active-backup（单边即可）</option>' +
        '<option value="802.3ad"' + (bondMode === "802.3ad" ? " selected" : "") + '>802.3ad（LACP）</option>' +
      '</select></div>' +
      '<div class="form-row"><label>Slaves（勾选 ≥ 2 块物理网卡）<span class="required">*</span></label>' +
      '<table class="data-table" style="margin-top:.25em">' +
      '<thead><tr><th></th><th>名称</th><th>MAC</th><th>型号 / 驱动</th><th>Link / 速度</th></tr></thead>' +
      '<tbody>' + bondNICRows + '</tbody></table></div>' +
      '<div class="form-row"><label for="mk-inst-bond-miimon">Miimon (ms)</label>' +
      '<input type="number" id="mk-inst-bond-miimon" min="50" max="10000" value="' + bondMiimon + '"></div>' +
      '<div class="form-row" id="mk-inst-bond-primary-row"' +
        (bondMode === "active-backup" ? '' : ' hidden') + '>' +
      '<label for="mk-inst-bond-primary">主网卡（可选，需在 slaves 列表中）</label>' +
      '<input type="text" id="mk-inst-bond-primary" value="' + escapeHTML(bondPrimary) +
        '" placeholder="eno1"></div>' +
      '<div class="form-row" id="mk-inst-bond-lacp-row"' +
        (bondMode === "802.3ad" ? '' : ' hidden') + '>' +
      '<label for="mk-inst-bond-lacp">LACP rate</label>' +
      '<select id="mk-inst-bond-lacp">' +
        '<option value="fast"' + (bondLACP === "fast" ? " selected" : "") + '>fast</option>' +
        '<option value="slow"' + (bondLACP === "slow" ? " selected" : "") + '>slow</option>' +
      '</select></div>' +
      '<div class="form-row" id="mk-inst-bond-xmit-row"' +
        (bondMode === "802.3ad" ? '' : ' hidden') + '>' +
      '<label for="mk-inst-bond-xmit">Transmit hash policy</label>' +
      '<select id="mk-inst-bond-xmit">' +
        '<option value="layer2"' + (bondXmit === "layer2" ? " selected" : "") + '>layer2</option>' +
        '<option value="layer2+3"' + (bondXmit === "layer2+3" ? " selected" : "") + '>layer2+3</option>' +
        '<option value="layer3+4"' + (bondXmit === "layer3+4" ? " selected" : "") + '>layer3+4（推荐）</option>' +
      '</select></div>' +
      '</div></fieldset>';

    const body =
      '<p class="muted">机器 UUID：<span class="mono">' + escapeHTML(uuid) + '</span></p>' +
      profileMissingBanner +
      '<div class="kv-form">' +
      '<label class="kv"><span>镜像</span>' +
        '<select id="mk-inst-image" required>' + imgOpts + "</select></label>" +
      '<label class="kv"><span>配置</span>' +
        '<select id="mk-inst-profile" required>' + profOpts + "</select></label>" +
      '<label class="kv"><span>子网</span>' +
        '<select id="mk-inst-subnet">' + snOpts + "</select></label>" +
      '<div id="mk-inst-subnet-hint" class="kv-hint muted" hidden></div>' +
      '<label class="kv"><span>主机名</span>' +
        '<input type="text" id="mk-inst-hostname" value="' +
        escapeHTML(defaults.hostname || "") +
        '" placeholder="可选覆盖（留空用 profile 模板）" autocomplete="off"></label>' +
      '<label class="kv"><span>静态地址</span>' +
        '<input type="text" id="mk-inst-static" value="' +
        escapeHTML(defaults.static_address || "") +
        '" placeholder="例如 192.168.10.50（绑定子网后必填，不用带 /24）" autocomplete="off"></label>' +
      '<div id="mk-inst-static-hint" class="kv-hint muted" hidden></div>' +
      '<label class="kv"><span>Root 密码</span>' +
        '<div class="inline-row">' +
        '<input type="text" id="mk-inst-password" value="" autocomplete="off"' +
        ' placeholder="' +
        (defaults.has_password
          ? "已设置 —— 留空保留；输入新值覆盖"
          : "留空使用 profile 默认；点 🎲 生成随机口令") +
        '">' +
        '<button type="button" id="mk-inst-password-random" class="btn" title="生成 16 位随机口令">🎲 随机</button>' +
        '</div></label>' +
      '<label class="kv"><span>目标盘</span>' + diskSelect + "</label>" +
      '<div class="kv"><span>承载网卡</span>' + nicFieldset + "</div>" +
      '<div class="kv"><span>Bond</span>' + bondFieldset + "</div>" +
      '<div class="kv"><span>动作</span><div>' + radios + "</div></div>" +
      "</div>" +
      '<p class="muted">' +
        '<strong>注意：</strong>提交后 orchestrator 会通过 BMC 重启该机器并擦除目标磁盘。' +
        '密码以 AES-GCM 加密存储，可在机器详情页查看。' +
      '</p>';

    const footer =
      '<button type="button" class="btn btn-ghost" data-modal-close>取消</button> ' +
      '<button type="submit" id="mk-inst-save" class="btn btn-primary">开始装机</button>';

    const title = defaults.image_id ? "装机 / 重装" : "首次装机";
    const dlg = openModal(modalShell(title, body, footer));
    wireInstallBondToggle(dlg);
    wireInstallNICPicker(dlg);
    wireInstallSubnet(dlg, subnetByID);
    wireInstallProfile(dlg, profileByID, initialSubnetID);
    wireInstallRandomPassword(dlg);
    const form = dlg.querySelector("form");
    form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      submitInstallModal(dlg, uuid, opts, subnetByID);
    });
  }

  // wireInstallRandomPassword 给「🎲 随机」按钮挂事件：点一下生成 16 位
  // 字母+数字+符号的口令写入 #mk-inst-password。字符集刻意避开 shell 中
  // 容易引号转义事故的字符（反引号、引号、空格、斜杠、$）。生成完保持
  // input 为 type=text，让操作员能立刻看到/复制；提交后从机器详情页也能
  // 通过 /password 接口取回。
  function wireInstallRandomPassword(dlg) {
    const btn = dlg.querySelector("#mk-inst-password-random");
    const inp = dlg.querySelector("#mk-inst-password");
    if (!btn || !inp) return;
    const alphabet =
      "abcdefghijklmnopqrstuvwxyz" +
      "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
      "0123456789" +
      "!@#%^*_-+=";
    btn.addEventListener("click", function () {
      const len = 16;
      const buf = new Uint32Array(len);
      (window.crypto || window.msCrypto).getRandomValues(buf);
      let out = "";
      for (let i = 0; i < len; i++) {
        out += alphabet.charAt(buf[i] % alphabet.length);
      }
      inp.value = out;
      inp.type = "text";
      inp.focus();
      inp.select();
    });
  }

  // wireInstallProfile 让"配置"下拉变更时联动"子网"下拉：每个 profile 自己挂着
  // 一个 default subnet_id，切 profile 就跟着切 subnet。但如果操作员已经手动
  // 改过子网下拉（即当前值跟上一次自动填的值不一致），就不再覆盖，避免抹掉
  // 手工选择。
  function wireInstallProfile(dlg, profileByID, initialSubnetID) {
    const profSel = dlg.querySelector("#mk-inst-profile");
    const snSel = dlg.querySelector("#mk-inst-subnet");
    if (!profSel || !snSel) return;
    // 当前自动填入的子网值。初始即外层算出的 initialSubnetID（可能来自
    // defaults.subnet_id 或 autoProfile.subnet_id；user 视角下二者都算"非手工"）。
    let lastAutoSubnet = initialSubnetID || "";
    snSel.addEventListener("change", function () {
      // 用户主动改了子网。下次切 profile 不再覆盖。lastAutoSubnet 同步成
      // 新值，等价于"以这个为准"——后续若用户再手动改回原 profile 的默认值，
      // 也不会被误判为自动。
      lastAutoSubnet = snSel.value;
    });
    profSel.addEventListener("change", function () {
      // 操作员主动改了子网时，snSel.value !== lastAutoSubnet，跳过。
      if (snSel.value !== lastAutoSubnet) return;
      const p = profileByID[profSel.value];
      const want = (p && p.subnet_id) || "";
      if (snSel.value === want) return; // 没变化
      // 仅当 dropdown 里真的有这个 option（subnet 没被删过）才能赋值；
      // 否则保持当前，避免把 select 置成不合法值。
      const hasOpt = Array.from(snSel.options).some(function (o) { return o.value === want; });
      if (!hasOpt) return;
      snSel.value = want;
      lastAutoSubnet = want;
      // 触发 change 让 wireInstallSubnet 的 hint 跟着刷新。
      snSel.dispatchEvent(new Event("change"));
    });
  }

  // wireInstallSubnet 联动子网下拉 + 静态地址提示：选中后展示 CIDR/网关/DNS，
  // 静态地址实时校验是否在子网范围内（与后端 HostInSubnet 同口径）。
  function wireInstallSubnet(dlg, subnetByID) {
    const snSel = dlg.querySelector("#mk-inst-subnet");
    const snHint = dlg.querySelector("#mk-inst-subnet-hint");
    const staticInp = dlg.querySelector("#mk-inst-static");
    const staticHint = dlg.querySelector("#mk-inst-static-hint");
    if (!snSel || !snHint || !staticInp || !staticHint) return;
    function refreshSubnetHint() {
      const id = snSel.value;
      if (!id) { snHint.hidden = true; return; }
      const s = subnetByID && subnetByID[id];
      if (!s) { snHint.hidden = true; return; }
      const dns = (s.dns || []).join(", ") || "—";
      snHint.hidden = false;
      snHint.innerHTML =
        "CIDR <code>" + escapeHTML(s.cidr) + "</code> · 网关 <code>" +
        escapeHTML(s.gateway) + "</code> · DNS " + escapeHTML(dns) +
        (s.vlan_id ? " · VLAN " + s.vlan_id : "");
    }
    function refreshStaticHint() {
      const id = snSel.value;
      const host = staticInp.value.trim().split("/")[0]; // 容忍 CIDR 形式输入
      if (!id || !host) { staticHint.hidden = true; return; }
      const s = subnetByID && subnetByID[id];
      if (!s) { staticHint.hidden = true; return; }
      const err = hostInSubnetCheck(host, s.cidr, s.gateway);
      if (err) {
        staticHint.hidden = false;
        staticHint.className = "kv-hint";
        staticHint.style.color = "var(--accent-red, #c33)";
        staticHint.textContent = err;
      } else {
        staticHint.hidden = false;
        staticHint.className = "kv-hint muted";
        staticHint.style.color = "";
        staticHint.textContent = "✓ 在 " + s.cidr + " 范围内";
      }
    }
    snSel.addEventListener("change", function () {
      refreshSubnetHint();
      refreshStaticHint();
    });
    staticInp.addEventListener("input", refreshStaticHint);
    refreshSubnetHint();
    refreshStaticHint();
  }

  // hostInSubnetCheck mirrors internal/subnets HostInSubnet — client-side
  // pre-flight for snappy UX. Backend still re-validates authoritatively.
  function hostInSubnetCheck(host, cidr, gateway) {
    const ip = parseIPv4(host);
    if (!ip) return "地址不是合法 IPv4";
    const m = /^(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\/(\d{1,2})$/.exec(cidr);
    if (!m) return "subnet CIDR 解析失败";
    const net = parseIPv4(m[1]);
    const prefix = parseInt(m[2], 10);
    if (!net || prefix < 0 || prefix > 32) return "subnet CIDR 不合法";
    const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0;
    if ((ip & mask) !== (net & mask)) return "地址不在 " + cidr + " 范围内";
    if (ip === (net & mask)) return "地址等于网络号";
    const bcast = (net & mask) | (~mask >>> 0);
    if (ip === bcast) return "地址等于广播地址";
    const gw = parseIPv4(gateway);
    if (gw && ip === gw) return "地址不能与网关相同";
    return "";
  }
  function parseIPv4(s) {
    const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec((s || "").trim());
    if (!m) return null;
    let v = 0;
    for (let i = 1; i <= 4; i++) {
      const n = parseInt(m[i], 10);
      if (n < 0 || n > 255) return null;
      v = (v * 256 + n) >>> 0;
    }
    return v;
  }

  // wireInstallBondToggle binds the enable-checkbox + mode-select events so
  // the bond sub-fields show/hide as the operator interacts with the form.
  function wireInstallBondToggle(dlg) {
    const enable = dlg.querySelector("#mk-inst-bond-enable");
    const fields = dlg.querySelector("#mk-inst-bond-fields");
    const nicFs = dlg.querySelector("#mk-inst-nic-fieldset");
    const setBondVisuals = function () {
      if (fields) fields.hidden = !enable.checked;
      // Bond 启用时 NIC picker 不生效；用 disabled 视觉提示。
      if (nicFs) {
        if (enable && enable.checked) {
          nicFs.setAttribute("disabled", "disabled");
          nicFs.style.opacity = "0.55";
        } else {
          nicFs.removeAttribute("disabled");
          nicFs.style.opacity = "";
        }
      }
    };
    if (enable) {
      enable.addEventListener("change", setBondVisuals);
      setBondVisuals();
    }
    const mode = dlg.querySelector("#mk-inst-bond-mode");
    const primaryRow = dlg.querySelector("#mk-inst-bond-primary-row");
    const lacpRow = dlg.querySelector("#mk-inst-bond-lacp-row");
    const xmitRow = dlg.querySelector("#mk-inst-bond-xmit-row");
    if (mode) {
      mode.addEventListener("change", function () {
        const ab = mode.value === "active-backup";
        if (primaryRow) primaryRow.hidden = !ab;
        if (lacpRow) lacpRow.hidden = ab;
        if (xmitRow) xmitRow.hidden = ab;
      });
    }
  }

  // wireInstallNICPicker 让「auto / pick」radio 切换时显隐网卡表格。
  function wireInstallNICPicker(dlg) {
    const tbl = dlg.querySelector("#mk-inst-nic-table");
    const radios = dlg.querySelectorAll('input[name="mk-inst-nic-mode"]');
    if (!tbl || !radios.length) return;
    radios.forEach(function (r) {
      r.addEventListener("change", function () {
        const pick = (dlg.querySelector('input[name="mk-inst-nic-mode"]:checked') || {}).value === "pick";
        tbl.hidden = !pick;
      });
    });
  }

  async function submitInstallModal(dlg, uuid, opts, subnetByID) {
    const imageID = dlg.querySelector("#mk-inst-image").value.trim();
    const profileID = dlg.querySelector("#mk-inst-profile").value.trim();
    const hostname = dlg.querySelector("#mk-inst-hostname").value.trim();
    const staticAddr = dlg.querySelector("#mk-inst-static").value.trim();
    const subnetID = (dlg.querySelector("#mk-inst-subnet") || {}).value || "";
    // 密码字段不做 trim —— 用户故意带前后空格的极端情况也保留下来。
    const password = dlg.querySelector("#mk-inst-password").value;
    const state = (dlg.querySelector('input[name="mk-install-state"]:checked') || {}).value || "install";
    const diskChoice = (dlg.querySelector("#mk-inst-disk") || {}).value || "";

    if (!imageID || !profileID) {
      flashError("镜像与配置必填");
      return;
    }
    // 子网 + 静态地址联动校验：
    //   绑定 subnet → 静态地址可选（为空时后端自动分配），若填写则验证在 CIDR 范围内
    //   不绑定 subnet → 不做校验
    if (subnetID && staticAddr) {
      const hostOnly = staticAddr.split("/")[0];
      const s = subnetByID && subnetByID[subnetID];
      if (s) {
        const err = hostInSubnetCheck(hostOnly, s.cidr, s.gateway);
        if (err) {
          flashError(err);
          return;
        }
      }
    }
    if (password !== "" && password.length < 8) {
      flashError("Root 密码长度至少 8 个字符");
      return;
    }

    // Bond：弹窗是本次装机的 override 唯一真相源。复选框关 = 永远显式清除
    // （发 null），即使 defaults 里没传 bond——历史 bug 是「关掉 + 不发字段」
    // 导致旧 bond_override 残留，重装出来还是 bond。
    const bondEnabled = !!(dlg.querySelector("#mk-inst-bond-enable") || {}).checked;
    let bondField; // null = 清除；object = 设置
    if (bondEnabled) {
      const slaves = Array.prototype.slice.call(
        dlg.querySelectorAll(".mk-inst-bond-slave:checked")
      ).map(function (cb) { return cb.value; });
      if (slaves.length < 2) {
        flashError("Bond 至少选 2 块物理网卡");
        return;
      }
      if (slaves.length > 8) {
        flashError("Bond slaves 最多 8 块");
        return;
      }
      const mode = (dlg.querySelector("#mk-inst-bond-mode") || {}).value || "active-backup";
      const miimon = parseInt((dlg.querySelector("#mk-inst-bond-miimon") || {}).value, 10) || 100;
      const bondObj = { mode: mode, slaves: slaves, miimon: miimon };
      if (mode === "active-backup") {
        const primary = ((dlg.querySelector("#mk-inst-bond-primary") || {}).value || "").trim();
        if (primary !== "") {
          if (slaves.indexOf(primary) < 0) {
            flashError("主网卡必须在 slaves 列表中");
            return;
          }
          bondObj.primary = primary;
        }
      } else if (mode === "802.3ad") {
        bondObj.lacp_rate = (dlg.querySelector("#mk-inst-bond-lacp") || {}).value || "fast";
        bondObj.xmit_hash_policy = (dlg.querySelector("#mk-inst-bond-xmit") || {}).value || "layer3+4";
      }
      bondField = bondObj;
    } else {
      bondField = null;
    }

    // NIC selector：装机弹窗是 override 真相源。bond 启用时 spec 端忽略，
    // 这里仍然清掉 nic_selector_override（避免和 bond 共存的歧义存储）。
    //   bond 启用 → ""（NULL，沿用 profile）
    //   auto      → ""（NULL，沿用 profile，profile 一般是 "auto"）
    //   pick      → "by-mac:<MAC>"，用 radio 选中的 NIC
    let nicSelectorOverride = "";
    const nicMode = (dlg.querySelector('input[name="mk-inst-nic-mode"]:checked') || {}).value || "auto";
    if (!bondEnabled && nicMode === "pick") {
      const picked = (dlg.querySelector('.mk-inst-nic-pick:checked') || {}).value || "";
      if (!picked) {
        flashError("请在「承载网卡」里勾选一块物理网卡");
        return;
      }
      nicSelectorOverride = "by-mac:" + picked;
    }

    const saveBtn = dlg.querySelector("#mk-inst-save");
    saveBtn.disabled = true;
    try {
      const body = {
        image_id: imageID,
        profile_id: profileID,
        desired_state: state,
        hostname: hostname,
        static_address: staticAddr,
        subnet_id: subnetID, // "" = 显式清除 / 不绑定
        // 弹窗是 override 真相源：bond 永远显式发（null = 清除）；vlan_override
        // 目前弹窗里没有编辑 UI，所以永远发 0 = 沿用 subnet.vlan_id。这样不会
        // 残留以前手动设过的 vlan_override。
        bond: bondField,
        vlan_override: 0,
        // NIC 选择器永远显式发：空字符串 = 清除 override / 沿用 profile.network.nic_selector。
        nic_selector_override: nicSelectorOverride,
      };
      // 三态：未填 → 不带字段（沿用旧密文）；填了 → 带上明文覆盖。
      // 「清除」走详情页单独按钮，弹窗里不暴露，避免误清。
      if (password !== "") {
        body.root_password = password;
      }
      // 目标盘三态：
      //   ""         → 不带字段，保持当前 override 不动
      //   "__clear__"→ 显式发送 null，清除 override
      //   "smallest" → {mode:"smallest"}
      //   "by-wwn:.."→ {mode:"by-wwn", value:".."}
      //   "by-path:.."→ {mode:"by-path", value:".."}
      if (diskChoice === "__clear__") {
        body.target_disk = null;
      } else if (diskChoice === "smallest") {
        body.target_disk = { mode: "smallest" };
      } else if (diskChoice.indexOf("by-wwn:") === 0) {
        body.target_disk = { mode: "by-wwn", value: diskChoice.slice("by-wwn:".length) };
      } else if (diskChoice.indexOf("by-path:") === 0) {
        body.target_disk = { mode: "by-path", value: diskChoice.slice("by-path:".length) };
      }
      const updated = await apiSend("PUT", "/bindings/" + encodeURIComponent(uuid), body);
      closeModal();
      const label = state === "reinstall" ? "重装" : "装机";
      flashSuccess("已请求" + label);
      clearError();
      if (opts && typeof opts.onSuccess === "function") {
        try { opts.onSuccess(updated); } catch (_) { /* ignore */ }
      }
    } catch (e) {
      flashError(e.message);
    } finally {
      saveBtn.disabled = false;
    }
  }

  // openBMCDialog opens a modal for creating/editing BMC credentials for a
  // machine. Used by both the list page (sync button) and the detail page
  // (provisioning card). prefill = {ip:"10.0.0.10"} from agent report;
  // existing = the current Credential object (for edit) or null (for create).
  // onSaved(credential) is called after successful PUT, before closeModal.
  function openBMCDialog(uuid, prefill, existing, onSaved) {
    const base = { ip: "", port: 623, username: "", ipmi_interface: "lanplus", name: "" };
    const b = Object.assign({}, base, prefill || {}, existing || {});
    const ifaces = ["lanplus", "lan"];
    const ifaceOpts = ifaces.map(function (i) {
      const sel = i === (b.ipmi_interface || "lanplus") ? " selected" : "";
      return '<option value="' + i + '"' + sel + ">" + i + "</option>";
    }).join("");
    const pwdPlaceholder = existing ? "（留空保持现有值）" : "必填";

    const body =
      '<div class="kv-form">' +
      '<label class="kv"><span>机器 UUID</span>' +
        '<input type="text" value="' + escapeHTML(uuid) +
        '" readonly disabled class="mono"></label>' +
      '<label class="kv"><span>名称 <span class="muted">（可选别名）</span></span>' +
        '<input type="text" id="bmc-name" maxlength="64" value="' +
        escapeHTML(b.name || "") + '" autocomplete="off" placeholder="例如：rack01-r630-01"></label>' +
      '<label class="kv"><span>IP 地址</span>' +
        '<input type="text" id="bmc-ip" value="' + escapeHTML(b.ip || "") +
        '" required autocomplete="off"></label>' +
      '<label class="kv"><span>端口</span>' +
        '<input type="number" id="bmc-port" min="1" max="65535" value="' +
        escapeHTML(String(b.port || 623)) + '" autocomplete="off"></label>' +
      '<label class="kv"><span>用户名</span>' +
        '<input type="text" id="bmc-username" value="' +
        escapeHTML(b.username || "") + '" required autocomplete="off"></label>' +
      '<label class="kv"><span>密码</span>' +
        '<input type="password" id="bmc-password" placeholder="' + pwdPlaceholder +
        '" autocomplete="new-password"></label>' +
      '<label class="kv"><span>接口</span>' +
        '<select id="bmc-iface">' + ifaceOpts + "</select></label>" +
      "</div>";

    const footer =
      '<button type="button" class="btn btn-ghost" data-modal-close>取消</button> ' +
      '<button type="submit" id="bmc-save" class="btn btn-primary">' +
      (existing ? "保存 BMC" : "创建 BMC") + "</button>";

    const title = existing ? "编辑 BMC" : "配置 BMC";
    const dlg = openModal(modalShell(title, body, footer));
    dlg.querySelector("form").addEventListener("submit", async function (ev) {
      ev.preventDefault();
      const name = (dlg.querySelector("#bmc-name").value || "").trim();
      const ip = dlg.querySelector("#bmc-ip").value.trim();
      const portRaw = dlg.querySelector("#bmc-port").value.trim();
      const username = dlg.querySelector("#bmc-username").value.trim();
      const password = dlg.querySelector("#bmc-password").value;
      const iface = dlg.querySelector("#bmc-iface").value;

      if (!ip || !username) {
        flashError("IP 与用户名必填");
        return;
      }
      if (name.length > 64) {
        flashError("名称长度不能超过 64");
        return;
      }
      if (!existing && !password) {
        flashError("新建 BMC 时密码必填");
        return;
      }

      const reqBody = {
        name: name,
        ip: ip,
        username: username,
        ipmi_interface: iface,
      };
      if (portRaw) {
        const p = parseInt(portRaw, 10);
        if (!isNaN(p)) reqBody.port = p;
      }
      if (password) reqBody.password = password;

      const saveBtn = dlg.querySelector("#bmc-save");
      saveBtn.disabled = true;
      try {
        const c = await apiSend("PUT", "/bmc/" + encodeURIComponent(uuid), reqBody);
        closeModal();
        flashSuccess("BMC 已保存");
        clearError();
        if (typeof onSaved === "function") onSaved(c);
      } catch (e) {
        flashError(e.message);
      } finally {
        saveBtn.disabled = false;
      }
    });
  }

  window.MK = {
    API_BASE: API_BASE,
    escapeHTML: escapeHTML,
    fmtRelative: fmtRelative,
    fmtAbsolute: fmtAbsolute,
    fmtISO: fmtISO,
    fmtDuration: fmtDuration,
    fmtBytes: fmtBytes,
    truncate: truncate,
    POLL: POLL,
    withBusy: withBusy,
    wireCopyables: wireCopyables,
    apiGet: apiGet,
    apiSend: apiSend,
    openModal: openModal,
    closeModal: closeModal,
    modalShell: modalShell,
    flashSuccess: flashSuccess,
    flashError: flashError,
    clearError: clearError,
    copyText: copyText,
    basicAuthUser: basicAuthUser,
    logout: logout,
    loadCurrentUser: loadCurrentUser,
    openInstallModal: openInstallModal,
    openBMCDialog: openBMCDialog,
  };
})();
