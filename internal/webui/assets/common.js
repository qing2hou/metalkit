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
  // 表单字段：镜像 / 配置 / 主机名 / 静态地址（CIDR） / 期望状态 radio。
  // 提交时直接 PUT /bindings/{uuid} —— orchestrator 下一 tick 拾起。
  async function openInstallModal(uuid, opts) {
    opts = opts || {};
    const defaults = opts.defaults || {};
    let suggestState = opts.suggestState ||
      (defaults.desired_state === "reinstall" ? "reinstall" : "install");

    // 把目录预拉一遍。失败时退化成只显示 ID。机器报告失败时退化成"自动"。
    const [imgs, profs, report] = await Promise.all([
      apiGet("/images").catch(function () { return []; }),
      apiGet("/profiles").catch(function () { return []; }),
      apiGet("/machines/" + encodeURIComponent(uuid)).catch(function () { return null; }),
    ]);
    const images = (Array.isArray(imgs) ? imgs : []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    const profiles = (Array.isArray(profs) ? profs : []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
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

    const profOpts = ['<option value="">— 选择配置 —</option>'].concat(
      profiles.map(function (p) {
        const sel = p.id === defaults.profile_id ? " selected" : "";
        return '<option value="' + escapeHTML(p.id) + '"' + sel + ">" +
          escapeHTML(p.name || p.id) + "</option>";
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
      '<div class="kv-form">' +
      '<label class="kv"><span>镜像</span>' +
        '<select id="mk-inst-image" required>' + imgOpts + "</select></label>" +
      '<label class="kv"><span>配置</span>' +
        '<select id="mk-inst-profile" required>' + profOpts + "</select></label>" +
      '<label class="kv"><span>主机名</span>' +
        '<input type="text" id="mk-inst-hostname" value="' +
        escapeHTML(defaults.hostname || "") +
        '" placeholder="可选覆盖（留空用 profile 模板）" autocomplete="off"></label>' +
      '<label class="kv"><span>静态地址</span>' +
        '<input type="text" id="mk-inst-static" value="' +
        escapeHTML(defaults.static_address || "") +
        '" placeholder="可选 CIDR，如 192.168.10.50/24" autocomplete="off"></label>' +
      '<label class="kv"><span>Root 密码</span>' +
        '<input type="text" id="mk-inst-password" value="" autocomplete="off"' +
        ' placeholder="' +
        (defaults.has_password
          ? "已设置 —— 留空保留；输入新值覆盖"
          : "可选，留空则沿用 profile 默认 hash") +
        '"></label>' +
      '<label class="kv"><span>目标盘</span>' + diskSelect + "</label>" +
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
    const form = dlg.querySelector("form");
    form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      submitInstallModal(dlg, uuid, opts);
    });
  }

  // wireInstallBondToggle binds the enable-checkbox + mode-select events so
  // the bond sub-fields show/hide as the operator interacts with the form.
  function wireInstallBondToggle(dlg) {
    const enable = dlg.querySelector("#mk-inst-bond-enable");
    const fields = dlg.querySelector("#mk-inst-bond-fields");
    if (enable && fields) {
      enable.addEventListener("change", function () {
        fields.hidden = !enable.checked;
      });
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

  async function submitInstallModal(dlg, uuid, opts) {
    const imageID = dlg.querySelector("#mk-inst-image").value.trim();
    const profileID = dlg.querySelector("#mk-inst-profile").value.trim();
    const hostname = dlg.querySelector("#mk-inst-hostname").value.trim();
    const staticAddr = dlg.querySelector("#mk-inst-static").value.trim();
    // 密码字段不做 trim —— 用户故意带前后空格的极端情况也保留下来。
    const password = dlg.querySelector("#mk-inst-password").value;
    const state = (dlg.querySelector('input[name="mk-install-state"]:checked') || {}).value || "install";
    const diskChoice = (dlg.querySelector("#mk-inst-disk") || {}).value || "";

    if (!imageID || !profileID) {
      flashError("镜像与配置必填");
      return;
    }
    if (password !== "" && password.length < 8) {
      flashError("Root 密码长度至少 8 个字符");
      return;
    }

    // Bond 三态读取：
    //   checkbox 开                                 → 组装 bond 对象（要求 ≥2 slaves）
    //   checkbox 关 + 之前 defaults.bond 有值       → 发 null 显式清除 override
    //   checkbox 关 + 之前没有 bond override        → 不带字段，保持原状
    const defaultsBond = (opts && opts.defaults && opts.defaults.bond) || null;
    const bondEnabled = !!(dlg.querySelector("#mk-inst-bond-enable") || {}).checked;
    let bondField; // undefined = 不发；null = 清除；object = 设置
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
    } else if (defaultsBond) {
      bondField = null;
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
      if (bondField !== undefined) {
        body.bond = bondField;
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

  window.MK = {
    API_BASE: API_BASE,
    escapeHTML: escapeHTML,
    fmtRelative: fmtRelative,
    fmtAbsolute: fmtAbsolute,
    fmtISO: fmtISO,
    fmtDuration: fmtDuration,
    truncate: truncate,
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
  };
})();
