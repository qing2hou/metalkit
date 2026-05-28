// metalkit operator UI — BMC credentials page.
//
// Lists BMC credentials per machine; CRUD via modal. Editing leaves password
// blank by default so the stored ciphertext is preserved (PUT omits password
// field → store keeps existing). "Test now" calls POST /bmc/{uuid}/test which
// runs `ipmitool chassis power status` server-side and returns:
//   200 {ok:true,  power:"on"|"off"|"unknown"}   — probe succeeded
//   200 {ok:false, error:"..."}                  — probe ran but ipmi failed
//   503 — controller has no ipmi tester wired
//   404 — no BMC for this machine_uuid
//
// Schema reference: internal/bmc/store.go (Credential, UpsertInput) — see also
// internal/bmc/api.go for HTTP shape and the /test endpoint contract.

(function () {
  "use strict";

  const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
  const PLACEHOLDER_RE = /^placeholder-(\d{1,3})-(\d{1,3})-(\d{1,3})-(\d{1,3})$/;
  const IPV4_RE = /^(\d{1,3}\.){3}\d{1,3}$/;
  const VALID_IFACES = ["lan", "lanplus", "lanplus+cipher3"];
  // Power actions surfaced in the UI. Maps to POST /bmc/{uuid}/power/{action}.
  // `destructive` controls the confirm wording — a hard "off"/"cycle"/"reset"
  // can corrupt running workloads, so we require explicit "我了解风险" wording.
  const POWER_ACTIONS = [
    { id: "on",    label: "上电",     desc: "Power on (no-op if already on)",          destructive: false },
    { id: "soft",  label: "软关机",   desc: "ACPI soft-off (graceful shutdown)",        destructive: true  },
    { id: "off",   label: "强制关机", desc: "Hard power-off — no graceful shutdown",    destructive: true  },
    { id: "cycle", label: "重启",     desc: "Power cycle (warm reboot if on, on if off)", destructive: true  },
    { id: "reset", label: "硬复位",   desc: "Hardware reset — equivalent to reset button", destructive: true  },
  ];

  let machineByUUID = {}; // uuid -> {uuid, product, ...} from /api/v1/machines

  document.addEventListener("DOMContentLoaded", function () {
    document.getElementById("refresh-btn").addEventListener("click", reload);
    document.getElementById("new-bmc-btn").addEventListener("click", function () {
      openBMCModal(null);
    });
    document.getElementById("import-bmc-btn").addEventListener("click", function () {
      openImportModal();
    });
    reload();
  });

  async function reload() {
    MK.clearError();
    const loading = document.getElementById("bmc-loading");
    const empty = document.getElementById("bmc-empty");
    const wrap = document.getElementById("bmc-wrap");
    loading.hidden = false;
    empty.hidden = true;
    wrap.hidden = true;
    try {
      // Pull machine lookup in parallel with bmc list so the table can show
      // friendly product strings alongside the uuid8.
      const [bmcs, machines] = await Promise.all([
        MK.apiGet("/bmc"),
        MK.apiGet("/machines").catch(function () { return []; }),
      ]);
      const mlist = Array.isArray(machines) ? machines : (machines && machines.machines) || [];
      machineByUUID = {};
      for (const m of mlist) {
        if (m && m.uuid) machineByUUID[String(m.uuid).toLowerCase()] = m;
      }
      render(bmcs || []);
    } catch (err) {
      MK.flashError("加载 BMC 列表失败：" + err.message);
      loading.hidden = true;
    }
  }

  function render(list) {
    const loading = document.getElementById("bmc-loading");
    const empty = document.getElementById("bmc-empty");
    const wrap = document.getElementById("bmc-wrap");
    const count = document.getElementById("bmc-count");
    loading.hidden = true;
    count.textContent = "共 " + list.length + " 条";
    if (list.length === 0) {
      empty.hidden = false;
      return;
    }
    wrap.hidden = false;
    const tbody = document.getElementById("bmc-tbody");
    tbody.innerHTML = "";
    for (const c of list) {
      const tr = document.createElement("tr");
      tr.appendChild(nameCell(c));
      tr.appendChild(machineCell(c));
      tr.appendChild(addrCell(c));
      tr.appendChild(textCell(c.username || "-"));
      tr.appendChild(textCell(c.ipmi_interface || "-"));
      tr.appendChild(updatedCell(c));
      tr.appendChild(actionsCell(c));
      tbody.appendChild(tr);
    }
  }

  function nameCell(c) {
    const td = document.createElement("td");
    const n = (c.name || "").trim();
    if (n) {
      td.textContent = n;
    } else {
      td.innerHTML = '<span class="muted">—</span>';
    }
    return td;
  }

  function machineCell(c) {
    const td = document.createElement("td");
    const mu = String(c.machine_uuid || "");
    if (PLACEHOLDER_RE.test(mu)) {
      // 占位 UUID（按 BMC IP 注册，机器尚未上报） —— 用 IP 显示 + 待对账徽标。
      td.innerHTML =
        '<span class="mono" title="' + MK.escapeHTML(mu) + '">' +
        MK.escapeHTML("BMC@" + (c.ip || "?")) + '</span>' +
        ' <span class="badge" data-status="pending" title="目标机首次 PXE 上报后会自动迁移到真实 UUID">待对账</span>';
      return td;
    }
    const m = machineByUUID[mu.toLowerCase()];
    const product = m && m.product ? m.product : "";
    td.innerHTML =
      '<a class="mono" href="/ui/m/' + encodeURIComponent(mu) + '" title="' +
      MK.escapeHTML(mu) + '">' + MK.escapeHTML(mu.slice(0, 8)) + '</a>' +
      (product ? '<div class="muted">' + MK.escapeHTML(product) + '</div>' : '');
    return td;
  }

  function addrCell(c) {
    const td = document.createElement("td");
    td.className = "mono";
    td.textContent = (c.ip || "-") + ":" + (c.port || 623);
    return td;
  }

  function textCell(t) {
    const td = document.createElement("td");
    td.textContent = t;
    return td;
  }

  function updatedCell(c) {
    const td = document.createElement("td");
    const iso = c.updated_at || "";
    td.innerHTML = MK.escapeHTML(MK.fmtISO(iso)) +
      (c.updated_by ? '<div class="muted">操作人：' + MK.escapeHTML(c.updated_by) + '</div>' : '');
    return td;
  }

  function actionsCell(c) {
    const td = document.createElement("td");
    td.className = "col-actions";
    const isPlaceholder = PLACEHOLDER_RE.test(String(c.machine_uuid || ""));
    const muEsc = MK.escapeHTML(c.machine_uuid);

    // Power dropdown — always available (works for placeholder too; we only
    // need a reachable BMC IP, not a real machine row).
    let powerOpts = '<option value="">电源…</option>';
    for (const a of POWER_ACTIONS) {
      powerOpts += '<option value="' + a.id + '">' + a.label + '</option>';
    }

    // 纳管：bootdev=pxe + power cycle —— 让目标机进 live 上报硬件。占位 UUID
    // 的主战场（首次发现），真实 UUID 也可用于"重新发现 / 诊断"。**不装系统** ——
    // 没有 binding 的话 agent claim 不到 job，纯采集。
    const onboardTitle = isPlaceholder
      ? "PXE 进 live 上报硬件 —— 占位 UUID 会迁移到真实 SMBIOS UUID"
      : "重新进 live（不装系统） —— 用于硬件信息刷新 / 诊断";

    td.innerHTML =
      '<button type="button" class="btn test-btn" data-uuid="' + muEsc + '">测试</button> ' +
      '<select class="btn power-select" data-uuid="' + muEsc + '">' + powerOpts + '</select> ' +
      '<button type="button" class="btn onboard-btn" data-uuid="' + muEsc + '" title="' + MK.escapeHTML(onboardTitle) + '">纳管</button> ' +
      (isPlaceholder
        ? ''
        : '<button type="button" class="btn reinstall-btn" data-uuid="' + muEsc + '" title="打开重装对话框 —— 可选择 image + profile 后触发 PXE 重启">重装</button> ') +
      '<button type="button" class="btn edit-btn" data-uuid="' + muEsc + '">编辑</button> ' +
      '<button type="button" class="btn btn-danger delete-btn" data-uuid="' + muEsc + '">删除</button>';

    td.querySelector(".test-btn").addEventListener("click", function () { testBMC(c); });
    td.querySelector(".edit-btn").addEventListener("click", function () { editBMC(c); });
    td.querySelector(".delete-btn").addEventListener("click", function () { deleteBMC(c); });
    td.querySelector(".onboard-btn").addEventListener("click", function () { onboardMachine(c); });
    const sel = td.querySelector(".power-select");
    sel.addEventListener("change", function () {
      const action = sel.value;
      sel.value = "";
      if (action) powerBMC(c, action);
    });
    const reBtn = td.querySelector(".reinstall-btn");
    if (reBtn) reBtn.addEventListener("click", function () { reinstallMachine(c); });
    return td;
  }

  // ---- create / edit modal -------------------------------------------

  async function editBMC(existing) {
    openBMCModal(existing);
  }

  function openBMCModal(existing) {
    const isEdit = !!existing;
    const titleSuffix = isEdit
      ? ((existing.name || "").trim() || (existing.machine_uuid || "").slice(0, 8))
      : "";
    const title = isEdit ? "编辑 BMC：" + titleSuffix : "新建 BMC 凭据";
    const body = renderForm(existing);
    const footer =
      '<button type="button" class="btn" data-modal-close>取消</button>' +
      '<button type="button" id="bmc-save" class="btn btn-primary">' +
      (isEdit ? "保存" : "创建") + '</button>';
    const dlg = MK.openModal(MK.modalShell(title, body, footer));

    dlg.querySelector("#bmc-save").addEventListener("click", function () {
      submitForm(dlg, existing).catch(function (err) {
        showFormError(dlg, err.message);
      });
    });
  }

  function renderForm(existing) {
    const isEdit = !!existing;
    const muuid = isEdit ? existing.machine_uuid : "";
    const name = isEdit ? (existing.name || "") : "";
    const ip = isEdit ? existing.ip : "";
    const port = isEdit ? existing.port : 623;
    const user = isEdit ? existing.username : "";
    const iface = isEdit ? existing.ipmi_interface : "lanplus";

    let ifaceOpts = "";
    for (const v of VALID_IFACES) {
      ifaceOpts += '<option value="' + v + '"' + (iface === v ? " selected" : "") + '>' + v + '</option>';
    }

    return (
      '<div class="form-error error-banner" id="form-error" hidden></div>' +
      '<div class="kv-form">' +

      (isEdit
        ? '<label>机器 UUID' +
          '<input type="text" value="' + MK.escapeHTML(muuid) + '" disabled class="mono">' +
          '<span class="label-hint">不可变 —— 如需更换请删除后重建。</span></label>'
        : '<div class="label-hint" style="margin-bottom:.5em">' +
          '按 BMC IP 注册：服务端会生成占位 UUID（<code>placeholder-&lt;ip-dashed&gt;</code>），' +
          '目标机首次 PXE 上报后自动迁移到真实 SMBIOS UUID。' +
          '</div>') +

      '<label for="f-name">名称 <span class="muted">（可选）</span>' +
      '<input type="text" id="f-name" maxlength="64" placeholder="例如：rack01-r630-01" value="' +
      MK.escapeHTML(name) + '" autocomplete="off">' +
      '<span class="label-hint">显示别名，列表/详情页会优先用它代替 UUID。留空则继续显示 UUID。</span></label>' +

      '<div class="row-2col">' +
        '<label for="f-ip">IP（IPv4）' +
        '<input type="text" id="f-ip" required placeholder="10.0.0.10" value="' +
        MK.escapeHTML(ip) + '" autocomplete="off"></label>' +
        '<label for="f-port">端口' +
        '<input type="number" id="f-port" min="1" max="65535" value="' +
        (port || 623) + '"></label>' +
      '</div>' +

      '<label for="f-user">用户名' +
      '<input type="text" id="f-user" required value="' + MK.escapeHTML(user) + '" autocomplete="off"></label>' +

      '<label for="f-pass">密码' +
      (isEdit ? '' : ' <span class="muted">（必填）</span>') +
      '<input type="password" id="f-pass" autocomplete="new-password" placeholder="' +
      (isEdit ? "留空保持现有值" : "与 BMC 一致的密码") + '">' +
      '<span class="label-hint">' +
      (isEdit
        ? "留空保持现有密文；输入新值则轮换。"
        : "使用 controller 主密钥静态加密。") +
      '</span></label>' +

      '<label for="f-iface">IPMI 接口' +
      '<select id="f-iface">' + ifaceOpts + '</select>' +
      '<span class="label-hint">Dell/Supermicro 多数 BMC 选 <code>lanplus</code>。</span></label>' +

      '</div>'
    );
  }

  function showFormError(dlg, msg) {
    const e = dlg.querySelector("#form-error");
    if (!e) return;
    e.textContent = msg;
    e.hidden = false;
  }

  async function submitForm(dlg, existing) {
    const isEdit = !!existing;
    const name = (dlg.querySelector("#f-name").value || "").trim();
    const ip = (dlg.querySelector("#f-ip").value || "").trim();
    const port = parseInt(dlg.querySelector("#f-port").value, 10);
    const user = (dlg.querySelector("#f-user").value || "").trim();
    const pass = dlg.querySelector("#f-pass").value;
    const iface = dlg.querySelector("#f-iface").value;

    if (name.length > 64) {
      throw new Error("名称长度不能超过 64");
    }
    if (!IPV4_RE.test(ip)) {
      throw new Error("ip 必须是 IPv4 点分四段");
    }
    if (!(port >= 1 && port <= 65535)) {
      throw new Error("port 必须在 1..65535");
    }
    if (!user) {
      throw new Error("username 必填");
    }
    if (!isEdit && !pass) {
      throw new Error("新建 BMC 时 password 必填");
    }
    if (VALID_IFACES.indexOf(iface) === -1) {
      throw new Error("ipmi_interface 非法");
    }

    const body = {
      name: name,
      ip: ip,
      port: port,
      username: user,
      ipmi_interface: iface,
    };
    if (pass) body.password = pass;

    if (isEdit) {
      // 编辑：PUT 到原 UUID（含占位 UUID 也可以原地改）
      await MK.apiSend("PUT", "/bmc/" + encodeURIComponent(existing.machine_uuid), body);
    } else {
      // 新建：POST /bmc — 服务端按 IP 派生占位 UUID 并自动建 machines 占位行。
      // 409 表示该 IP 已有 BMC 注册，错误信息里会附带 existing_machine_uuid。
      try {
        await MK.apiSend("POST", "/bmc", body);
      } catch (err) {
        // apiSend 抛的 err.message 已包含服务端 JSON 的 error 字段；
        // 409 的特殊响应里有 existing_machine_uuid，提示更具体一些。
        throw err;
      }
    }
    MK.closeModal();
    MK.flashSuccess(isEdit ? "BMC 已更新" : "BMC 已创建（占位 UUID，等待目标机首次上报）");
    reload();
  }

  // ---- delete -------------------------------------------------------

  async function deleteBMC(c) {
    const muuid = c.machine_uuid;
    const label = (c.name || "").trim() || muuid.slice(0, 8);
    if (!confirm("删除 BMC“" + label + "”的凭据？\n\n删除后无法通过该 BMC 重装机器，除非重新登记。")) {
      return;
    }
    try {
      await MK.apiSend("DELETE", "/bmc/" + encodeURIComponent(muuid));
      MK.flashSuccess("BMC 已删除");
      reload();
    } catch (err) {
      MK.flashError("删除失败：" + err.message);
    }
  }

  // ---- test ---------------------------------------------------------

  async function testBMC(c) {
    const muuid = c.machine_uuid;
    const label = (c.name || "").trim() || muuid.slice(0, 8);
    MK.clearError();
    try {
      const r = await MK.apiSend("POST", "/bmc/" + encodeURIComponent(muuid) + "/test", null);
      if (r && r.ok) {
        MK.flashSuccess("BMC “" + label + "” 可达 —— 电源：" + (r.power || "未知"));
      } else if (r && r.ok === false) {
        MK.flashError("BMC 测试失败：" + (r.error || "未知错误"));
      } else {
        MK.flashError("BMC 测试：响应异常");
      }
    } catch (err) {
      // 404/503/etc surface here.
      MK.flashError("BMC 测试失败：" + err.message);
    }
  }

  // ---- power -------------------------------------------------------
  //
  // POST /bmc/{uuid}/power/{action} — controller wraps `ipmitool chassis power`.
  // Destructive actions get a stricter confirm (operator must type "确认").
  // 同 /test endpoint：服务端遇到 ipmi 失败也返回 200 {ok:false,error:...}，
  // 这里逐项判断并通过 flash 显示。

  async function powerBMC(c, actionID) {
    const spec = POWER_ACTIONS.find(function (a) { return a.id === actionID; });
    if (!spec) return;
    const muuid = c.machine_uuid;
    const label = (c.name || "").trim() || (PLACEHOLDER_RE.test(muuid) ? "BMC@" + (c.ip || "?") : muuid.slice(0, 8));

    if (spec.destructive) {
      const ok = window.prompt(
        "⚠️ 即将对 " + label + " 执行：" + spec.label + "\n\n" +
        spec.desc + "\n\n" +
        "此操作不可撤销，会立即对目标机生效。\n" +
        "请输入【确认】以继续：");
      if (ok !== "确认") {
        MK.flashError("已取消");
        return;
      }
    } else {
      if (!confirm("对 " + label + " 执行：" + spec.label + " ？")) return;
    }

    MK.clearError();
    try {
      const r = await MK.apiSend("POST",
        "/bmc/" + encodeURIComponent(muuid) + "/power/" + encodeURIComponent(actionID), null);
      if (r && r.ok) {
        MK.flashSuccess("BMC “" + label + "” 已执行 " + spec.label);
      } else if (r && r.ok === false) {
        MK.flashError(spec.label + " 失败：" + (r.error || "未知错误"));
      } else {
        MK.flashError(spec.label + "：响应异常");
      }
    } catch (err) {
      MK.flashError(spec.label + " 失败：" + err.message);
    }
  }

  // ---- onboard -----------------------------------------------------
  //
  // POST /bmc/{uuid}/onboard — 设 bootdev=pxe + power cycle，让目标机进 live 上报
  // 硬件。**不装系统**：没有 binding 的话 agent claim 不到 job，纯采集；占位 UUID
  // 在首次上报后会自动迁移到真实 SMBIOS UUID。
  //
  // 真实 UUID 也可以用于"重新发现 / 诊断"——目标机会被强制重启回 live，
  // 现在跑的工作负载会被中断，所以仍然要二次确认。

  async function onboardMachine(c) {
    const muuid = c.machine_uuid;
    const isPlaceholder = PLACEHOLDER_RE.test(muuid);
    const label = (c.name || "").trim() ||
      (isPlaceholder ? "BMC@" + (c.ip || "?") : muuid.slice(0, 8));

    const intro = isPlaceholder
      ? "目标机会立即 PXE 重启进 live 系统并上报硬件，占位 UUID 会在收到首次报告后自动迁移到真实 SMBIOS UUID。"
      : "目标机会立即 PXE 重启进 live 系统刷新硬件信息。";

    const body =
      '<div class="form-error error-banner" id="form-error" hidden></div>' +
      '<p>即将对 <strong>' + MK.escapeHTML(label) + '</strong>（BMC ' +
      MK.escapeHTML(c.ip || "?") + '）触发<strong>纳管</strong>。</p>' +
      '<p>' + MK.escapeHTML(intro) + '</p>' +
      '<p class="muted">⚠️ 这只是<strong>上报硬件</strong>，不会安装操作系统。' +
      '如果目标机当前在跑业务，会被打断。</p>' +
      '<label style="display:flex;align-items:center;gap:.5em;margin-top:1em;">' +
      '<input type="checkbox" id="onboard-confirm">' +
      '<span>我已知晓上述风险，确认对目标机触发 PXE 重启</span></label>';
    const footer =
      '<button type="button" class="btn" data-modal-close>取消</button>' +
      '<button type="button" id="onboard-go" class="btn btn-primary" disabled>纳管</button>';
    const dlg = MK.openModal(MK.modalShell("纳管：" + label, body, footer));

    const cb = dlg.querySelector("#onboard-confirm");
    const go = dlg.querySelector("#onboard-go");
    cb.addEventListener("change", function () { go.disabled = !cb.checked; });

    go.addEventListener("click", async function () {
      if (!cb.checked) return;
      go.disabled = true;
      MK.clearError();
      try {
        const r = await MK.apiSend("POST",
          "/bmc/" + encodeURIComponent(muuid) + "/onboard", null);
        if (r && r.ok) {
          MK.closeModal();
          MK.flashSuccess("已请求 “" + label + "” 进 live —— 目标机将 PXE 重启并上报硬件");
        } else if (r && r.ok === false) {
          showFormError(dlg, "纳管失败：" + (r.error || "未知错误"));
          go.disabled = false;
        } else {
          showFormError(dlg, "纳管：响应异常");
          go.disabled = false;
        }
      } catch (err) {
        showFormError(dlg, "纳管失败：" + err.message);
        go.disabled = false;
      }
    });
  }

  // ---- reinstall ---------------------------------------------------
  //
  // 重装走 binding desired_state = "reinstall" —— orchestrator 下一 tick 检测到
  // 后会调 ipmitool chassis bootdev pxe + power cycle 强制目标机重进 live。
  // 需要 binding 已经设了 image + profile；占位 BMC 没有真实 machine_uuid，
  // actionsCell 已经隐藏了重装按钮，这里 defensively 再校验一次。

  async function reinstallMachine(c) {
    const muuid = c.machine_uuid;
    if (PLACEHOLDER_RE.test(muuid)) {
      MK.flashError("占位 BMC 还没绑定真实机器，无法重装");
      return;
    }
    const label = (c.name || "").trim() || muuid.slice(0, 8);

    let binding = null;
    try {
      binding = await MK.apiGet("/bindings/" + encodeURIComponent(muuid));
    } catch (err) {
      binding = null;
    }

    let images = [];
    let profiles = [];
    try {
      const results = await Promise.all([
        MK.apiGet("/images"),
        MK.apiGet("/profiles"),
      ]);
      images = (results[0] && results[0].items) || results[0] || [];
      profiles = (results[1] && results[1].items) || results[1] || [];
    } catch (err) {
      MK.flashError("加载 image / profile 列表失败：" + err.message);
      return;
    }

    images = images.slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });
    profiles = profiles.slice().sort(function (a, b) {
      return String(a.name || "").localeCompare(String(b.name || ""));
    });

    const curImageID = (binding && binding.image_id) || "";
    const curProfileID = (binding && binding.profile_id) || "";
    const curStatic = (binding && binding.static_address) || "";
    const curHostname = (binding && binding.hostname_override) || "";

    const imgOpts = ['<option value="">— 选择镜像 —</option>'].concat(
      images.map(function (i) {
        const lbl = (i.name || i.id) +
          (i.version ? " " + i.version : "") +
          (i.family ? " (" + i.family + ")" : "");
        const sel = i.id === curImageID ? " selected" : "";
        return '<option value="' + MK.escapeHTML(i.id) + '"' + sel + ">" +
          MK.escapeHTML(lbl) + "</option>";
      })
    ).join("");

    const profOpts = ['<option value="">— 选择配置 —</option>'].concat(
      profiles.map(function (p) {
        const lbl = (p.name || p.id) +
          (p.os_family && p.os_family !== "any" ? " [" + p.os_family + "]" : "");
        const sel = p.id === curProfileID ? " selected" : "";
        return '<option value="' + MK.escapeHTML(p.id) + '"' + sel + ">" +
          MK.escapeHTML(lbl) + "</option>";
      })
    ).join("");

    const imgFamilyByID = {};
    images.forEach(function (i) { imgFamilyByID[i.id] = (i.family || "").toLowerCase(); });
    const profFamilyByID = {};
    profiles.forEach(function (p) { profFamilyByID[p.id] = (p.os_family || "any").toLowerCase(); });

    const body =
      '<div class="form-error error-banner" id="form-error" hidden></div>' +
      '<p class="muted">对 <strong>' + MK.escapeHTML(label) + '</strong>' +
      ' (<code>' + MK.escapeHTML(muuid.slice(0, 8)) + '</code>)' +
      ' 触发重装。Orchestrator 下一 tick 会通过 BMC 把目标机 PXE 重启进 live —— ' +
      '盘上现有数据会被覆盖。</p>' +
      '<label for="re-image">镜像<select id="re-image" required>' + imgOpts + '</select></label>' +
      '<label for="re-profile">配置<select id="re-profile" required>' + profOpts + '</select></label>' +
      '<div id="re-compat" class="muted" hidden></div>' +
      '<label for="re-static">静态地址 (可选 CIDR)<input id="re-static" type="text" value="' +
        MK.escapeHTML(curStatic) + '" placeholder="例如 192.168.10.160/24" autocomplete="off"></label>' +
      '<label for="re-hostname">主机名覆盖 (可选)<input id="re-hostname" type="text" value="' +
        MK.escapeHTML(curHostname) + '" placeholder="覆盖 profile 模板" autocomplete="off"></label>';

    const footer =
      '<button type="button" class="btn btn-ghost" data-modal-close>取消</button> ' +
      '<button type="submit" id="re-submit" class="btn btn-danger">确认重装</button>';

    const dlg = MK.openModal(MK.modalShell("重装 " + label, body, footer));
    const imgSel = dlg.querySelector("#re-image");
    const profSel = dlg.querySelector("#re-profile");
    const compatEl = dlg.querySelector("#re-compat");
    const errEl = dlg.querySelector("#form-error");

    function refreshCompat() {
      const imgFam = imgFamilyByID[imgSel.value] || "";
      const profFam = profFamilyByID[profSel.value] || "any";
      if (!imgSel.value || !profSel.value) { compatEl.hidden = true; return; }
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
    imgSel.addEventListener("change", refreshCompat);
    profSel.addEventListener("change", refreshCompat);
    refreshCompat();

    const form = dlg.querySelector("form");
    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      const imageID = imgSel.value.trim();
      const profileID = profSel.value.trim();
      if (!imageID || !profileID) {
        errEl.textContent = "镜像与配置必填";
        errEl.hidden = false;
        return;
      }
      const staticAddr = dlg.querySelector("#re-static").value.trim();
      const hostname = dlg.querySelector("#re-hostname").value.trim();
      const submitBtn = dlg.querySelector("#re-submit");
      submitBtn.disabled = true;
      try {
        await MK.apiSend("PUT", "/bindings/" + encodeURIComponent(muuid), {
          image_id: imageID,
          profile_id: profileID,
          desired_state: "reinstall",
          static_address: staticAddr,
          hostname_override: hostname,
        });
        MK.closeModal();
        MK.flashSuccess("已请求重装 “" + label + "” —— orchestrator 将在下一 tick 触发 PXE 重启");
      } catch (err) {
        errEl.textContent = "触发重装失败：" + err.message;
        errEl.hidden = false;
        submitBtn.disabled = false;
      }
    });
  }

  // ---- bulk import via CSV ------------------------------------------
  //
  // CSV columns (header is required): machine_uuid,ip,username,password,interface,port
  //   - machine_uuid: SMBIOS UUID, must match an inventoried machine (UUID_RE)
  //   - ip:           IPv4 dotted-quad
  //   - username:     non-empty
  //   - password:     non-empty (always required for import; we can't reuse
  //                   existing ciphertext when the row is new)
  //   - interface:    one of VALID_IFACES (default lanplus when blank)
  //   - port:         1..65535 (default 623 when blank)
  // Comments (lines starting with #) and blank lines are skipped. Parsing is
  // minimal but quote-aware so passwords with commas survive when wrapped in
  // double quotes ("p,a,s,s"). Each row triggers PUT /bmc/{uuid}; results are
  // shown row-by-row in the modal so partial failures don't lose context.

  function openImportModal() {
    const body =
      '<div class="form-error error-banner" id="form-error" hidden></div>' +
      '<p class="muted">CSV 列：<code>machine_uuid,ip,username,password,interface,port</code>。' +
      '<code>interface</code> 缺省 <code>lanplus</code>；<code>port</code> 缺省 623。' +
      '空行和以 <code>#</code> 开头的行会忽略。带逗号的字段请用双引号包起来。</p>' +
      '<label for="csv-file">选择 CSV 文件' +
      '<input type="file" id="csv-file" accept=".csv,text/csv,text/plain"></label>' +
      '<details><summary class="muted">或直接粘贴内容</summary>' +
      '<textarea id="csv-text" rows="8" placeholder="machine_uuid,ip,username,password,interface,port\n' +
      '4c4c4544-...,192.168.10.254,root,calvin,lanplus,623\n"' +
      ' style="width:100%;font-family:var(--font-mono,monospace);"></textarea></details>' +
      '<div id="csv-results" style="margin-top:1em;"></div>';
    const footer =
      '<button type="button" class="btn" data-modal-close>关闭</button>' +
      '<button type="button" id="csv-run" class="btn btn-primary">开始导入</button>';
    const dlg = MK.openModal(MK.modalShell("批量导入 BMC", body, footer));

    const fileInput = dlg.querySelector("#csv-file");
    const textArea = dlg.querySelector("#csv-text");
    fileInput.addEventListener("change", function () {
      const f = fileInput.files && fileInput.files[0];
      if (!f) return;
      const r = new FileReader();
      r.onload = function () { textArea.value = String(r.result || ""); };
      r.readAsText(f);
    });

    dlg.querySelector("#csv-run").addEventListener("click", function () {
      runImport(dlg).catch(function (err) { showFormError(dlg, err.message); });
    });
  }

  async function runImport(dlg) {
    const raw = dlg.querySelector("#csv-text").value || "";
    const results = dlg.querySelector("#csv-results");
    results.innerHTML = "";
    const rows = parseCSV(raw);
    if (rows.length === 0) {
      throw new Error("没有可导入的行（请先选择文件或粘贴内容）");
    }
    const header = rows.shift();
    const idx = headerIndex(header);
    if (idx.machine_uuid < 0 || idx.ip < 0 || idx.username < 0 || idx.password < 0) {
      throw new Error("CSV 头缺少必需列（至少 machine_uuid/ip/username/password）");
    }

    const btn = dlg.querySelector("#csv-run");
    btn.disabled = true;

    const list = document.createElement("ul");
    list.style.listStyle = "none";
    list.style.padding = "0";
    results.appendChild(list);

    let ok = 0, fail = 0;
    for (let i = 0; i < rows.length; i++) {
      const row = rows[i];
      const li = document.createElement("li");
      li.className = "muted";
      li.textContent = "第 " + (i + 2) + " 行：处理中…";
      list.appendChild(li);

      const muuid = (row[idx.machine_uuid] || "").trim().toLowerCase();
      const ip = (row[idx.ip] || "").trim();
      const user = (row[idx.username] || "").trim();
      const pass = row[idx.password] || ""; // do NOT trim — password may have edge spaces
      const ifaceRaw = idx.interface >= 0 ? (row[idx.interface] || "").trim() : "";
      const portRaw = idx.port >= 0 ? (row[idx.port] || "").trim() : "";
      const iface = ifaceRaw || "lanplus";
      const port = portRaw ? parseInt(portRaw, 10) : 623;

      let err = null;
      if (!UUID_RE.test(muuid)) err = "machine_uuid 非法";
      else if (!IPV4_RE.test(ip)) err = "ip 非 IPv4";
      else if (!user) err = "username 空";
      else if (!pass) err = "password 空";
      else if (VALID_IFACES.indexOf(iface) < 0) err = "interface 非法";
      else if (!(port >= 1 && port <= 65535)) err = "port 超界";

      if (err) {
        li.className = "";
        li.style.color = "var(--color-danger,#c00)";
        li.textContent = "第 " + (i + 2) + " 行：✗ " + err + " — " + muuid.slice(0, 8);
        fail++;
        continue;
      }

      try {
        await MK.apiSend("PUT", "/bmc/" + encodeURIComponent(muuid), {
          ip: ip, port: port, username: user, password: pass, ipmi_interface: iface,
        });
        li.className = "";
        li.style.color = "var(--color-ok,#0a0)";
        li.textContent = "第 " + (i + 2) + " 行：✓ " + muuid.slice(0, 8) + " @ " + ip;
        ok++;
      } catch (e) {
        li.className = "";
        li.style.color = "var(--color-danger,#c00)";
        li.textContent = "第 " + (i + 2) + " 行：✗ " + muuid.slice(0, 8) + " — " + e.message;
        fail++;
      }
    }

    btn.disabled = false;
    const summary = document.createElement("p");
    summary.innerHTML = "<strong>导入完成：</strong>成功 " + ok + " 条，失败 " + fail + " 条。";
    results.appendChild(summary);
    if (ok > 0) {
      // Refresh the underlying list so successful rows show up immediately;
      // modal stays open so the user can see per-row results.
      reload();
    }
  }

  // headerIndex maps the well-known column names to their column index in the
  // CSV header, normalised to lowercase + trimmed. Unknown columns are ignored.
  function headerIndex(header) {
    const out = { machine_uuid: -1, ip: -1, username: -1, password: -1, interface: -1, port: -1 };
    for (let i = 0; i < header.length; i++) {
      const k = String(header[i] || "").trim().toLowerCase();
      if (k in out) out[k] = i;
    }
    return out;
  }

  // parseCSV is a minimal RFC 4180–ish parser: supports double-quoted fields
  // with embedded commas and doubled quotes ("" → "). Stops parsing a logical
  // row at the first unquoted newline. Lines starting with # (after trim) and
  // empty lines are dropped. Returns array-of-arrays of strings.
  function parseCSV(input) {
    const out = [];
    let row = [];
    let field = "";
    let inQuotes = false;
    let i = 0;
    const n = input.length;
    while (i < n) {
      const ch = input[i];
      if (inQuotes) {
        if (ch === '"') {
          if (i + 1 < n && input[i + 1] === '"') { field += '"'; i += 2; continue; }
          inQuotes = false; i++; continue;
        }
        field += ch; i++; continue;
      }
      if (ch === '"') { inQuotes = true; i++; continue; }
      if (ch === ",") { row.push(field); field = ""; i++; continue; }
      if (ch === "\r") { i++; continue; }
      if (ch === "\n") {
        row.push(field); field = "";
        flushRow(out, row); row = []; i++; continue;
      }
      field += ch; i++;
    }
    if (field.length > 0 || row.length > 0) {
      row.push(field);
      flushRow(out, row);
    }
    return out;
  }

  function flushRow(out, row) {
    // Drop comments and fully-empty lines.
    const first = (row[0] || "").trim();
    if (row.length === 1 && first === "") return;
    if (first.startsWith("#")) return;
    out.push(row);
  }
})();
