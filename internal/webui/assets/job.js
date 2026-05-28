// metalkit operator UI — job detail page.
//
// Adaptive polling: while job.status ∈ {pending, running} we re-fetch the
// job summary AND new log lines every 1s; once the job lands on a terminal
// state (succeeded/failed/cancelled) we do exactly one more poll after
// POLL_TERMINAL_MS to catch the tail, then stop. since_id-incremental log
// fetching avoids re-rendering history rows.
//
// API:
//   GET    /api/v1/jobs/{id}                                  → Job
//   GET    /api/v1/jobs/{id}/logs?since_id=N&limit=M          → []JobLog
//   POST   /api/v1/jobs/{id}/cancel                           → 204
//   PUT    /api/v1/bindings/{machine_uuid}                    → re-arm for retry

(function () {
  "use strict";

  const POLL_RUNNING_MS = MK.POLL.JOB_RUNNING_MS;
  const POLL_TERMINAL_MS = MK.POLL.JOB_TERMINAL_MS;
  const LOG_PAGE_LIMIT = 500;

  // Authoritative stage sequence for the timeline. Mirrors installer pipeline.
  const STAGES = [
    "download", "write", "grow", "mount", "seed", "grub-install", "umount", "succeed",
  ];

  const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

  let jobID = null;
  let lastLogID = 0;
  let pollTimer = null;
  let stoppedTailTick = false;
  let job = null; // last fetched
  let followTail = true;

  document.addEventListener("DOMContentLoaded", function () {
    jobID = parseJobIDFromPath();
    if (!jobID) {
      MK.flashError("无法从 URL 解析作业 ID");
      return;
    }
    document.getElementById("job-id-short").textContent = jobID.slice(0, 8);
    document.title = "metalkit — 作业 " + jobID.slice(0, 8);

    document.getElementById("refresh-btn").addEventListener("click", function () { tick(true); });
    document.getElementById("follow-tail").addEventListener("change", function (e) {
      followTail = !!e.target.checked;
      if (followTail) scrollToBottom();
    });
    const dl = document.getElementById("job-download-log");
    if (dl) dl.addEventListener("click", function () { downloadLogs(dl); });

    tick(true);
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") tick(true);
    });
  });

  function parseJobIDFromPath() {
    // /ui/jobs/{id}
    const m = window.location.pathname.match(/^\/ui\/jobs\/([^/]+)\/?$/);
    return m ? decodeURIComponent(m[1]) : null;
  }

  // ---- labels ---------------------------------------------------------

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

  // ---- polling --------------------------------------------------------

  function schedule(ms) {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = setTimeout(function () { tick(false); }, ms);
  }

  async function tick(immediate) {
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
    try {
      const fresh = await MK.apiGet("/jobs/" + encodeURIComponent(jobID));
      job = fresh;
      MK.clearError();
      renderJob(job);
      await fetchLogs();
    } catch (err) {
      MK.flashError("加载作业失败：" + err.message);
    }
    const status = (job && job.status) || "pending";
    if (TERMINAL.has(status)) {
      if (!stoppedTailTick) {
        stoppedTailTick = true;
        schedule(POLL_TERMINAL_MS); // one last tail tick
      }
      // else: don't reschedule; polling stops.
    } else {
      stoppedTailTick = false;
      schedule(POLL_RUNNING_MS);
    }
  }

  async function fetchLogs() {
    try {
      const lines = await MK.apiGet(
        "/jobs/" + encodeURIComponent(jobID) +
        "/logs?since_id=" + lastLogID + "&limit=" + LOG_PAGE_LIMIT
      );
      if (Array.isArray(lines) && lines.length > 0) {
        appendLogs(lines);
        lastLogID = lines[lines.length - 1].id || lastLogID;
        if (followTail) scrollToBottom();
      } else {
        // First load with zero rows.
        const tbody = document.getElementById("logs-tbody");
        if (!tbody.children.length) {
          document.getElementById("logs-loading").hidden = true;
          document.getElementById("logs-empty").hidden = false;
        }
      }
    } catch (err) {
      // Don't tear down the UI on a transient log fetch error; just flash.
      MK.flashError("加载日志失败：" + err.message);
    }
  }

  // ---- rendering ------------------------------------------------------

  function renderJob(j) {
    const badge = document.getElementById("job-status-badge");
    const status = j.status || "";
    badge.innerHTML = '<span class="badge" data-status="' + MK.escapeHTML(status) + '">' +
      MK.escapeHTML(statusLabel(status)) + '</span>' +
      (status === "running" ? ' <span class="spinner" aria-hidden="true"></span>' : '');

    const kv = document.getElementById("job-kv");
    kv.innerHTML = "";
    addKV(kv, "机器",
      '<a class="mono" href="/ui/m/' + encodeURIComponent(j.machine_uuid || "") + '" title="' +
      MK.escapeHTML(j.machine_uuid || "") + '">' + MK.escapeHTML((j.machine_uuid || "").slice(0, 8)) + '</a>');
    addKV(kv, "类型", '<span class="badge" data-type="' + MK.escapeHTML(j.type || "") + '">' + MK.escapeHTML(typeLabel(j.type)) + '</span>');
    addKV(kv, "阶段", '<span class="mono">' + MK.escapeHTML(j.stage || "-") + '</span>');
    addKV(kv, "镜像", j.image_id ? '<span class="mono">' + MK.escapeHTML((j.image_id || "").slice(0, 8)) + '</span>' : "-");
    addKV(kv, "配置", j.profile_id ? '<span class="mono">' + MK.escapeHTML((j.profile_id || "").slice(0, 8)) + '</span>' : "-");
    addKV(kv, "创建时间", fmtTimePair(j.created_at));
    addKV(kv, "开始时间", fmtTimePair(j.started_at));
    addKV(kv, "完成时间", fmtTimePair(j.finished_at));
    const dur = durationSec(j);
    addKV(kv, "耗时", dur == null ? "-" : MK.fmtDuration(dur));
    if (j.created_by) addKV(kv, "创建者", MK.escapeHTML(j.created_by));
    if (j.retry_of_job_id) {
      addKV(kv, "重试自",
        '<a class="mono" href="/ui/jobs/' + encodeURIComponent(j.retry_of_job_id) + '">' +
        MK.escapeHTML((j.retry_of_job_id || "").slice(0, 8)) + '</a>');
    }

    const errBox = document.getElementById("job-error");
    if (j.error) {
      errBox.textContent = j.error;
      errBox.hidden = false;
    } else {
      errBox.hidden = true;
    }

    renderTimeline(j);
    renderActions(j);
  }

  function addKV(parent, label, valueHTML) {
    const div = document.createElement("div");
    const dt = document.createElement("dt");
    dt.textContent = label;
    const dd = document.createElement("dd");
    dd.innerHTML = valueHTML;
    div.appendChild(dt);
    div.appendChild(dd);
    parent.appendChild(div);
  }

  function renderTimeline(j) {
    const tl = document.getElementById("timeline");
    tl.innerHTML = "";
    const status = j.status || "pending";
    const currentStage = j.stage || "";
    let curIdx = STAGES.indexOf(currentStage);
    // Tolerate stages we don't know about — collapse to the closest match.
    if (curIdx === -1 && currentStage) curIdx = 0;
    const failed = status === "failed";
    const cancelled = status === "cancelled";
    const succeeded = status === "succeeded";

    for (let i = 0; i < STAGES.length; i++) {
      const name = STAGES[i];
      const step = document.createElement("div");
      step.className = "timeline-step";
      let stateLabel = "";
      if (succeeded || i < curIdx) {
        step.classList.add("done");
        stateLabel = "✓ 完成";
      } else if (i === curIdx && (failed || cancelled)) {
        step.classList.add("failed");
        stateLabel = failed ? "✗ 失败" : "⊘ 已取消";
      } else if (i === curIdx) {
        step.classList.add("active");
        stateLabel = status === "running" ? "运行中…" : "排队中";
      } else {
        stateLabel = "等待中";
      }
      step.innerHTML =
        '<div class="step-name">' + MK.escapeHTML(name) + '</div>' +
        '<div class="step-state">' + MK.escapeHTML(stateLabel) + '</div>';
      tl.appendChild(step);
    }
  }

  function renderActions(j) {
    const actions = document.getElementById("job-actions");
    const cancelBtn = document.getElementById("cancel-btn");
    const retryBtn = document.getElementById("retry-btn");
    const deleteBtn = document.getElementById("delete-btn");
    const canCancel = j.status === "pending" || j.status === "running";
    const canRetry = j.status === "failed" || j.status === "cancelled";
    const canDelete = TERMINAL.has(j.status);
    cancelBtn.hidden = !canCancel;
    retryBtn.hidden = !canRetry;
    if (deleteBtn) deleteBtn.hidden = !canDelete;
    actions.hidden = !(canCancel || canRetry || canDelete);

    // Re-attach to avoid double-fires across renders.
    cancelBtn.onclick = function () { cancelJob(j); };
    retryBtn.onclick = function () { retryJob(j); };
    if (deleteBtn) deleteBtn.onclick = function () { deleteJob(j); };
  }

  function appendLogs(lines) {
    document.getElementById("logs-loading").hidden = true;
    document.getElementById("logs-empty").hidden = true;
    document.getElementById("logs-wrap").hidden = false;
    const tbody = document.getElementById("logs-tbody");
    for (const ln of lines) {
      const tr = document.createElement("tr");
      tr.className = "log-row";
      tr.dataset.level = String(ln.level || "info").toLowerCase();
      const tdTs = document.createElement("td");
      tdTs.className = "log-ts";
      tdTs.textContent = MK.fmtISO(ln.ts);
      const tdLvl = document.createElement("td");
      tdLvl.className = "log-lvl";
      tdLvl.textContent = ln.level || "info";
      const tdMsg = document.createElement("td");
      tdMsg.textContent = ln.message || "";
      tr.appendChild(tdTs);
      tr.appendChild(tdLvl);
      tr.appendChild(tdMsg);
      tbody.appendChild(tr);
    }
  }

  function scrollToBottom() {
    const tbody = document.getElementById("logs-tbody");
    if (!tbody) return;
    const last = tbody.lastElementChild;
    if (last && last.scrollIntoView) last.scrollIntoView({ block: "end" });
  }

  // ---- helpers --------------------------------------------------------

  function fmtTimePair(iso) {
    if (!iso) return "-";
    const t = Date.parse(iso);
    if (!isFinite(t)) return MK.escapeHTML(String(iso));
    const u = Math.floor(t / 1000);
    return MK.escapeHTML(MK.fmtRelative(u)) +
      '<div class="muted mono">' + MK.escapeHTML(MK.fmtAbsolute(u)) + '</div>';
  }

  function durationSec(j) {
    const s = j.started_at ? Math.floor(Date.parse(j.started_at) / 1000) : null;
    const f = j.finished_at ? Math.floor(Date.parse(j.finished_at) / 1000) : null;
    if (s == null || !isFinite(s)) return null;
    if (f != null && isFinite(f)) return Math.max(0, f - s);
    return Math.max(0, Math.floor(Date.now() / 1000) - s);
  }

  // ---- actions --------------------------------------------------------

  async function downloadLogs(btn) {
    await MK.withBusy(btn, "导出中…", async function () {
      try {
        const lines = await MK.apiGet("/jobs/" + encodeURIComponent(jobID) + "/logs");
        const arr = Array.isArray(lines) ? lines.slice() : [];
        arr.sort(function (a, b) { return (a.id || 0) - (b.id || 0); });
        const text = arr.map(function (ln) {
          return (ln.ts || "") + "  [" + (ln.level || "info") + "]  " + (ln.message || "");
        }).join("\n") + (arr.length ? "\n" : "");
        const blob = new Blob([text], { type: "text/plain" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = "job-" + jobID + ".log";
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
      } catch (err) {
        MK.flashError("导出日志失败：" + err.message);
      }
    });
  }

  async function cancelJob(j) {
    if (!confirm("取消作业 " + (j.id || "").slice(0, 8) + "？\n\nagent 会在下一个安全断点停止。")) return;
    try {
      await MK.apiSend("POST", "/jobs/" + encodeURIComponent(j.id) + "/cancel", null);
      MK.flashSuccess("已请求取消");
      tick(true);
    } catch (err) {
      MK.flashError("取消失败：" + err.message);
    }
  }

  async function deleteJob(j) {
    if (!confirm("删除作业 " + (j.id || "").slice(0, 8) + " 及其所有日志？此操作不可撤销。")) return;
    try {
      await MK.apiSend("DELETE", "/jobs/" + encodeURIComponent(j.id), null);
      MK.flashSuccess("已删除作业 " + (j.id || "").slice(0, 8));
      // Job no longer exists — go back to the list.
      setTimeout(function () { window.location.href = "/ui/jobs"; }, 600);
    } catch (err) {
      MK.flashError("删除失败：" + err.message);
    }
  }

  async function retryJob(j) {
    // Re-arming the binding (desired_state=install|reinstall) is how the
    // orchestrator picks the machine back up. We don't create a new job from
    // here directly; the orchestrator's next tick will.
    const action = (j.type === "reinstall") ? "reinstall" : "install";
    const actionLabel = (action === "reinstall") ? "重装" : "装机";
    if (!confirm("将机器 " + (j.machine_uuid || "").slice(0, 8) +
                 " 的 binding 重置为 desired_state=" + action + "（" + actionLabel + "）？\n\n这会通过 BMC 重启该机器。")) return;
    try {
      // Fetch current binding to preserve image/profile fields.
      const binding = await MK.apiGet("/bindings/" + encodeURIComponent(j.machine_uuid));
      const body = {
        image_id: binding.image_id,
        profile_id: binding.profile_id,
        desired_state: action,
      };
      if (binding.static_address) body.static_address = binding.static_address;
      if (binding.hostname_override) body.hostname_override = binding.hostname_override;
      await MK.apiSend("PUT", "/bindings/" + encodeURIComponent(j.machine_uuid), body);
      MK.flashSuccess("binding 已重新触发，orchestrator 将很快创建新作业");
    } catch (err) {
      MK.flashError("重试失败：" + err.message);
    }
  }
})();
