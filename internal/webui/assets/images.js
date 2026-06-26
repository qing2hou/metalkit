// metalkit operator UI — images page.
//
// Flow:
//   1. user picks a file + fills metadata
//   2. we compute SHA-256 of the whole file (SubtleCrypto, streaming over slices)
//   3. POST /api/v1/images/uploads → returns {id, num_chunks, chunk_size, ...}
//   4. PUT /api/v1/images/uploads/{id}/chunks/{n} for each chunk, with
//      X-Chunk-Sha256 header (server verifies)
//   5. POST /api/v1/images/uploads/{id}/finalize → returns the Image row
//   6. refresh the catalog table
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

  function bufToHex(buf) {
    const b = new Uint8Array(buf);
    const hex = new Array(b.length);
    for (let i = 0; i < b.length; i++) {
      hex[i] = b[i].toString(16).padStart(2, "0");
    }
    return hex.join("");
  }

  async function sha256Hex(blob) {
    const buf = await blob.arrayBuffer();
    const digest = await sha256Digest(buf);
    return bufToHex(digest);
  }

  // sha256Digest returns an ArrayBuffer of the SHA-256 hash.
  // Uses SubtleCrypto when available (secure context), falls back to pure JS
  // for HTTP non-localhost (e.g. 192.168.x.x internal deployments).
  async function sha256Digest(data) {
    if (crypto.subtle) {
      return crypto.subtle.digest("SHA-256", data);
    }
    return sha256Pure(data);
  }

  // Pure JS SHA-256 — only used as fallback when crypto.subtle is unavailable.
  function sha256Pure(data) {
    const msg = new Uint8Array(data);

    // Constants
    const K = [
      0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
      0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
      0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
      0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
      0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
      0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
      0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
      0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2,
    ];

    // Initial hash values
    let H = [0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19];

    // Padding (RFC 6234 §4.1): 0x80 | zeroes | 64-bit big-endian bit length.
    // Total padded size must be a multiple of 64 bytes.
    const bitLen = msg.length * 8;
    const zeroBytes = (56 - (msg.length + 1) % 64 + 64) % 64;
    const padLen = 1 + zeroBytes;
    const padded = new Uint8Array(msg.length + padLen + 8);
    padded.set(msg);
    padded[msg.length] = 0x80;
    const dv = new DataView(padded.buffer);
    dv.setUint32(padded.length - 4, bitLen >>> 0, false);
    dv.setUint32(padded.length - 8, Math.floor(bitLen / 0x100000000), false);

    // Process 512-bit blocks
    for (let i = 0; i < padded.length; i += 64) {
      const W = new Uint32Array(64);
      for (let t = 0; t < 16; t++) {
        W[t] = (padded[i + t*4] << 24) | (padded[i + t*4 + 1] << 16) | (padded[i + t*4 + 2] << 8) | padded[i + t*4 + 3];
      }
      for (let t = 16; t < 64; t++) {
        const s0 = rotr(W[t-15], 7) ^ rotr(W[t-15], 18) ^ (W[t-15] >>> 3);
        const s1 = rotr(W[t-2], 17) ^ rotr(W[t-2], 19) ^ (W[t-2] >>> 10);
        W[t] = (W[t-16] + s0 + W[t-7] + s1) >>> 0;
      }

      let [a,b,c,d,e,f,g,h] = H;
      for (let t = 0; t < 64; t++) {
        const S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
        const ch = (e & f) ^ (~e & g);
        const t1 = (h + S1 + ch + K[t] + W[t]) >>> 0;
        const S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
        const maj = (a & b) ^ (a & c) ^ (b & c);
        const t2 = (S0 + maj) >>> 0;
        h = g; g = f; f = e; e = (d + t1) >>> 0;
        d = c; c = b; b = a; a = (t1 + t2) >>> 0;
      }
      H = [H[0]+a, H[1]+b, H[2]+c, H[3]+d, H[4]+e, H[5]+f, H[6]+g, H[7]+h].map(function(v) { return v >>> 0; });
    }

    const out = new ArrayBuffer(32);
    const outDV = new DataView(out);
    for (let i = 0; i < 8; i++) { outDV.setUint32(i*4, H[i], false); }
    return out;
  }

  function rotr(x, n) { return (x >>> n) | (x << (32 - n)); }

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
      // 1. compute full-file SHA-256.
      setProgress(0, 1, "正在计算文件哈希…");
      const sha = await sha256Hex(f);
      if (currentUpload.cancelled) return;

      // 2. init session.
      const initBody = {
        name: $("upload-name").value || f.name,
        version: $("upload-version").value || "",
        family: $("upload-family").value || "",
        notes: $("upload-notes").value || "",
        expected_sha256: sha,
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

      // 3. PUT chunks.
      const total = sess.num_chunks;
      for (let n = 1; n <= total; n++) {
        if (currentUpload.cancelled) return;
        const start = (n - 1) * sess.chunk_size;
        const end = Math.min(start + sess.chunk_size, f.size);
        const blob = f.slice(start, end);
        const chunkSha = await sha256Hex(blob);
        setProgress(n - 1, total, "上传分块 " + n + " / " + total);
        const putResp = await fetch(
          API_BASE + "/images/uploads/" + encodeURIComponent(sess.id) + "/chunks/" + n,
          {
            method: "PUT",
            credentials: "same-origin",
            headers: {
              "Content-Type": "application/octet-stream",
              "X-Chunk-Sha256": chunkSha,
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
