// metalkit operator UI - jobs list page.
//
// Behaviour:
//   - Filter bar (machine_uuid substring + status chips + last-24h checkbox)
//     is mirrored in the URL query string so reload preserves state.
//   - Poll /api/v1/jobs every 5s while the tab is visible; pause when hidden.
//   - Build a machine_uuid -> {product} lookup once from /api/v1/machines so
//     the table can show a friendly product tag alongside the uuid8.
//   - Cancel is offered for pending|running jobs only; confirms then POSTs.
//
// API shape: /api/v1/jobs returns a plain JSON array of jobs (see
// internal/jobs/api.go; writeJSON of []Job). We tolerate {jobs:[...]} too
// in case the response wrapper is added later.

(function () {
  "use strict";

  const POLL_MS = MK.POLL.JOBS_MS;
  const KNOWN_STATUSES = ["pending", "running", "succeeded", "failed", "cancelled"];

  let pollTimer = null;
  let machineByUUID = {};   // uuid -> {uuid, product, ...}
  let latestJobs = [];      // last fetched list (unfiltered)
  let filters = {
    machine: "",
    status: new Set(),
    recent: false,
  };

  document.addEventListener("DOMContentLoaded", function () {
    parseFiltersFromURL();
    wireUI();
    refreshMachines().finally(function () {
      tick();           // first fetch immediately
      schedulePoll();
    });
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") {
        tick();
        schedulePoll();
      } else {
        cancelPoll();
      }
    });
  });

  // ---- filters / URL --------------------------------------------------

  function parseFiltersFromURL() {
    const q = new URLSearchParams(window.location.search);
    filters.machine = (q.get("machine_uuid") || "").trim().toLowerCase();
    const st = (q.get("status") || "").split(",").map(function (s) { return s.trim(); }).filter(Boolean);
    filters.status = new Set();
    for (const s of st) {
      if (KNOWN_STATUSES.indexOf(s) !== -1) filters.status.add(s);
    }
    filters.recent = q.get("recent") === "1";
  }

  function writeFiltersToURL() {
    const q = new URLSearchParams();
    if (filters.machine) q.set("machine_uuid", filters.machine);
    if (filters.status.size > 0) {
      q.set("status", Array.from(filters.status).join(","));
    }
    if (filters.recent) q.set("recent", "1");
    const qs = q.toString();
    const newURL = window.location.pathname + (qs ? "?" + qs : "");
    window.history.replaceState(null, "", newURL);
  }

  function wireUI() {
    const machineInput = document.getElementById("filter-machine");
    machineInput.value = filters.machine;
    machineInput.addEventListener("input", function () {
      filters.machine = machineInput.value.trim().toLowerCase();
      writeFiltersToURL();
      render();
    });

    const chips = document.querySelectorAll("#filter-status .chip");
    chips.forEach(function (chip) {
      const s = chip.dataset.status;
      if (filters.status.has(s)) chip.classList.add("active");
      chip.addEventListener("click", function () {
        if (filters.status.has(s)) {
          filters.status.delete(s);
          chip.classList.remove("active");
        } else {
          filters.status.add(s);
          chip.classList.add("active");
        }
        writeFiltersToURL();
        render();
      });
    });

    const recent = document.getElementById("filter-recent");
    recent.checked = filters.recent;
    recent.addEventListener("change", function () {
      filters.recent = recent.checked;
      writeFiltersToURL();
      render();
    });

    document.getElementById("filter-clear").addEventListener("click", function () {
      filters.machine = "";
      filters.status = new Set();
      filters.recent = false;
      machineInput.value = "";
      recent.checked = false;
      chips.forEach(function (c) { c.classList.remove("active"); });
      writeFiltersToURL();
      render();
    });

    document.getElementById("refresh-btn").addEventListener("click", function () {
      tick();
    });

    const purgeBtn = document.getElementById("purge-btn");
    if (purgeBtn) {
      purgeBtn.addEventListener("click", function () { purgeAll(purgeBtn); });
    }
  }

  // ---- polling --------------------------------------------------------

  function schedulePoll() {
    cancelPoll();
    pollTimer = setInterval(tick, POLL_MS);
  }

  function cancelPoll() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  async function tick() {
    try {
      const resp = await MK.apiGet("/jobs");
      const list = normaliseJobs(resp);
      latestJobs = list;
      MK.clearError();
      render();
    } catch (err) {
      MK.flashError("加载作业失败：" + err.message);
      document.getElementById("jobs-loading").hidden = true;
    }
  }

  async function refreshMachines() {
    try {
      const resp = await MK.apiGet("/machines");
      const list = Array.isArray(resp) ? resp : (resp && resp.machines) || [];
      machineByUUID = {};
      for (const m of list) {
        if (m && m.uuid) machineByUUID[String(m.uuid).toLowerCase()] = m;
      }
    } catch (err) {
      // Non-fatal — the table just shows uuid8 without a product name.
      machineByUUID = {};
    }
  }

  function normaliseJobs(resp) {
    if (Array.isArray(resp)) return resp;
    if (resp && Array.isArray(resp.jobs)) return resp.jobs;
    return [];
  }

  // ---- rendering ------------------------------------------------------

  function applyFilters(list) {
    const cutoff = Date.now() / 1000 - 86400;
    return list.filter(function (j) {
      if (filters.machine && String(j.machine_uuid || "").toLowerCase().indexOf(filters.machine) === -1) {
        return false;
      }
      if (filters.status.size > 0 && !filters.status.has(j.status)) return false;
      if (filters.recent) {
        const ts = unixOf(j.created_at);
        if (ts == null || ts < cutoff) return false;
      }
      return true;
    });
  }

  function unixOf(iso) {
    if (iso == null) return null;
    if (typeof iso === "number") return iso;
    const t = Date.parse(iso);
    if (!isFinite(t)) return null;
    return Math.floor(t / 1000);
  }

  function durationSec(j) {
    const s = unixOf(j.started_at);
    const f = unixOf(j.finished_at);
    if (s == null) return null;
    if (f != null) return Math.max(0, f - s);
    return Math.max(0, Math.floor(Date.now() / 1000) - s);
  }

  function uuid8(s) { return s ? String(s).slice(0, 8) : "-"; }

  function statusLabel(s) {
    switch (s) {
      case "pending":   return "等待中";
      case "running":   return "运行中";
      case "succeeded": return "成功";
      case "failed":    return "失败";
      case "cancelled": return "已取消";
      default:          return s || "-";
    }
  }

  function typeLabel(t) {
    switch (t) {
      case "install":   return "装机";
      case "reinstall": return "重装";
      default:          return t || "-";
    }
  }

  function render() {
    const filtered = applyFilters(latestJobs);
    document.getElementById("jobs-loading").hidden = true;
    document.getElementById("jobs-count").textContent =
      "共 " + filtered.length + " 个作业" +
      (filtered.length !== latestJobs.length ? "（总数 " + latestJobs.length + "）" : "");
    const empty = document.getElementById("jobs-empty");
    const wrap = document.getElementById("jobs-wrap");
    if (filtered.length === 0) {
      empty.hidden = false;
      wrap.hidden = true;
      return;
    }
    empty.hidden = true;
    wrap.hidden = false;

    const tbody = document.getElementById("jobs-tbody");
    tbody.innerHTML = "";
    for (const j of filtered) {
      const tr = document.createElement("tr");
      tr.appendChild(jobCell(j));
      tr.appendChild(machineCell(j));
      tr.appendChild(typeCell(j));
      tr.appendChild(statusCell(j));
      tr.appendChild(stageCell(j));
      tr.appendChild(startedCell(j));
      tr.appendChild(durationCell(j));
      tr.appendChild(actionsCell(j));
      tbody.appendChild(tr);
    }
  }

  function jobCell(j) {
    const td = document.createElement("td");
    const short = uuid8(j.id);
    td.innerHTML =
      '<a class="mono" href="/ui/jobs/' + encodeURIComponent(j.id) + '" title="' +
      MK.escapeHTML(j.id) + '">' + MK.escapeHTML(short) + '</a> ' +
      '<button type="button" class="btn btn-ghost copy-btn" data-copy="' +
      MK.escapeHTML(j.id) + '" title="复制完整作业 ID">复制</button>';
    td.querySelector(".copy-btn").addEventListener("click", function (e) {
      MK.copyText(j.id, e.currentTarget);
    });
    return td;
  }

  function machineCell(j) {
    const td = document.createElement("td");
    const mu = String(j.machine_uuid || "");
    const m = machineByUUID[mu.toLowerCase()];
    const product = m && m.product ? m.product : "";
    td.innerHTML =
      '<a class="mono" href="/ui/m/' + encodeURIComponent(mu) + '" title="' +
      MK.escapeHTML(mu) + '">' + MK.escapeHTML(uuid8(mu)) + '</a>' +
      (product ? '<div class="muted">' + MK.escapeHTML(product) + '</div>' : '');
    return td;
  }

  function typeCell(j) {
    const td = document.createElement("td");
    const t = j.type || "";
    td.innerHTML = '<span class="badge" data-type="' + MK.escapeHTML(t) + '">' + MK.escapeHTML(typeLabel(t)) + '</span>';
    return td;
  }

  function statusCell(j) {
    const td = document.createElement("td");
    const s = j.status || "";
    let html = '<span class="badge" data-status="' + MK.escapeHTML(s) + '">' + MK.escapeHTML(statusLabel(s)) + '</span>';
    if (s === "running") html += ' <span class="spinner" aria-hidden="true"></span>';
    td.innerHTML = html;
    return td;
  }

  function stageCell(j) {
    const td = document.createElement("td");
    td.className = "mono muted";
    td.textContent = j.stage || "-";
    return td;
  }

  function startedCell(j) {
    const td = document.createElement("td");
    const u = unixOf(j.started_at) || unixOf(j.created_at);
    if (u == null) {
      td.textContent = "-";
      return td;
    }
    td.innerHTML = MK.escapeHTML(MK.fmtRelative(u)) +
      '<div class="muted mono">' + MK.escapeHTML(MK.fmtAbsolute(u)) + '</div>';
    return td;
  }

  function durationCell(j) {
    const td = document.createElement("td");
    const d = durationSec(j);
    if (d == null) {
      td.textContent = "-";
      return td;
    }
    if (!j.finished_at && (j.status === "pending" || j.status === "running")) {
      td.innerHTML = '<span class="spinner" aria-hidden="true"></span> ' +
        MK.escapeHTML(MK.fmtDuration(d));
    } else {
      td.textContent = MK.fmtDuration(d);
    }
    return td;
  }

  function actionsCell(j) {
    const td = document.createElement("td");
    td.className = "col-actions";
    let html =
      '<a class="btn btn-ghost" href="/ui/jobs/' + encodeURIComponent(j.id) + '">查看</a>';
    if (j.status === "pending" || j.status === "running") {
      html +=
        ' <button type="button" class="btn btn-danger cancel-btn" data-id="' +
        MK.escapeHTML(j.id) + '">取消</button>';
    } else {
      // succeeded | failed | cancelled — log is no longer mutating, allow purge.
      html +=
        ' <button type="button" class="btn btn-danger delete-btn" data-id="' +
        MK.escapeHTML(j.id) + '">删除</button>';
    }
    td.innerHTML = html;
    const cancelBtn = td.querySelector(".cancel-btn");
    if (cancelBtn) {
      cancelBtn.addEventListener("click", function () { cancelJob(j); });
    }
    const deleteBtn = td.querySelector(".delete-btn");
    if (deleteBtn) {
      deleteBtn.addEventListener("click", function () { deleteJob(j); });
    }
    return td;
  }

  async function cancelJob(j) {
    if (!confirm("取消机器 " + uuid8(j.machine_uuid) + " 上的作业 " + uuid8(j.id) + "？")) {
      return;
    }
    try {
      await MK.apiSend("POST", "/jobs/" + encodeURIComponent(j.id) + "/cancel", null);
      MK.flashSuccess("已请求取消作业 " + uuid8(j.id));
      tick(); // force-refresh now
    } catch (err) {
      MK.flashError("取消失败：" + err.message);
    }
  }

  async function deleteJob(j) {
    if (!confirm("删除作业 " + uuid8(j.id) + " 及其所有日志？此操作不可撤销。")) {
      return;
    }
    try {
      await MK.apiSend("DELETE", "/jobs/" + encodeURIComponent(j.id), null);
      MK.flashSuccess("已删除作业 " + uuid8(j.id));
      tick();
    } catch (err) {
      MK.flashError("删除失败：" + err.message);
    }
  }

  async function purgeAll(btn) {
    // Count terminal jobs in latestJobs to show in the confirm message.
    const terminal = latestJobs.filter(function (j) {
      return j.status === "succeeded" || j.status === "failed" || j.status === "cancelled";
    });
    if (terminal.length === 0) {
      MK.flashError("当前没有可清除的已完成作业");
      return;
    }
    if (!confirm("将删除全部 " + terminal.length + " 个已完成 / 失败 / 已取消的作业及其日志。\n\n此操作不可撤销。确定继续？")) {
      return;
    }
    await MK.withBusy(btn, "清除中…", async function () {
      try {
        const resp = await MK.apiSend("POST", "/jobs/purge", null);
        const n = (resp && typeof resp.deleted === "number") ? resp.deleted : terminal.length;
        MK.flashSuccess("已清除 " + n + " 个作业");
        tick();
      } catch (err) {
        MK.flashError("清除失败：" + err.message);
      }
    });
  }
})();
