// metalkit operator UI — images page.
//
// Flow:
//   1. user picks a file + fills metadata
//   2. POST /api/v1/images/uploads → returns {id, num_chunks, chunk_size, ...}
//   3. PUT /api/v1/images/uploads/{id}/chunks/{n} for each chunk
//   4. POST /api/v1/images/uploads/{id}/finalize → returns the Image row
//   5. refresh the catalog table
//
// The server streams SHA-256 while assembling chunks in finalize, and uses
// that hash for file naming, dedup, and the images.sha256 column. The client
// does NOT compute any hash — that previously failed on >2GB files because
// Chrome's blob.arrayBuffer() caps near 2GB. expected_sha256 is sent empty;
// the server treats empty as "skip pre-finalize verification."
//
// If the user aborts mid-upload, we DELETE the session so server-side temp
// files get cleaned up.

(function () {
  "use strict";

  const API_BASE = (function () {
    const m = document.querySelector('meta[name="metalkit-api-base"]');
    return (m && m.getAttribute("content")) || "/api/v1";
  })();

  const CHUNK_SIZE = 8 * 1024 * 1024; // 8 MiB — server allows up to 64 MiB

  let currentUpload = null; // {sessionID, abortController, cancelled}

  document.addEventListener("DOMContentLoaded", function () {
    document.getElementById("refresh-btn").addEventListener("click", loadImages);
    document.getElementById("upload-file").addEventListener("change", onFilePicked);
    document.getElementById("upload-start").addEventListener("click", startUpload);
    document.getElementById("upload-abort").addEventListener("click", abortUpload);
    loadImages();
  });

  // ---- helpers --------------------------------------------------------

  function $(id) { return document.getElementById(id); }

  function escapeHTML(s) {
    if (s == null) return "";
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function showError(msg) {
    const b = $("error-banner");
    b.textContent = msg;
    b.hidden = false;
  }

  function clearError() {
    const b = $("error-banner");
    b.textContent = "";
    b.hidden = true;
  }

  function fmtBytes(n) {
    if (n == null) return "-";
    if (n === 0) return "0 B";
    const v = Number(n);
    if (!isFinite(v)) return "-";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0, cur = v;
    while (cur >= 1024 && i < units.length - 1) { cur /= 1024; i++; }
    return (cur >= 100 ? cur.toFixed(0) : cur.toFixed(1)) + " " + units[i];
  }

  function fmtRelative(iso) {
    if (!iso) return "-";
    const t = Date.parse(iso);
    if (!isFinite(t)) return "-";
    const diff = Math.max(0, Math.floor((Date.now() - t) / 1000));
    if (diff < 5) return "刚刚";
    if (diff < 60) return diff + " 秒前";
    if (diff < 3600) return Math.floor(diff / 60) + " 分钟前";
    if (diff < 86400) return Math.floor(diff / 3600) + " 小时前";
    return Math.floor(diff / 86400) + " 天前";
  }

  // ---- catalog list ---------------------------------------------------

  async function loadImages() {
    clearError();
    $("images-loading").hidden = false;
    $("images-empty").hidden = true;
    $("images-wrap").hidden = true;
    try {
      const resp = await fetch(API_BASE + "/images", { credentials: "same-origin" });
      if (!resp.ok) throw new Error("GET /images: " + resp.status);
      const list = await resp.json();
      renderImages(list || []);
    } catch (err) {
      showError("加载镜像列表失败：" + err.message);
      $("images-loading").hidden = true;
    }
  }

  function renderImages(list) {
    $("images-loading").hidden = true;
    $("images-count").textContent = "共 " + list.length + " 个镜像";
    if (list.length === 0) {
      $("images-empty").hidden = false;
      return;
    }
    $("images-wrap").hidden = false;
    const tbody = $("images-tbody");
    tbody.innerHTML = "";
    list.sort((a, b) => (b.uploaded_at || "").localeCompare(a.uploaded_at || ""));
    for (const img of list) {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td>${escapeHTML(img.name)}${img.notes ? `<div class="muted">${escapeHTML(img.notes)}</div>` : ""}</td>
        <td>${escapeHTML(img.family || "-")}${img.version ? " / " + escapeHTML(img.version) : ""}</td>
        <td>${escapeHTML(img.format)}</td>
        <td>${fmtBytes(img.size_bytes)}${img.virtual_size ? " / " + fmtBytes(img.virtual_size) : ""}</td>
        <td title="${escapeHTML(img.uploaded_at)} 上传者 ${escapeHTML(img.uploaded_by)}">${fmtRelative(img.uploaded_at)}<div class="muted">${escapeHTML(img.uploaded_by)}</div></td>
        <td class="mono" title="${escapeHTML(img.sha256)}">${escapeHTML(img.sha256.slice(0, 12))}&hellip;</td>
        <td class="col-actions"><button type="button" class="btn delete-btn" data-id="${escapeHTML(img.id)}" data-name="${escapeHTML(img.name)}">删除</button></td>
      `;
      tbody.appendChild(tr);
    }
    tbody.querySelectorAll(".delete-btn").forEach(function (b) {
      b.addEventListener("click", function () {
        deleteImage(b.dataset.id, b.dataset.name);
      });
    });
  }

  async function deleteImage(id, name) {
    if (!confirm("删除镜像「" + name + "」？这将删除目录条目和磁盘上的文件。")) {
      return;
    }
    try {
      const resp = await fetch(API_BASE + "/images/" + encodeURIComponent(id), {
        method: "DELETE",
        credentials: "same-origin",
      });
      if (!resp.ok) {
        const body = await resp.text();
        throw new Error("DELETE: " + resp.status + " " + body);
      }
      loadImages();
    } catch (err) {
      showError("删除失败：" + err.message);
    }
  }

  // ---- upload ---------------------------------------------------------

  function onFilePicked() {
    const f = $("upload-file").files[0];
    const startBtn = $("upload-start");
    const hint = $("upload-detected-hint");
    if (!f) {
      startBtn.disabled = true;
      $("upload-status").textContent = "";
      if (hint) hint.hidden = true;
      return;
    }
    $("upload-status").textContent = f.name + " (" + fmtBytes(f.size) + ")";
    if (!$("upload-name").value) {
      $("upload-name").value = f.name;
    }
    // Filename-based family/version suggestion (mirrors backend detect.go).
    // Auto-fills empty fields; existing operator values are not overwritten.
    const det = detectFromFilename(f.name);
    if (det.family) {
      const sel = $("upload-family");
      if (sel && !sel.value) sel.value = det.family;
    }
    if (det.version) {
      const ver = $("upload-version");
      if (ver && !ver.value) ver.value = det.version;
    }
    if (hint) {
      if (det.family || det.version) {
        hint.textContent = "（识别：" + (det.family || "?") + (det.version ? " / " + det.version : "") + "）";
        hint.hidden = false;
      } else {
        hint.hidden = true;
      }
    }
    startBtn.disabled = false;
  }

  // detectFromFilename — client-side mirror of internal/images/detect.go.
  // Best-effort: matches well-known cloud-image naming conventions and returns
  // {family, version}. Empty values mean "no match". The backend re-runs the
  // same logic at upload init time, so this is purely a UX preview.
  function detectFromFilename(name) {
    if (!name) return { family: "", version: "" };
    const n = name.toLowerCase();
    const ubuntuCodename = { noble: "24.04", jammy: "22.04", focal: "20.04",
      bionic: "18.04", xenial: "16.04", trusty: "14.04" };
    const debianCodename = { trixie: "13", bookworm: "12", bullseye: "11",
      buster: "10", stretch: "9" };
    let m;
    if ((m = n.match(/(?:^|[-_./])(noble|jammy|focal|bionic|xenial|trusty)(?:[-_./]|$)/))) {
      return { family: "ubuntu", version: ubuntuCodename[m[1]] || "" };
    }
    if ((m = n.match(/ubuntu[-_.]?(\d+\.\d+)/))) {
      return { family: "ubuntu", version: m[1] };
    }
    if ((m = n.match(/(?:^|[-_./])(bookworm|bullseye|buster|stretch|trixie)(?:[-_./]|$)/))) {
      return { family: "debian", version: debianCodename[m[1]] || "" };
    }
    if ((m = n.match(/debian[-_.]?(\d+)/))) {
      return { family: "debian", version: m[1] };
    }
    if ((m = n.match(/centos[-_.]?(7(?:\.\d+)?)\b/))) {
      return { family: "rhel7", version: m[1] };
    }
    if ((m = n.match(/rocky[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "rhel", version: m[1] };
    }
    if ((m = n.match(/almalinux[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "rhel", version: m[1] };
    }
    if ((m = n.match(/centos[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "rhel", version: m[1] };
    }
    if ((m = n.match(/rhel[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "rhel", version: m[1] };
    }
    if ((m = n.match(/(?:ol|oracle)[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "rhel", version: m[1] };
    }
    if (/fedora/.test(n) && (m = n.match(/fedora[^0-9]+(\d{2,3})\b/))) {
      return { family: "rhel", version: m[1] };
    }
    if ((m = n.match(/kylin[-_.]?v?(\d+(?:\.\d+)?)/))) {
      return { family: "kylin", version: m[1] };
    }
    if ((m = n.match(/openeuler[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "openeuler", version: m[1] };
    }
    if (/opensuse[-_.]?tumbleweed/.test(n)) {
      return { family: "opensuse", version: "" };
    }
    if ((m = n.match(/opensuse[-_.]?leap[-_.]?(\d+(?:\.\d+)?)/))) {
      return { family: "opensuse", version: m[1] };
    }
    return { family: "", version: "" };
  }

  function setProgress(done, total, label) {
    const pct = total > 0 ? (done / total) * 100 : 0;
    $("upload-progress-wrap").hidden = false;
    $("upload-progress-bar").style.width = pct.toFixed(1) + "%";
    $("upload-progress-text").textContent = label;
  }

  async function startUpload() {
    clearError();
    const f = $("upload-file").files[0];
    if (!f) return;

    const startBtn = $("upload-start");
    const abortBtn = $("upload-abort");
    startBtn.disabled = true;
    abortBtn.hidden = false;

    currentUpload = { sessionID: null, cancelled: false };

    try {
      // 1. init session. Server streams SHA-256 during chunk assembly, so we
      //    don't compute it client-side — that fails on >2GB files (Chrome's
      //    ArrayBuffer limit). expected_sha256="" tells the server to skip
      //    pre-finalize verification and use the hash it computes itself.
      setProgress(0, 1, "正在初始化上传会话…");
      const initBody = {
        name: $("upload-name").value || f.name,
        version: $("upload-version").value || "",
        family: $("upload-family").value || "",
        notes: $("upload-notes").value || "",
        expected_sha256: "",
        total_size: f.size,
        chunk_size: CHUNK_SIZE,
      };
      const initResp = await fetch(API_BASE + "/images/uploads", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(initBody),
      });
      if (!initResp.ok) {
        const txt = await initResp.text();
        throw new Error("初始化失败 " + initResp.status + "：" + txt);
      }
      const sess = await initResp.json();
      currentUpload.sessionID = sess.id;

      // 2. PUT chunks. No per-chunk SHA-256 — server verifies final assembled
      //    file hash in finalize, and total-size check catches missing/extra
      //    bytes. Skipping per-chunk hash avoids reading each 8MB slice into
      //    an ArrayBuffer, which matters for the overall upload throughput.
      const total = sess.num_chunks;
      for (let n = 1; n <= total; n++) {
        if (currentUpload.cancelled) return;
        const start = (n - 1) * sess.chunk_size;
        const end = Math.min(start + sess.chunk_size, f.size);
        const blob = f.slice(start, end);
        setProgress(n - 1, total, "上传分块 " + n + " / " + total);
        const putResp = await fetch(
          API_BASE + "/images/uploads/" + encodeURIComponent(sess.id) + "/chunks/" + n,
          {
            method: "PUT",
            credentials: "same-origin",
            headers: {
              "Content-Type": "application/octet-stream",
            },
            body: blob,
          }
        );
        if (!putResp.ok) {
          const txt = await putResp.text();
          throw new Error("分块 " + n + "：" + putResp.status + " " + txt);
        }
        setProgress(n, total, "已上传 " + n + " / " + total);
      }

      if (currentUpload.cancelled) return;

      // 4. finalize.
      setProgress(total, total, "正在收尾…");
      const finalResp = await fetch(
        API_BASE + "/images/uploads/" + encodeURIComponent(sess.id) + "/finalize",
        { method: "POST", credentials: "same-origin" }
      );
      if (!finalResp.ok) {
        const txt = await finalResp.text();
        throw new Error("收尾失败 " + finalResp.status + "：" + txt);
      }
      const img = await finalResp.json();
      $("upload-status").textContent = "已上传：" + img.name + "（" + img.sha256.slice(0, 12) + "…）";
      currentUpload = null;
      resetUploadForm();
      loadImages();
    } catch (err) {
      showError("上传失败：" + err.message);
      // Best-effort cleanup of the session.
      if (currentUpload && currentUpload.sessionID) {
        fetch(API_BASE + "/images/uploads/" + encodeURIComponent(currentUpload.sessionID), {
          method: "DELETE",
          credentials: "same-origin",
        }).catch(function () {});
      }
      currentUpload = null;
      resetUploadForm();
    }
  }

  async function abortUpload() {
    if (!currentUpload) return;
    currentUpload.cancelled = true;
    const sid = currentUpload.sessionID;
    if (sid) {
      try {
        await fetch(API_BASE + "/images/uploads/" + encodeURIComponent(sid), {
          method: "DELETE",
          credentials: "same-origin",
        });
      } catch (err) { /* ignore */ }
    }
    currentUpload = null;
    $("upload-status").textContent = "已中止。";
    resetUploadForm();
  }

  function resetUploadForm() {
    $("upload-start").disabled = !$("upload-file").files[0];
    $("upload-abort").hidden = true;
    setTimeout(function () {
      $("upload-progress-wrap").hidden = true;
    }, 2000);
  }
})();
