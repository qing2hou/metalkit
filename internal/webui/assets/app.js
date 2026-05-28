// metalkit operator UI — single-file vanilla JS app.
// Two modes: list (machine table) and detail (one machine + reports history).
// Mode is selected from window.location.pathname.

(function () {
  "use strict";

  const { escapeHTML, fmtRelative, fmtAbsolute, fmtISO, fmtBytes, apiGet, flashError, clearError, copyText, wireCopyables, POLL } = window.MK;

  const page = document.body.dataset.page;

  document.addEventListener("DOMContentLoaded", function () {
    wireRefreshButton();
    if (page === "list") {
      initList();
    } else if (page === "detail") {
      initDetail();
    }
  });

  // ----- shared helpers -------------------------------------------------

  function $(id) { return document.getElementById(id); }

  function humanStatus(s) {
    if (!s) return "unknown";
    const v = String(s).toLowerCase();
    if (v === "online" || v === "offline" || v === "unknown") return v;
    return v;
  }

  function truncateUUID(u) {
    if (!u) return "-";
    if (u.length <= 12) return u;
    return u.slice(0, 8) + "…" + u.slice(-4);
  }

  function wireRefreshButton() {
    const btn = $("refresh-btn");
    if (!btn) return;
    btn.addEventListener("click", function () {
      if (page === "list") loadMachines();
      else if (page === "detail") reloadDetail();
    });
  }

  // ----- list mode ------------------------------------------------------

  let listTimer = null;
  let countdownTimer = null;
  let nextRefreshAt = 0;

  function initList() {
    loadMachines();
    scheduleListRefresh();
    // jobs.js pattern: pause poll when tab hidden so background tabs don't burn cycles.
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") {
        loadMachines();
        scheduleListRefresh();
      } else {
        if (listTimer) { clearTimeout(listTimer); listTimer = null; }
        if (countdownTimer) { clearInterval(countdownTimer); countdownTimer = null; }
      }
    });
  }

  function scheduleListRefresh() {
    if (listTimer) clearTimeout(listTimer);
    if (countdownTimer) clearInterval(countdownTimer);
    nextRefreshAt = Date.now() + POLL.LIST_MS;
    updateCountdown();
    countdownTimer = setInterval(updateCountdown, 1000);
    listTimer = setTimeout(function () {
      loadMachines();
      scheduleListRefresh();
    }, POLL.LIST_MS);
  }

  function updateCountdown() {
    const el = $("refresh-countdown");
    if (!el) return;
    const secs = Math.max(0, Math.ceil((nextRefreshAt - Date.now()) / 1000));
    el.textContent = "下次刷新 " + secs + " 秒";
  }

  async function loadMachines() {
    clearError();
    try {
      const data = await apiGet("/machines");
      renderMachines(Array.isArray(data) ? data : []);
    } catch (e) {
      flashError(e.message);
      $("machines-loading").hidden = true;
    }
  }

  function renderMachines(rows) {
    $("machines-loading").hidden = true;
    const empty = $("machines-empty");
    const wrap = $("machines-wrap");
    const count = $("machines-count");

    if (!rows || rows.length === 0) {
      empty.hidden = false;
      wrap.hidden = true;
      count.textContent = "";
      return;
    }
    empty.hidden = true;
    wrap.hidden = false;
    count.textContent = "共 " + rows.length + " 台机器";

    // sort by last_seen desc; stable secondary sort by uuid
    rows.sort(function (a, b) {
      const lb = Number(b.last_seen || 0) - Number(a.last_seen || 0);
      if (lb !== 0) return lb;
      return String(a.uuid || "").localeCompare(String(b.uuid || ""));
    });

    const tbody = $("machines-tbody");
    tbody.innerHTML = "";
    for (const row of rows) {
      const tr = document.createElement("tr");
      const status = humanStatus(row.status);
      const mfgProd = [row.manufacturer, row.product_name].filter(Boolean).join(" / ") || "-";
      const uuid = row.uuid || "";
      const lastSeen = Number(row.last_seen || 0);
      const lastSeenTitle = fmtAbsolute(lastSeen);

      tr.innerHTML =
        '<td><span class="status-dot" data-status="' + escapeHTML(status) + '" title="' + escapeHTML(status) + '"></span></td>' +
        '<td>' + escapeHTML(row.serial || "-") + '</td>' +
        '<td>' + escapeHTML(mfgProd) + '</td>' +
        '<td><span class="mono copyable" data-copy="' + escapeHTML(uuid) + '" title="' + escapeHTML(uuid) + '（点击复制）">' + escapeHTML(truncateUUID(uuid)) + '</span></td>' +
        '<td title="' + escapeHTML(lastSeenTitle) + '">' + escapeHTML(fmtRelative(lastSeen)) + '</td>' +
        '<td class="col-actions">' +
          '<a href="/ui/m/' + encodeURIComponent(uuid) + '" class="btn btn-ghost btn-sm">详情</a>' +
          '<button type="button" class="btn btn-primary btn-sm install-row-btn" data-uuid="' +
          escapeHTML(uuid) + '">装机/重装</button>' +
          '<button type="button" class="btn btn-danger btn-sm delete-row-btn" data-uuid="' +
          escapeHTML(uuid) + '" data-serial="' + escapeHTML(row.serial || "") + '">删除</button>' +
        '</td>';
      tbody.appendChild(tr);
    }

    wireCopyables(tbody);

    // wire 装机/重装 buttons —— 拉 binding 预填，再开统一弹窗。
    tbody.querySelectorAll(".install-row-btn").forEach(function (btn) {
      btn.addEventListener("click", function () {
        triggerListInstall(btn.dataset.uuid || "");
      });
    });

    // wire 删除 buttons —— 二次确认，DELETE，刷新列表。
    tbody.querySelectorAll(".delete-row-btn").forEach(function (btn) {
      btn.addEventListener("click", function () {
        triggerDeleteMachine(btn.dataset.uuid || "", btn.dataset.serial || "");
      });
    });
  }

  async function triggerDeleteMachine(uuid, serial) {
    if (!uuid || !window.MK) return;
    const label = serial ? (serial + "（" + truncateUUID(uuid) + "）") : uuid;
    const ok = window.confirm(
      "确认删除机器 " + label + "？\n\n" +
      "将一并清除：硬件报告历史、BMC 凭据、binding、作业历史。\n" +
      "存在 pending/running 作业时会被拒绝。\n" +
      "机器若再次 PXE 上线会作为新机器入库。"
    );
    if (!ok) return;
    try {
      await MK.apiSend("DELETE", "/machines/" + encodeURIComponent(uuid));
      MK.flashSuccess("已删除 " + label);
      await loadMachines();
    } catch (e) {
      if (e && e.status === 409) {
        MK.flashError("删除被拒绝：" + (e.message || "存在未完成作业，请先取消"));
      } else {
        MK.flashError("删除失败：" + (e.message || e));
      }
    }
  }

  async function triggerListInstall(uuid) {
    if (!uuid || !window.MK || !MK.openInstallModal) return;
    // 拉一次 binding 预填弹窗。404 → 让弹窗显示空表单。
    let binding = null;
    try {
      binding = await MK.apiGet("/bindings/" + encodeURIComponent(uuid));
    } catch (e) {
      if (!(e && e.status === 404)) {
        MK.flashError("读取 binding 失败：" + e.message);
        return;
      }
    }
    // 顺手看一眼是否有过 succeeded 作业；有则默认重装。
    let suggestState = "install";
    try {
      const js = await MK.apiGet("/jobs?machine_uuid=" + encodeURIComponent(uuid) + "&limit=20");
      const arr = Array.isArray(js) ? js : [];
      if (arr.some(function (j) { return j.status === "succeeded"; })) {
        suggestState = "reinstall";
      }
    } catch (_) { /* 拉不到就按 install */ }

    await MK.openInstallModal(uuid, {
      defaults: {
        image_id: binding ? binding.image_id : "",
        profile_id: binding ? binding.profile_id : "",
        hostname: binding ? binding.hostname : "",
        static_address: binding ? binding.static_address : "",
        subnet_id: binding ? (binding.subnet_id || "") : "",
        bond: binding ? (binding.bond || null) : null,
        nic_selector_override: binding ? (binding.nic_selector_override || "") : "",
        desired_state: suggestState,
      },
      suggestState: suggestState,
      // 列表页不需要立即刷新单行，下次轮询自然带出。
    });
  }

  // ----- detail mode ----------------------------------------------------

  let currentUUID = "";
  let currentReportID = null; // numeric id or null for latest

  function initDetail() {
    const m = window.location.pathname.match(/\/ui\/m\/([^/]+)\/?$/);
    if (!m) {
      flashError("无效的详情 URL：" + window.location.pathname);
      return;
    }
    currentUUID = decodeURIComponent(m[1]);

    const params = new URLSearchParams(window.location.search);
    const r = params.get("report");
    if (r) {
      const n = parseInt(r, 10);
      if (!isNaN(n)) currentReportID = n;
    }

    const raw = $("raw-toggle");
    if (raw) {
      raw.addEventListener("change", function () {
        $("raw-section").hidden = !raw.checked;
      });
    }

    loadDetail();
  }

  function reloadDetail() {
    loadDetail();
  }

  async function loadDetail() {
    clearError();
    try {
      const [machine, reports] = await Promise.all([
        apiGet("/machines/" + encodeURIComponent(currentUUID)),
        apiGet("/machines/" + encodeURIComponent(currentUUID) + "/reports"),
      ]);
      renderMachineHeader(machine);
      renderReportsList(Array.isArray(reports) ? reports : []);

      // Pick which report to fetch.
      let targetID = currentReportID;
      if (targetID == null) {
        if (machine && machine.latest_report_id) {
          targetID = machine.latest_report_id;
        } else if (Array.isArray(reports) && reports.length > 0) {
          targetID = reports[0].id;
        }
      }
      if (targetID == null) {
        $("report-loading").hidden = true;
        flashError("该机器尚无上报记录");
        return;
      }
      currentReportID = targetID;
      highlightActiveReport(targetID);
      const report = await apiGet("/machines/" + encodeURIComponent(currentUUID) + "/reports/" + encodeURIComponent(targetID));
      renderReport(report);
    } catch (e) {
      flashError(e.message);
      $("machine-header-loading").hidden = true;
      $("reports-loading").hidden = true;
      $("report-loading").hidden = true;
    }
  }

  function renderMachineHeader(m) {
    $("machine-header-loading").hidden = true;
    $("machine-header-body").hidden = false;
    if (!m) return;

    const status = humanStatus(m.status);
    $("m-status-dot").dataset.status = status;
    $("m-status-badge").dataset.status = status;
    $("m-status-badge").textContent = status;

    $("m-product").textContent = [m.manufacturer, m.product_name].filter(Boolean).join(" ") || (m.serial || m.uuid || "machine");
    $("m-serial").textContent = m.serial || "-";
    $("m-serial").dataset.copy = m.serial || "";
    $("m-uuid").textContent = m.uuid || "-";
    $("m-uuid").dataset.copy = m.uuid || "";
    $("m-manufacturer").textContent = m.manufacturer || "-";

    $("m-first-seen").textContent = fmtRelative(m.first_seen);
    $("m-first-seen").title = fmtAbsolute(m.first_seen);
    $("m-last-seen").textContent = fmtRelative(m.last_seen);
    $("m-last-seen").title = fmtAbsolute(m.last_seen);

    const stale = $("m-stale-badge");
    if (stale) {
      const lastSeen = Number(m.last_seen || 0);
      const ageSec = Math.floor(Date.now() / 1000 - lastSeen);
      stale.hidden = !(lastSeen > 0 && ageSec > 24 * 3600);
      if (!stale.hidden) stale.title = "上次上报已过 " + Math.floor(ageSec / 3600) + " 小时，数据可能过期";
    }

    // The detail header shows "report timestamp" once the report itself loads.
    // For now, leave it as a dash.
    $("m-report-ts").textContent = "-";

    // MACs
    const macsEl = $("m-macs");
    macsEl.innerHTML = "";
    const macs = Array.isArray(m.macs) ? m.macs : [];
    if (macs.length === 0) {
      macsEl.textContent = "-";
    } else {
      for (const entry of macs) {
        const span = document.createElement("span");
        span.className = "mac-chip copyable";
        span.dataset.copy = entry.mac || "";
        span.title = "点击复制 MAC";
        span.textContent = entry.mac || "?";
        if (entry.role) {
          const r = document.createElement("span");
          r.className = "mac-role";
          r.textContent = entry.role;
          span.appendChild(r);
        }
        macsEl.appendChild(span);
      }
    }

    wireCopyables(document.getElementById("machine-header"));
  }

  function renderReportsList(reports) {
    $("reports-loading").hidden = true;
    const list = $("reports-list");
    list.hidden = false;
    list.innerHTML = "";
    if (reports.length === 0) {
      const li = document.createElement("li");
      li.className = "muted";
      li.textContent = "无上报记录";
      list.appendChild(li);
      return;
    }
    for (const r of reports) {
      const li = document.createElement("li");
      li.dataset.reportId = String(r.id);
      const ts = Number(r.ts || 0);
      li.innerHTML =
        '<div>' + escapeHTML(fmtRelative(ts)) + '</div>' +
        '<div class="rid">id ' + escapeHTML(String(r.id)) + ' &middot; ' + escapeHTML(fmtAbsolute(ts)) + '</div>';
      li.addEventListener("click", function () {
        currentReportID = r.id;
        const url = new URL(window.location.href);
        url.searchParams.set("report", String(r.id));
        window.history.replaceState({}, "", url.toString());
        loadDetail();
      });
      list.appendChild(li);
    }
  }

  function highlightActiveReport(id) {
    const list = $("reports-list");
    if (!list) return;
    list.querySelectorAll("li").forEach(function (li) {
      li.classList.toggle("active", li.dataset.reportId === String(id));
    });
  }

  // ----- report rendering ----------------------------------------------

  function renderReport(report) {
    $("report-loading").hidden = true;
    $("report-body").hidden = false;
    if (!report || typeof report !== "object") {
      flashError("上报数据为空");
      return;
    }

    // Header timestamp pulled from the report itself.
    if (report.collected_at) {
      $("m-report-ts").textContent = fmtISO(report.collected_at);
      $("m-report-ts").title = String(report.collected_at);
    }

    renderMachineSection($("sec-machine"), report.machine);
    renderFirmwareSection($("sec-firmware"), report.firmware);
    renderCPUSection($("sec-cpu"), report.cpu);
    renderMemorySection($("sec-memory"), report.memory);
    renderDisksSection($("sec-disks"), report.disks);
    renderNICsSection($("sec-nics"), report.nics);
    renderPCISection($("sec-pci"), report.pci_devices);
    renderAccelSection($("sec-accel"), report.accelerators);
    renderBMCSection($("sec-bmc"), report.bmc);
    renderSensorsSection($("sec-sensors"), report.sensors);
    renderSystemSection($("sec-system"), report.system);
    renderAgentSection($("sec-agent"), report.agent, report);

    $("raw-json").textContent = JSON.stringify(report, null, 2);
  }

  // kv: render an object as a definition list of non-empty fields.
  // `fields` is an array of [key, label] tuples; values pulled from obj.
  function kv(obj, fields) {
    if (!obj) return '<div class="empty-sub">不可用</div>';
    let body = "";
    for (const f of fields) {
      const key = f[0], label = f[1];
      let val = obj[key];
      if (f[2]) val = f[2](val);
      if (val === undefined || val === null || val === "" || val === false && f[3] !== "showBool") continue;
      body += '<dt>' + escapeHTML(label) + '</dt><dd>' + escapeHTML(String(val)) + '</dd>';
    }
    if (!body) return '<div class="empty-sub">无数据</div>';
    return '<dl class="kv">' + body + '</dl>';
  }

  // table: render an array of objects as a small table.
  // columns: [{key, label, fmt?}]
  function table(rows, columns) {
    if (!rows || rows.length === 0) return '<div class="empty-sub">无</div>';
    let head = "<thead><tr>";
    for (const c of columns) head += "<th>" + escapeHTML(c.label) + "</th>";
    head += "</tr></thead>";
    let body = "<tbody>";
    for (const row of rows) {
      body += "<tr>";
      for (const c of columns) {
        let v = row ? row[c.key] : null;
        if (c.fmt) v = c.fmt(v, row);
        if (v === undefined || v === null) v = "";
        body += "<td>" + escapeHTML(String(v)) + "</td>";
      }
      body += "</tr>";
    }
    body += "</tbody>";
    return '<table class="sub-table">' + head + body + "</table>";
  }

  function fmtBool(v) {
    if (v === true) return "是";
    if (v === false) return "否";
    return "";
  }

  function renderMachineSection(el, m) {
    if (!m) { el.innerHTML = '<div class="empty-sub">不可用</div>'; return; }
    let html = "";
    html += '<div class="sub-head">身份</div>';
    html += kv(m, [
      ["smbios_uuid", "SMBIOS UUID"],
      ["manufacturer", "制造商"],
      ["product_name", "产品型号"],
      ["version", "版本"],
      ["serial", "序列号"],
      ["sku", "SKU"],
      ["family", "家族"],
    ]);
    html += '<div class="sub-head">主板</div>';
    html += kv(m.baseboard, [
      ["manufacturer", "制造商"],
      ["product", "产品"],
      ["version", "版本"],
      ["serial", "序列号"],
      ["asset_tag", "资产标签"],
    ]);
    html += '<div class="sub-head">机箱</div>';
    html += kv(m.chassis, [
      ["manufacturer", "制造商"],
      ["type", "类型"],
      ["serial", "序列号"],
      ["asset_tag", "资产标签"],
    ]);
    el.innerHTML = html;
  }

  function renderFirmwareSection(el, f) {
    if (!f) { el.innerHTML = '<div class="empty-sub">不可用</div>'; return; }
    let html = "";
    html += '<div class="sub-head">BIOS</div>';
    html += kv(f.bios, [
      ["vendor", "厂商"],
      ["version", "版本"],
      ["release_date", "发布日期"],
      ["revision", "修订"],
    ]);
    html += '<div class="sub-head">UEFI / Secure Boot</div>';
    html += kv(f, [
      ["uefi_mode", "UEFI 模式", fmtBool, "showBool"],
      ["secure_boot", "Secure Boot", fmtBool, "showBool"],
    ]);
    if (f.tpm) {
      html += '<div class="sub-head">TPM</div>';
      html += kv(f.tpm, [
        ["present", "存在", fmtBool, "showBool"],
        ["version", "版本"],
        ["manufacturer", "制造商"],
      ]);
    }
    el.innerHTML = html;
  }

  function renderCPUSection(el, c) {
    if (!c) { el.innerHTML = '<div class="empty-sub">不可用</div>'; return; }
    let html = "";
    html += '<div class="sub-head">拓扑</div>';
    html += kv(c, [
      ["vendor", "厂商"],
      ["arch", "架构"],
      ["sockets", "插槽数"],
      ["total_cores", "总核心数"],
      ["total_threads", "总线程数"],
      ["numa_nodes", "NUMA 节点数"],
    ]);
    html += '<div class="sub-head">分插槽</div>';
    html += table(c.per_socket, [
      { key: "socket", label: "插槽" },
      { key: "model", label: "型号" },
      { key: "cores", label: "核心" },
      { key: "threads", label: "线程" },
      { key: "base_freq_mhz", label: "基准 MHz" },
      { key: "max_freq_mhz", label: "最大 MHz" },
      { key: "microcode", label: "微码" },
      { key: "l1_kb", label: "L1 KB" },
      { key: "l2_kb", label: "L2 KB" },
      { key: "l3_kb", label: "L3 KB" },
    ]);
    if (Array.isArray(c.flags) && c.flags.length > 0) {
      html += '<div class="sub-head">特性标志（共 ' + c.flags.length + '）</div>';
      html += '<div class="mono" style="font-size:11px;word-break:break-all;">' + escapeHTML(c.flags.join(" ")) + '</div>';
    }
    el.innerHTML = html;
  }

  function renderMemorySection(el, m) {
    if (!m) { el.innerHTML = '<div class="empty-sub">不可用</div>'; return; }
    let html = "";
    html += '<div class="sub-head">汇总</div>';
    html += kv(m, [
      ["total_bytes", "总量", fmtBytes],
      ["available_bytes", "可用", fmtBytes],
      ["ecc", "ECC", fmtBool, "showBool"],
    ]);
    html += '<div class="sub-head">DIMM</div>';
    html += table(m.dimms, [
      { key: "locator", label: "位置" },
      { key: "bank", label: "Bank" },
      { key: "size_bytes", label: "容量", fmt: fmtBytes },
      { key: "type", label: "类型" },
      { key: "speed_mts", label: "速率 MT/s" },
      { key: "configured_speed_mts", label: "配置 MT/s" },
      { key: "manufacturer", label: "制造商" },
      { key: "part_number", label: "型号" },
      { key: "serial", label: "序列号" },
      { key: "rank", label: "Rank" },
      { key: "voltage", label: "电压 V" },
    ]);
    el.innerHTML = html;
  }

  function renderDisksSection(el, disks) {
    if (!disks || disks.length === 0) { el.innerHTML = '<div class="empty-sub">无</div>'; return; }
    let html = "";
    html += table(disks, [
      { key: "kname", label: "名称" },
      { key: "type", label: "类型" },
      { key: "size_bytes", label: "容量", fmt: fmtBytes },
      { key: "model", label: "型号" },
      { key: "serial", label: "序列号" },
      { key: "transport", label: "传输" },
      { key: "rotational", label: "机械", fmt: fmtBool },
      { key: "firmware", label: "固件" },
      { key: "pci_address", label: "PCI" },
    ]);
    // SMART summary for any disk that reports it
    const smartRows = disks.filter(function (d) { return d && d.smart; }).map(function (d) {
      return Object.assign({ kname: d.kname }, d.smart);
    });
    if (smartRows.length > 0) {
      html += '<div class="sub-head">SMART</div>';
      html += table(smartRows, [
        { key: "kname", label: "磁盘" },
        { key: "health", label: "健康" },
        { key: "power_on_hours", label: "通电小时" },
        { key: "power_cycles", label: "通电次数" },
        { key: "temperature_c", label: "温度 °C" },
        { key: "bytes_read", label: "已读", fmt: fmtBytes },
        { key: "bytes_written", label: "已写", fmt: fmtBytes },
        { key: "reallocated_sectors", label: "重映射扇区" },
        { key: "pending_sectors", label: "待映射扇区" },
        { key: "smart_errors", label: "错误数" },
      ]);
    }
    el.innerHTML = html;
  }

  function renderNICsSection(el, nics) {
    if (!nics || nics.length === 0) { el.innerHTML = '<div class="empty-sub">无</div>'; return; }
    let html = "";
    html += table(nics, [
      { key: "name", label: "名称" },
      { key: "mac", label: "MAC" },
      { key: "permanent_mac", label: "永久 MAC" },
      { key: "link", label: "Link", fmt: fmtBool },
      { key: "speed_mbps", label: "Mbps" },
      { key: "duplex", label: "双工" },
      { key: "mtu", label: "MTU" },
      { key: "driver", label: "驱动" },
      { key: "firmware_version", label: "固件" },
      { key: "pci_address", label: "PCI" },
      { key: "addresses", label: "地址", fmt: function (v) { return Array.isArray(v) ? v.join(", ") : ""; } },
    ]);
    const sfps = nics.filter(function (n) { return n && n.sfp; }).map(function (n) {
      return Object.assign({ name: n.name }, n.sfp);
    });
    if (sfps.length > 0) {
      html += '<div class="sub-head">SFP 模块</div>';
      html += table(sfps, [
        { key: "name", label: "网卡" },
        { key: "vendor", label: "厂商" },
        { key: "part_number", label: "型号" },
        { key: "serial", label: "序列号" },
        { key: "type", label: "类型" },
        { key: "wavelength_nm", label: "波长 nm" },
      ]);
    }
    el.innerHTML = html;
  }

  function renderPCISection(el, devices) {
    if (!devices || devices.length === 0) { el.innerHTML = '<div class="empty-sub">无</div>'; return; }
    el.innerHTML = table(devices, [
      { key: "address", label: "地址" },
      { key: "class_name", label: "类别" },
      { key: "vendor_name", label: "厂商" },
      { key: "device_name", label: "设备" },
      { key: "driver", label: "驱动" },
      { key: "link_speed", label: "链路速率" },
      { key: "link_width", label: "链路宽度" },
      { key: "kernel_modules", label: "内核模块", fmt: function (v) { return Array.isArray(v) ? v.join(", ") : ""; } },
    ]);
  }

  function renderAccelSection(el, accels) {
    if (!accels || accels.length === 0) { el.innerHTML = '<div class="empty-sub">无</div>'; return; }
    el.innerHTML = table(accels, [
      { key: "pci_address", label: "PCI" },
      { key: "vendor", label: "厂商" },
      { key: "model", label: "型号" },
      { key: "class", label: "类别" },
      { key: "vram_bytes", label: "显存", fmt: fmtBytes },
      { key: "driver", label: "驱动" },
    ]);
  }

  function renderBMCSection(el, bmc) {
    if (!bmc) { el.innerHTML = '<div class="empty-sub">无 BMC</div>'; return; }
    let html = "";
    html += '<div class="sub-head">控制器</div>';
    html += kv(bmc, [
      ["vendor", "厂商"],
      ["product_id", "Product ID"],
      ["firmware_version", "固件版本"],
      ["ipmi_version", "IPMI 版本"],
      ["mac", "MAC"],
      ["ip", "IP"],
      ["gateway", "网关"],
      ["subnet", "子网"],
      ["vlan_id", "VLAN"],
    ]);
    if (bmc.fru) {
      html += '<div class="sub-head">FRU</div>';
      html += kv(bmc.fru, [
        ["board_mfg", "主板制造商"],
        ["board_product", "主板产品"],
        ["board_serial", "主板序列号"],
        ["product_serial", "产品序列号"],
      ]);
    }
    el.innerHTML = html;
  }

  function renderSensorsSection(el, sensors) {
    if (!sensors || sensors.length === 0) { el.innerHTML = '<div class="empty-sub">无</div>'; return; }
    el.innerHTML = table(sensors, [
      { key: "name", label: "名称" },
      { key: "type", label: "类型" },
      { key: "value", label: "数值" },
      { key: "unit", label: "单位" },
      { key: "status", label: "状态" },
      { key: "threshold_min", label: "最小" },
      { key: "threshold_max", label: "最大" },
    ]);
  }

  function renderSystemSection(el, s) {
    if (!s) { el.innerHTML = '<div class="empty-sub">不可用</div>'; return; }
    let html = "";
    html += '<div class="sub-head">内核与 OS</div>';
    html += kv(s, [
      ["kernel_release", "内核"],
      ["hostname", "主机名"],
      ["live_image_version", "Live 镜像"],
      ["boot_time", "启动时间", fmtAbsolute],
      ["uptime_seconds", "运行时长", function (v) {
        if (!v) return "";
        const n = Number(v);
        if (!isFinite(n) || n <= 0) return "";
        const d = Math.floor(n / 86400);
        const h = Math.floor((n % 86400) / 3600);
        const m = Math.floor((n % 3600) / 60);
        return d + " 天 " + h + " 时 " + m + " 分";
      }],
      ["kernel_cmdline", "内核参数"],
    ]);
    if (Array.isArray(s.mounts) && s.mounts.length > 0) {
      html += '<div class="sub-head">挂载</div>';
      html += table(s.mounts, [
        { key: "source", label: "源" },
        { key: "target", label: "挂载点" },
        { key: "fstype", label: "文件系统" },
        { key: "opts", label: "选项" },
      ]);
    }
    el.innerHTML = html;
  }

  function renderAgentSection(el, a, report) {
    let html = "";
    html += '<div class="sub-head">上报元数据</div>';
    html += kv(report, [
      ["schema_version", "Schema 版本"],
      ["agent_version", "Agent 版本"],
      ["collected_at", "采集时间", fmtISO],
      ["collection_duration_ms", "采集耗时 ms"],
    ]);
    if (a) {
      html += '<div class="sub-head">Agent</div>';
      html += kv(a, [
        ["version", "版本"],
        ["collected_at", "采集时间", fmtISO],
        ["collection_duration_ms", "采集耗时 ms"],
      ]);
      if (Array.isArray(a.errors) && a.errors.length > 0) {
        html += '<div class="sub-head">采集器错误</div>';
        html += table(a.errors, [
          { key: "collector", label: "采集器" },
          { key: "err", label: "错误" },
        ]);
      }
    }
    el.innerHTML = html;
  }
})();
