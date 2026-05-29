// metalkit operator UI — 机器详情页 Provisioning 面板。
// M2.3-9 的 Slice D。独立 IIFE；在 common.js 与 app.js 之后加载。
// 在 /ui/m/{uuid} 现有硬件报告网格上方渲染 4 张卡片
// （当前 binding、BMC、装机操作、最近作业）。Slice A/B/C/E 各管各的页，
// 这里不动它们。
//
// 所有 API 调用走 window.MK 助手；遇到不存在或暂时不可用的端点
// （例如 Slice B 的 /bmc/{uuid}/test 还没上）会用 flash 提示后继续走。

(function () {
  "use strict";

  if (document.body.dataset.page !== "detail") return;

  // ---- 模块状态 -------------------------------------------------------

  let muuid = "";
  let currentBinding = null; // 未配置时为 null
  let currentBMC = null;     // 未配置时为 null
  let recentJobs = [];       // 最新在前
  const catalog = {
    images: null,    // 数组
    profiles: null,  // 数组
    imageByID: null, // map
    profileByID: null, // map
  };

  // ---- 标签 -----------------------------------------------------------

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

  function desiredStateLabel(s) {
    switch (s) {
      case "none":      return "无";
      case "install":   return "装机";
      case "reinstall": return "重装";
      default:          return s || "-";
    }
  }

  // ---- 入口 -----------------------------------------------------------

  document.addEventListener("DOMContentLoaded", function () {
    const m = window.location.pathname.match(/\/ui\/m\/([^/]+)\/?$/);
    if (!m) return; // app.js 已经显示了错误条
    muuid = decodeURIComponent(m[1]).toLowerCase();

    wireStaticButtons();
    // 三个独立加载器并行 fan out，再渲染装机操作
    // （它依赖 binding + jobs）并展开整段。
    Promise.all([
      loadCatalog().catch(function (e) {
        // 即便目录拉不到也不要废掉整张面板；
        // 退化成直接显示 image_id / profile_id。
        console.warn("provisioning: 目录加载失败", e);
      }),
      loadBinding(),
      loadBMC(),
      loadRecentJobs(),
    ]).then(function () {
      renderBindingCard();
      renderBMCCard();
      renderRecentJobsCard();
      renderActionsCard();
      const panel = document.getElementById("provisioning-panel");
      if (panel) panel.hidden = false;
    }).catch(function (e) {
      MK.flashError("装机面板：" + e.message);
      const panel = document.getElementById("provisioning-panel");
      if (panel) panel.hidden = false;
    });
  });

  function wireStaticButtons() {
    document.getElementById("prov-binding-edit").addEventListener("click", openBindingModal);
    document.getElementById("prov-binding-clear").addEventListener("click", clearBinding);
    document.getElementById("prov-bmc-test").addEventListener("click", testBMC);
    document.getElementById("prov-bmc-edit").addEventListener("click", openBMCModal);
  }

  // ---- loaders --------------------------------------------------------

  async function loadCatalog() {
    const [imgs, profs, subs] = await Promise.all([
      MK.apiGet("/images").catch(function () { return []; }),
      MK.apiGet("/profiles").catch(function () { return []; }),
      MK.apiGet("/subnets").catch(function () { return []; }),
    ]);
    catalog.images = Array.isArray(imgs) ? imgs : [];
    catalog.profiles = Array.isArray(profs) ? profs : [];
    catalog.subnets = Array.isArray(subs) ? subs : [];
    catalog.imageByID = Object.create(null);
    catalog.profileByID = Object.create(null);
    catalog.subnetByID = Object.create(null);
    catalog.images.forEach(function (i) { if (i && i.id) catalog.imageByID[i.id] = i; });
    catalog.profiles.forEach(function (p) { if (p && p.id) catalog.profileByID[p.id] = p; });
    catalog.subnets.forEach(function (s) { if (s && s.id) catalog.subnetByID[s.id] = s; });
  }

  async function loadBinding() {
    try {
      currentBinding = await MK.apiGet("/bindings/" + encodeURIComponent(muuid));
    } catch (e) {
      if (e && e.status === 404) {
        currentBinding = null;
      } else {
        currentBinding = null;
        throw e;
      }
    }
  }

  async function loadBMC() {
    try {
      currentBMC = await MK.apiGet("/bmc/" + encodeURIComponent(muuid));
    } catch (e) {
      if (e && e.status === 404) {
        currentBMC = null;
      } else {
        currentBMC = null;
        throw e;
      }
    }
  }

  async function loadRecentJobs() {
    try {
      // 后端支持 ?machine_uuid= 过滤；如果服务端忽略了，再用客户端过滤兜底。
      const js = await MK.apiGet("/jobs?machine_uuid=" + encodeURIComponent(muuid) + "&limit=20");
      const arr = Array.isArray(js) ? js : [];
      recentJobs = arr.filter(function (j) {
        return !j.machine_uuid || j.machine_uuid.toLowerCase() === muuid;
      });
      // 最新在前：按 created_at 倒序。
      recentJobs.sort(function (a, b) {
        const aa = a && a.created_at ? Date.parse(a.created_at) : 0;
        const bb = b && b.created_at ? Date.parse(b.created_at) : 0;
        return bb - aa;
      });
    } catch (e) {
      recentJobs = [];
      console.warn("provisioning: 作业加载失败", e);
    }
  }

  // ---- 卡片 1：当前 binding -------------------------------------------

  function renderBindingCard() {
    const body = document.getElementById("prov-binding-body");
    const stateSpan = document.getElementById("prov-binding-state");
    const clearBtn = document.getElementById("prov-binding-clear");

    if (!currentBinding) {
      body.className = "muted";
      body.textContent = "尚未配置 binding。";
      stateSpan.innerHTML = "";
      clearBtn.disabled = true;
      return;
    }

    clearBtn.disabled = false;
    body.className = "";

    const img = catalog.imageByID && catalog.imageByID[currentBinding.image_id];
    const prof = catalog.profileByID && catalog.profileByID[currentBinding.profile_id];
    const imgLabel = img
      ? (img.name + (img.version ? " " + img.version : "") +
         (img.family ? " (" + img.family + ")" : ""))
      : currentBinding.image_id;
    const profLabel = prof ? prof.name : currentBinding.profile_id;

    stateSpan.innerHTML =
      ' <span class="badge" data-status="' + MK.escapeHTML(currentBinding.desired_state) + '">' +
      MK.escapeHTML(desiredStateLabel(currentBinding.desired_state)) + "</span>";

    const updatedAt = currentBinding.updated_at
      ? MK.fmtISO(currentBinding.updated_at)
      : "-";

    const sn = currentBinding.subnet_id
      ? (catalog.subnetByID && catalog.subnetByID[currentBinding.subnet_id])
      : null;
    const snLabel = sn
      ? (MK.escapeHTML(sn.name) + ' <span class="muted">(' + MK.escapeHTML(sn.cidr) +
         ", gw " + MK.escapeHTML(sn.gateway) +
         (sn.vlan_id ? ", vlan " + sn.vlan_id : "") + ")</span>")
      : (currentBinding.subnet_id
          ? '<span class="muted">' + MK.escapeHTML(currentBinding.subnet_id) + " (未在 catalog 找到)</span>"
          : '<span class="muted">未绑定（沿用 profile.network）</span>');

    const vlanLabel = currentBinding.vlan_override
      ? String(currentBinding.vlan_override) +
        ' <span class="muted">(覆盖 subnet 默认)</span>'
      : (sn && sn.vlan_id
          ? String(sn.vlan_id) + ' <span class="muted">(来自 subnet)</span>'
          : '<span class="muted">无</span>');

    const bondLabel = currentBinding.bond
      ? MK.escapeHTML(currentBinding.bond.mode) +
        ' <span class="muted">[' +
        MK.escapeHTML((currentBinding.bond.slaves || []).join(", ")) +
        "]</span>"
      : '<span class="muted">无（单 NIC）</span>';

    const rows = [
      ["镜像", MK.escapeHTML(imgLabel)],
      ["配置", MK.escapeHTML(profLabel)],
      ["子网", snLabel],
      ["静态地址", MK.escapeHTML(currentBinding.static_address || "—")],
      ["VLAN", vlanLabel],
      ["Bond", bondLabel],
      ["主机名", MK.escapeHTML(currentBinding.hostname || "—")],
      ["Root 密码", renderPasswordCell()],
      ["更新时间", MK.escapeHTML(updatedAt) +
        (currentBinding.updated_by
          ? ' <span class="muted">操作人：' + MK.escapeHTML(currentBinding.updated_by) + "</span>"
          : "")],
    ];
    body.innerHTML =
      '<dl class="kv-strip">' +
      rows.map(function (r) {
        return "<div><dt>" + MK.escapeHTML(r[0]) + "</dt><dd>" + r[1] + "</dd></div>";
      }).join("") +
      "</dl>";

    // 绑定查看密码按钮。
    const viewBtn = body.querySelector(".prov-view-password");
    if (viewBtn) {
      viewBtn.addEventListener("click", openPasswordModal);
    }
  }

  function renderPasswordCell() {
    if (!currentBinding || !currentBinding.has_password) {
      return '<span class="muted">使用 profile 默认</span>';
    }
    return '<span class="badge" data-status="installed">已设置</span> ' +
      '<button type="button" class="btn btn-ghost prov-view-password">查看密码</button>';
  }

  async function openPasswordModal() {
    if (!currentBinding) {
      return;
    }
    const uuid = currentBinding.machine_uuid;
    let plain;
    try {
      const res = await MK.apiGet("/bindings/" + encodeURIComponent(uuid) + "/password");
      plain = res && res.password;
    } catch (e) {
      MK.flashError("查询密码失败：" + e.message);
      return;
    }
    if (!plain) {
      MK.flashError("此 binding 未设置自定义密码");
      return;
    }
    const body =
      '<p class="muted">本密码已存入此机器的 binding，安装时会写入 /etc/shadow。</p>' +
      '<div class="kv-form">' +
      '<label class="kv"><span>Root 密码</span>' +
        '<input type="text" id="prov-pw-value" readonly value="' +
        MK.escapeHTML(plain) + '" autocomplete="off"></label>' +
      '</div>';
    const footer =
      '<button type="button" class="btn btn-ghost" data-modal-close>关闭</button> ' +
      '<button type="button" id="prov-pw-copy" class="btn btn-primary">复制到剪贴板</button>';
    const dlg = MK.openModal(MK.modalShell("查看 Root 密码", body, footer));
    const input = dlg.querySelector("#prov-pw-value");
    input.addEventListener("focus", function () { input.select(); });
    dlg.querySelector("#prov-pw-copy").addEventListener("click", function () {
      input.select();
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(plain);
        } else {
          document.execCommand("copy");
        }
        MK.flashSuccess("已复制到剪贴板");
      } catch (e) {
        MK.flashError("复制失败：" + e.message);
      }
    });
  }

  function openBindingModal() {
    const b = currentBinding || {
      image_id: "",
      profile_id: "",
      desired_state: "none",
      static_address: "",
      hostname: "",
      subnet_id: "",
      vlan_override: 0,
      bond: null,
    };

    const images = (catalog.images || []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    const profiles = (catalog.profiles || []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    const subnetsArr = (catalog.subnets || []).slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });

    const imgOpts = ['<option value="">— 选择镜像 —</option>'].concat(
      images.map(function (i) {
        const label = (i.name || i.id) +
          (i.version ? " " + i.version : "") +
          (i.family ? " (" + i.family + ")" : "");
        const sel = i.id === b.image_id ? " selected" : "";
        return '<option value="' + MK.escapeHTML(i.id) + '"' + sel + ">" +
          MK.escapeHTML(label) + "</option>";
      })
    ).join("");

    const profOpts = ['<option value="">— 选择配置 —</option>'].concat(
      profiles.map(function (p) {
        const sel = p.id === b.profile_id ? " selected" : "";
        const label = (p.name || p.id) +
          (p.os_family && p.os_family !== "any" ? " [" + p.os_family + "]" : "");
        return '<option value="' + MK.escapeHTML(p.id) + '"' + sel + ">" +
          MK.escapeHTML(label) + "</option>";
      })
    ).join("");

    const snOpts = ['<option value="">— 不绑定（沿用 profile.network） —</option>'].concat(
      subnetsArr.map(function (s) {
        const sel = s.id === b.subnet_id ? " selected" : "";
        const label = s.name + " (" + s.cidr + ", gw " + s.gateway +
          (s.vlan_id ? ", vlan " + s.vlan_id : "") + ")";
        return '<option value="' + MK.escapeHTML(s.id) + '"' + sel + ">" +
          MK.escapeHTML(label) + "</option>";
      })
    ).join("");

    const imgFamilyByID = {};
    images.forEach(function (i) { imgFamilyByID[i.id] = (i.family || "").toLowerCase(); });
    const profFamilyByID = {};
    profiles.forEach(function (p) { profFamilyByID[p.id] = (p.os_family || "any").toLowerCase(); });

    const states = ["none", "install", "reinstall"];
    const radios = states.map(function (s) {
      const checked = s === (b.desired_state || "none") ? " checked" : "";
      return '<label class="kv"><input type="radio" name="prov-desired" value="' +
        s + '"' + checked + "> " + desiredStateLabel(s) + "</label>";
    }).join(" ");

    // Bond initial values
    const bondCur = b.bond || {};
    const bondEnabled = !!b.bond;
    const bondModes = ["active-backup", "802.3ad"];
    const bondModeOpts = bondModes.map(function (m) {
      const sel = m === (bondCur.mode || "active-backup") ? " selected" : "";
      return '<option value="' + m + '"' + sel + ">" + m + "</option>";
    }).join("");
    const slavesCSV = (bondCur.slaves || []).join(", ");

    const body =
      '<div class="kv-form">' +
      '<label class="kv"><span>镜像</span>' +
        '<select id="prov-mod-image" required>' + imgOpts + "</select></label>" +
      '<label class="kv"><span>配置</span>' +
        '<select id="prov-mod-profile" required>' + profOpts + "</select></label>" +
      '<div id="prov-mod-compat" class="kv-hint muted" hidden></div>' +
      '<div class="kv"><span>期望状态</span><div>' + radios + "</div></div>" +

      '<label class="kv"><span>子网</span>' +
        '<select id="prov-mod-subnet">' + snOpts + "</select></label>" +
      '<div id="prov-mod-subnet-hint" class="kv-hint muted" hidden></div>' +

      '<label class="kv"><span>静态地址</span>' +
        '<input type="text" id="prov-mod-static" value="' +
        MK.escapeHTML(b.static_address || "") +
        '" placeholder="例如 192.168.10.42" autocomplete="off"></label>' +
      '<div id="prov-mod-static-hint" class="kv-hint muted" hidden></div>' +

      '<label class="kv"><span>VLAN 覆盖</span>' +
        '<input type="number" id="prov-mod-vlan" min="0" max="4094" value="' +
        (b.vlan_override || 0) + '" placeholder="0=用 subnet 默认"></label>' +

      '<label class="kv"><span>主机名</span>' +
        '<input type="text" id="prov-mod-hostname" value="' +
        MK.escapeHTML(b.hostname || "") +
        '" placeholder="可选覆盖" autocomplete="off"></label>' +

      '<label class="kv"><span>Bond</span>' +
        '<label><input type="checkbox" id="prov-mod-bond-en"' +
        (bondEnabled ? " checked" : "") + "> 启用 bond</label></label>" +
      '<div id="prov-mod-bond-fields" ' + (bondEnabled ? "" : "hidden") + ">" +
        '<label class="kv"><span>模式</span>' +
          '<select id="prov-mod-bond-mode">' + bondModeOpts + "</select></label>" +
        '<label class="kv"><span>Slaves</span>' +
          '<input type="text" id="prov-mod-bond-slaves" value="' +
          MK.escapeHTML(slavesCSV) +
          '" placeholder="例如 eno1, eno2"></label>' +
        '<div class="kv-hint muted">2..8 个 NIC，逗号分隔。可以是接口名（eno1）或 MAC 地址。</div>' +
      "</div>" +
      "</div>";

    const footer =
      '<button type="button" class="btn btn-ghost" data-modal-close>取消</button> ' +
      '<button type="submit" id="prov-mod-save" class="btn btn-primary">保存 binding</button>';

    const title = currentBinding ? "编辑 binding" : "创建 binding";
    const dlg = MK.openModal(MK.modalShell(title, body, footer));
    const form = dlg.querySelector("form");
    const imgSel = dlg.querySelector("#prov-mod-image");
    const profSel = dlg.querySelector("#prov-mod-profile");
    const compatEl = dlg.querySelector("#prov-mod-compat");
    const snSel = dlg.querySelector("#prov-mod-subnet");
    const snHint = dlg.querySelector("#prov-mod-subnet-hint");
    const staticInp = dlg.querySelector("#prov-mod-static");
    const staticHint = dlg.querySelector("#prov-mod-static-hint");
    const bondEn = dlg.querySelector("#prov-mod-bond-en");
    const bondFields = dlg.querySelector("#prov-mod-bond-fields");

    function refreshCompat() {
      const imgFam = imgFamilyByID[imgSel.value] || "";
      const profFam = profFamilyByID[profSel.value] || "any";
      if (!imgSel.value || !profSel.value) {
        compatEl.hidden = true;
        return;
      }
      if (profFam === "any" || imgFam === "" || imgFam === profFam) {
        compatEl.hidden = true;
        return;
      }
      compatEl.hidden = false;
      compatEl.innerHTML =
        '<span class="badge" data-status="pending">⚠ 兼容性</span> 镜像家族 <code>' +
        MK.escapeHTML(imgFam) + "</code> 与配置 os_family <code>" +
        MK.escapeHTML(profFam) + "</code> 不匹配，保存时后端会拒绝。";
    }
    function refreshSubnetHint() {
      const id = snSel.value;
      if (!id) {
        snHint.hidden = true;
        return;
      }
      const s = catalog.subnetByID && catalog.subnetByID[id];
      if (!s) {
        snHint.hidden = true;
        return;
      }
      const dns = (s.dns || []).join(", ") || "—";
      snHint.hidden = false;
      snHint.innerHTML =
        "CIDR <code>" + MK.escapeHTML(s.cidr) + "</code> · 网关 <code>" +
        MK.escapeHTML(s.gateway) + "</code> · DNS " + MK.escapeHTML(dns) +
        (s.vlan_id ? " · VLAN " + s.vlan_id : "");
    }
    function refreshStaticHint() {
      const id = snSel.value;
      const host = staticInp.value.trim();
      if (!id || !host) {
        staticHint.hidden = true;
        return;
      }
      const s = catalog.subnetByID && catalog.subnetByID[id];
      if (!s) {
        staticHint.hidden = true;
        return;
      }
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
    function refreshBondFields() {
      bondFields.hidden = !bondEn.checked;
    }
    imgSel.addEventListener("change", refreshCompat);
    profSel.addEventListener("change", refreshCompat);
    snSel.addEventListener("change", function () {
      refreshSubnetHint();
      refreshStaticHint();
    });
    staticInp.addEventListener("input", refreshStaticHint);
    bondEn.addEventListener("change", refreshBondFields);
    refreshCompat();
    refreshSubnetHint();
    refreshStaticHint();
    refreshBondFields();
    form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      submitBinding(dlg);
    });
  }

  // hostInSubnetCheck mirrors internal/subnets HostInSubnet for snappy
  // client-side feedback. Returns "" if OK, else a Chinese error string.
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

  async function submitBinding(dlg) {
    const imageID = dlg.querySelector("#prov-mod-image").value.trim();
    const profileID = dlg.querySelector("#prov-mod-profile").value.trim();
    const desired = (dlg.querySelector('input[name="prov-desired"]:checked') || {}).value || "none";
    const staticAddr = dlg.querySelector("#prov-mod-static").value.trim();
    const hostname = dlg.querySelector("#prov-mod-hostname").value.trim();
    const subnetID = dlg.querySelector("#prov-mod-subnet").value.trim();
    const vlanRaw = dlg.querySelector("#prov-mod-vlan").value.trim();
    const bondEn = dlg.querySelector("#prov-mod-bond-en").checked;

    if (!imageID || !profileID) {
      MK.flashError("镜像与配置必填");
      return;
    }

    // Client-side host-in-subnet pre-check for snappier UX. The backend repeats
    // the check authoritatively.
    if (subnetID && staticAddr) {
      const s = catalog.subnetByID && catalog.subnetByID[subnetID];
      if (s) {
        const err = hostInSubnetCheck(staticAddr, s.cidr, s.gateway);
        if (err) {
          MK.flashError(err);
          return;
        }
      }
    }
    // 允许空的静态地址 - 后端会自动分配一个未使用的 IP
    // if (subnetID && !staticAddr) {
    //   MK.flashError("绑定 subnet 时必须填静态地址");
    //   return;
    // }

    let vlanInt = 0;
    if (vlanRaw !== "") {
      vlanInt = parseInt(vlanRaw, 10);
      if (Number.isNaN(vlanInt) || vlanInt < 0 || vlanInt > 4094) {
        MK.flashError("VLAN 覆盖必须是 0..4094 范围内的整数");
        return;
      }
    }

    let bond = null;
    if (bondEn) {
      const mode = dlg.querySelector("#prov-mod-bond-mode").value;
      const slavesCSV = dlg.querySelector("#prov-mod-bond-slaves").value;
      const slaves = slavesCSV.split(",").map(function (s) { return s.trim(); })
        .filter(function (s) { return s.length > 0; });
      if (slaves.length < 2 || slaves.length > 8) {
        MK.flashError("Bond slaves 必须 2..8 个");
        return;
      }
      bond = { mode: mode, slaves: slaves };
    }

    const saveBtn = dlg.querySelector("#prov-mod-save");
    saveBtn.disabled = true;
    try {
      const body = {
        image_id: imageID,
        profile_id: profileID,
        desired_state: desired,
        static_address: staticAddr,
        hostname: hostname,
        subnet_id: subnetID,            // "" = explicit clear
        vlan_override: vlanInt,         // 0 = clear
        bond: bond,                     // null = explicit clear
      };
      currentBinding = await MK.apiSend("PUT", "/bindings/" + encodeURIComponent(muuid), body);
      MK.closeModal();
      MK.flashSuccess("binding 已保存");
      MK.clearError();
      renderBindingCard();
      renderActionsCard();
    } catch (e) {
      MK.flashError(e.message);
    } finally {
      saveBtn.disabled = false;
    }
  }

  async function clearBinding() {
    if (!currentBinding) return;
    if (!confirm("清除该机器的 binding？这将停止后续装机协调。")) {
      return;
    }
    try {
      await MK.apiSend("DELETE", "/bindings/" + encodeURIComponent(muuid), null);
      currentBinding = null;
      MK.flashSuccess("binding 已清除");
      MK.clearError();
      renderBindingCard();
      renderActionsCard();
    } catch (e) {
      MK.flashError(e.message);
    }
  }

  // ---- 卡片 2：BMC ----------------------------------------------------

  function renderBMCCard() {
    const body = document.getElementById("prov-bmc-body");
    const testBtn = document.getElementById("prov-bmc-test");
    const editBtn = document.getElementById("prov-bmc-edit");

    if (!currentBMC) {
      body.className = "muted";
      body.textContent = "尚未配置 BMC。";
      testBtn.disabled = true;
      editBtn.textContent = "配置 BMC";
      return;
    }

    testBtn.disabled = false;
    editBtn.textContent = "编辑 BMC";
    body.className = "";

    const ip = currentBMC.ip || "-";
    const port = currentBMC.port ? ":" + currentBMC.port : "";
    const updatedAt = currentBMC.updated_at ? MK.fmtISO(currentBMC.updated_at) : "-";
    const name = (currentBMC.name || "").trim();

    const rows = [];
    if (name) {
      rows.push(["名称", MK.escapeHTML(name)]);
    }
    rows.push(
      ["地址", MK.escapeHTML(ip + port)],
      ["用户名", MK.escapeHTML(currentBMC.username || "-")],
      ["接口", MK.escapeHTML(currentBMC.ipmi_interface || "lanplus")],
      ["更新时间", MK.escapeHTML(updatedAt) +
        (currentBMC.updated_by
          ? ' <span class="muted">操作人：' + MK.escapeHTML(currentBMC.updated_by) + "</span>"
          : "")],
    );
    body.innerHTML =
      '<dl class="kv-strip">' +
      rows.map(function (r) {
        return "<div><dt>" + MK.escapeHTML(r[0]) + "</dt><dd>" + r[1] + "</dd></div>";
      }).join("") +
      "</dl>";
  }

  function openBMCModal() {
    const b = currentBMC || { ip: "", port: 623, username: "", ipmi_interface: "lanplus", name: "" };
    const ifaces = ["lanplus", "lan"];
    const ifaceOpts = ifaces.map(function (i) {
      const sel = i === (b.ipmi_interface || "lanplus") ? " selected" : "";
      return '<option value="' + i + '"' + sel + ">" + i + "</option>";
    }).join("");

    const pwdPlaceholder = currentBMC ? "（留空保持现有值）" : "必填";

    const body =
      '<div class="kv-form">' +
      '<label class="kv"><span>机器 UUID</span>' +
        '<input type="text" value="' + MK.escapeHTML(muuid) +
        '" readonly disabled class="mono"></label>' +
      '<label class="kv"><span>名称 <span class="muted">（可选别名）</span></span>' +
        '<input type="text" id="prov-bmc-name" maxlength="64" value="' +
        MK.escapeHTML(b.name || "") + '" autocomplete="off" placeholder="例如：rack01-r630-01"></label>' +
      '<label class="kv"><span>IP 地址</span>' +
        '<input type="text" id="prov-bmc-ip" value="' + MK.escapeHTML(b.ip || "") +
        '" required autocomplete="off"></label>' +
      '<label class="kv"><span>端口</span>' +
        '<input type="number" id="prov-bmc-port" min="1" max="65535" value="' +
        MK.escapeHTML(String(b.port || 623)) + '" autocomplete="off"></label>' +
      '<label class="kv"><span>用户名</span>' +
        '<input type="text" id="prov-bmc-username" value="' +
        MK.escapeHTML(b.username || "") + '" required autocomplete="off"></label>' +
      '<label class="kv"><span>密码</span>' +
        '<input type="password" id="prov-bmc-password" placeholder="' + pwdPlaceholder +
        '" autocomplete="new-password"></label>' +
      '<label class="kv"><span>接口</span>' +
        '<select id="prov-bmc-iface">' + ifaceOpts + "</select></label>" +
      "</div>";

    const footer =
      '<button type="button" class="btn btn-ghost" data-modal-close>取消</button> ' +
      '<button type="submit" id="prov-bmc-save" class="btn btn-primary">' +
      (currentBMC ? "保存 BMC" : "创建 BMC") + "</button>";

    const title = currentBMC ? "编辑 BMC" : "配置 BMC";
    const dlg = MK.openModal(MK.modalShell(title, body, footer));
    dlg.querySelector("form").addEventListener("submit", function (ev) {
      ev.preventDefault();
      submitBMC(dlg);
    });
  }

  async function submitBMC(dlg) {
    const name = (dlg.querySelector("#prov-bmc-name").value || "").trim();
    const ip = dlg.querySelector("#prov-bmc-ip").value.trim();
    const portRaw = dlg.querySelector("#prov-bmc-port").value.trim();
    const username = dlg.querySelector("#prov-bmc-username").value.trim();
    const password = dlg.querySelector("#prov-bmc-password").value; // 不要 trim
    const iface = dlg.querySelector("#prov-bmc-iface").value;

    if (!ip || !username) {
      MK.flashError("IP 与用户名必填");
      return;
    }
    if (name.length > 64) {
      MK.flashError("名称长度不能超过 64");
      return;
    }
    if (!currentBMC && !password) {
      MK.flashError("新建 BMC 时密码必填");
      return;
    }

    const body = {
      name: name,
      ip: ip,
      username: username,
      ipmi_interface: iface,
    };
    if (portRaw) {
      const p = parseInt(portRaw, 10);
      if (!isNaN(p)) body.port = p;
    }
    if (password) body.password = password;

    const saveBtn = dlg.querySelector("#prov-bmc-save");
    saveBtn.disabled = true;
    try {
      currentBMC = await MK.apiSend("PUT", "/bmc/" + encodeURIComponent(muuid), body);
      MK.closeModal();
      MK.flashSuccess("BMC 已保存");
      MK.clearError();
      renderBMCCard();
    } catch (e) {
      MK.flashError(e.message);
    } finally {
      saveBtn.disabled = false;
    }
  }

  async function testBMC() {
    if (!currentBMC) return;
    const btn = document.getElementById("prov-bmc-test");
    btn.disabled = true;
    const orig = btn.textContent;
    btn.textContent = "测试中…";
    try {
      const r = await MK.apiSend("POST", "/bmc/" + encodeURIComponent(muuid) + "/test", null);
      if (r && r.ok === false) {
        MK.flashError("BMC 测试失败：" + (r.error || "未知错误"));
      } else if (r && r.ok === true) {
        MK.flashSuccess("BMC 可达" + (r.power ? "（电源：" + r.power + "）" : ""));
      } else {
        // 端点返回 204 或我们不识别的 payload —— 当成 OK。
        MK.flashSuccess("BMC 测试已发出");
      }
    } catch (e) {
      if (e && e.status === 404) {
        MK.flashError("BMC 测试端点尚未上线");
      } else {
        MK.flashError("BMC 测试失败：" + e.message);
      }
    } finally {
      btn.disabled = false;
      btn.textContent = orig;
    }
  }

  // ---- 卡片 3：装机操作 -----------------------------------------------

  function renderActionsCard() {
    const body = document.getElementById("prov-actions-body");

    body.className = "";
    const hasSucceeded = recentJobs.some(function (j) { return j.status === "succeeded"; });
    const inflight = recentJobs.find(function (j) {
      return j.status === "pending" || j.status === "running";
    });
    // 默认主按钮：有 succeeded 历史 → 重装，否则 → 装机。
    // 无论是否已有 binding，都允许点击 —— 弹窗里会让用户确认/补全字段。
    const primaryLabel = hasSucceeded ? "重装" : "装机";
    const primaryState = hasSucceeded ? "reinstall" : "install";

    let html = "";
    if (!currentBinding) {
      html += '<p class="muted">该机器尚未配置 binding。点击下方按钮即可在弹窗里一次性' +
        '选择镜像、配置、主机名、静态地址并触发装机。</p>';
    } else {
      html += '<p>点击下方按钮打开装机弹窗：可调整镜像、配置、主机名、静态地址，' +
        '提交后 orchestrator 通过 BMC 重启该机器。</p>';
    }
    if (!currentBMC) {
      html += '<p class="muted">注意：尚未配置 BMC —— orchestrator ' +
        '在添加 BMC 之前无法重启该机器。</p>';
    }
    html += '<div class="action-row">' +
      '<button type="button" id="prov-install-btn" class="btn btn-primary">' +
      MK.escapeHTML(primaryLabel) + "</button>";
    if (inflight) {
      html += ' <button type="button" id="prov-cancel-btn" class="btn btn-danger" ' +
        'data-job-id="' + MK.escapeHTML(inflight.id || "") + '">取消当前作业</button>';
    }
    html += "</div>";

    if (inflight) {
      html += '<p class="muted">在途作业：' +
        '<a href="/ui/jobs/' + encodeURIComponent(inflight.id || "") + '">' +
        MK.escapeHTML(inflight.id || "") + "</a> — " +
        '<span class="badge" data-status="' + MK.escapeHTML(inflight.status) + '">' +
        MK.escapeHTML(statusLabel(inflight.status)) + "</span>" +
        (inflight.stage ? " — " + MK.escapeHTML(inflight.stage) : "") + "</p>";
    }

    body.innerHTML = html;

    document.getElementById("prov-install-btn").addEventListener("click", function () {
      triggerInstall(primaryState, primaryLabel);
    });
    const cancelBtn = document.getElementById("prov-cancel-btn");
    if (cancelBtn) {
      cancelBtn.addEventListener("click", function () {
        cancelJob(cancelBtn.dataset.jobId);
      });
    }
  }

  async function triggerInstall(desiredState, label) {
    // 改走统一的装机弹窗：让操作员在点击装机时直接确认/覆盖
    // 镜像、配置、主机名、静态地址，再提交。
    await MK.openInstallModal(muuid, {
      defaults: {
        image_id: currentBinding ? currentBinding.image_id : "",
        profile_id: currentBinding ? currentBinding.profile_id : "",
        hostname: currentBinding ? currentBinding.hostname : "",
        static_address: currentBinding ? currentBinding.static_address : "",
        subnet_id: currentBinding ? (currentBinding.subnet_id || "") : "",
        // 把当前 bond override 喂进弹窗：复选框/字段能反映现状。
        // 不勾选提交即清除（弹窗是 override 真相源）。
        bond: currentBinding ? (currentBinding.bond || null) : null,
        // 当前 NIC selector override（"" / "by-mac:..."），弹窗会据此预选 radio。
        nic_selector_override: currentBinding ? (currentBinding.nic_selector_override || "") : "",
        desired_state: desiredState,
      },
      suggestState: desiredState,
      onSuccess: async function (updated) {
        currentBinding = updated;
        await loadRecentJobs();
        renderBindingCard();
        renderRecentJobsCard();
        renderActionsCard();
      },
    });
  }

  async function cancelJob(jobID) {
    if (!jobID) return;
    if (!confirm("取消作业 " + jobID + "？")) return;
    try {
      await MK.apiSend("POST", "/jobs/" + encodeURIComponent(jobID) + "/cancel", null);
      MK.flashSuccess("作业已取消");
      MK.clearError();
      await loadRecentJobs();
      renderRecentJobsCard();
      renderActionsCard();
    } catch (e) {
      MK.flashError(e.message);
    }
  }

  // ---- 卡片 4：最近作业 -----------------------------------------------

  function renderRecentJobsCard() {
    const body = document.getElementById("prov-recent-body");
    if (!recentJobs || recentJobs.length === 0) {
      body.className = "muted";
      body.textContent = "暂无作业。";
      return;
    }
    body.className = "";
    const items = recentJobs.slice(0, 5).map(function (j) {
      const id = j.id || "";
      const started = j.started_at || j.created_at || null;
      const rel = started ? MK.fmtRelative(Date.parse(started) / 1000) : "-";
      const abs = started ? MK.fmtISO(started) : "-";
      return '<li>' +
        '<a class="mono" href="/ui/jobs/' + encodeURIComponent(id) + '">' +
        MK.escapeHTML(MK.truncate(id, 12)) + "</a>" +
        ' <span class="badge" data-status="' + MK.escapeHTML(j.type || "") + '">' +
        MK.escapeHTML(typeLabel(j.type)) + "</span>" +
        ' <span class="badge" data-status="' + MK.escapeHTML(j.status || "") + '">' +
        MK.escapeHTML(statusLabel(j.status)) + "</span>" +
        ' <span class="muted" title="' + MK.escapeHTML(abs) + '">' +
        MK.escapeHTML(rel) + "</span>" +
        (j.stage ? ' <span class="muted">— ' + MK.escapeHTML(j.stage) + "</span>" : "") +
        "</li>";
    }).join("");
    body.innerHTML = '<ul class="reports-list">' + items + "</ul>";
  }
})();
