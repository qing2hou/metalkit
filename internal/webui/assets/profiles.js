// metalkit operator UI — profiles page.
//
// Lists install profiles; opens a modal for create/edit/delete. Hashes the
// root password via POST /api/v1/util/crypt-sha512 before submit so the
// plaintext never lives in the profile payload.
//
// Schema reference: internal/profiles/store.go (Profile, CreateInput,
// UpdateInput) and internal/profiles/validate.go (TargetDisk, NetworkConfig).

(function () {
  "use strict";

  const PROFILE_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
  const MAC_RE = /^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$/;
  const IFNAME_MAX = 15;

  // Module-level subnet cache, populated on page load and refresh. The form
  // and the list both render synchronously from this so the dropdown / column
  // can show subnet names rather than opaque IDs.
  let subnetCache = [];

  // Module-level component cache, keyed by os_family. Populated lazily on
  // first OS-family change in the form. Avoids repeated API calls.
  let componentsCache = {};

  document.addEventListener("DOMContentLoaded", function () {
    document.getElementById("refresh-btn").addEventListener("click", function () {
      loadSubnets();
      loadProfiles();
    });
    document.getElementById("new-profile-btn").addEventListener("click", function () {
      openProfileModal(null);
    });
    loadSubnets();
    loadProfiles();
  });

  async function loadSubnets() {
    try {
      const list = await MK.apiGet("/subnets");
      subnetCache = Array.isArray(list) ? list : [];
      subnetCache.sort(function (a, b) {
        return String(a.name || "").localeCompare(String(b.name || ""));
      });
    } catch (err) {
      // Non-fatal: profile form will just show an empty subnet dropdown.
      subnetCache = [];
    }
  }

  // loadComponents fetches the available renderers/bootloaders for a given
  // OS family from the API. Results are cached client-side.
  async function loadComponents(osFamily) {
    var key = osFamily || "any";
    if (componentsCache[key]) return componentsCache[key];
    try {
      var cs = await MK.apiGet("/profiles/components?os_family=" + encodeURIComponent(key));
      componentsCache[key] = cs;
      return cs;
    } catch (err) {
      return null;
    }
  }

  // populateComponentDropdowns fills the network-renderer and bootloader
  // selects with options from the component set, preserving the current
  // selection if still valid.
  function populateComponentDropdowns(cs, currentRenderer, currentBootloader) {
    var nr = document.getElementById("f-net-renderer");
    var bl = document.getElementById("f-bootloader");
    if (!nr || !bl) return;

    // Network renderer dropdown.
    var nrVal = currentRenderer || "";
    var nrHTML = '<option value=""' + (nrVal===""?" selected":"") + '>自动（按 OS 家族默认）</option>';
    if (cs && cs.renderers) {
      for (var i = 0; i < cs.renderers.length; i++) {
        var r = cs.renderers[i];
        var sel = (nrVal === r.id) ? " selected" : "";
        nrHTML += '<option value="' + MK.escapeHTML(r.id) + '"' + sel + '>' +
          MK.escapeHTML(r.label) + '</option>';
      }
    }
    nr.innerHTML = nrHTML;

    // Bootloader dropdown.
    var blVal = currentBootloader || "";
    var blHTML = '<option value=""' + (blVal===""?" selected":"") + '>自动（按 OS 家族默认）</option>';
    if (cs && cs.bootloaders) {
      for (var j = 0; j < cs.bootloaders.length; j++) {
        var b = cs.bootloaders[j];
        var bsel = (blVal === b.id) ? " selected" : "";
        blHTML += '<option value="' + MK.escapeHTML(b.id) + '"' + bsel + '>' +
          MK.escapeHTML(b.label) + '</option>';
      }
    }
    bl.innerHTML = blHTML;

    // Update hints.
    var nrHint = document.getElementById("f-net-renderer-hint");
    var blHint = document.getElementById("f-bootloader-hint");
    if (nrHint) {
      if (cs && cs.renderers && cs.renderers.length > 0) {
        nrHint.textContent = "默认：" + cs.renderers[0].label;
      } else {
        nrHint.textContent = "留空则根据 OS 家族自动选择。";
      }
    }
    if (blHint) {
      if (cs && cs.bootloaders && cs.bootloaders.length > 0) {
        blHint.textContent = "默认：" + cs.bootloaders[0].label;
      } else {
        blHint.textContent = "留空则根据 OS 家族自动选择。";
      }
    }
  }

  function subnetByID(id) {
    if (!id) return null;
    for (const s of subnetCache) {
      if (s.id === id) return s;
    }
    return null;
  }

  function describeSubnet(id) {
    if (!id) return '<span class="muted">-</span>';
    const s = subnetByID(id);
    if (!s) return '<span class="muted mono" title="未找到 subnet">' + MK.escapeHTML(id.slice(0, 8)) + '</span>';
    const tail = s.cidr ? ' <span class="muted">' + MK.escapeHTML(s.cidr) + '</span>' : '';
    return MK.escapeHTML(s.name) + tail;
  }

  // ---- list -----------------------------------------------------------

  async function loadProfiles() {
    MK.clearError();
    const loading = document.getElementById("profiles-loading");
    const empty = document.getElementById("profiles-empty");
    const wrap = document.getElementById("profiles-wrap");
    loading.hidden = false;
    empty.hidden = true;
    wrap.hidden = true;
    try {
      const list = await MK.apiGet("/profiles");
      renderProfiles(list || []);
    } catch (err) {
      MK.flashError("加载配置失败：" + err.message);
      loading.hidden = true;
    }
  }

  function renderProfiles(list) {
    const loading = document.getElementById("profiles-loading");
    const empty = document.getElementById("profiles-empty");
    const wrap = document.getElementById("profiles-wrap");
    const count = document.getElementById("profiles-count");
    loading.hidden = true;
    count.textContent = "共 " + list.length + " 个配置";
    if (list.length === 0) {
      empty.hidden = false;
      return;
    }
    wrap.hidden = false;
    const tbody = document.getElementById("profiles-tbody");
    tbody.innerHTML = "";
    for (const p of list) {
      const tr = document.createElement("tr");
      tr.innerHTML =
        '<td><strong>' + MK.escapeHTML(p.name) + '</strong>' +
        '<div class="muted mono">' + MK.escapeHTML(p.id) + '</div></td>' +
        '<td>' + MK.escapeHTML(p.description || "") + '</td>' +
        '<td><span class="badge">' + MK.escapeHTML(p.os_family || "any") + '</span>' +
          (p.network_renderer ? ' <span class="badge muted">' + MK.escapeHTML(p.network_renderer) + '</span>' : '') +
          (p.bootloader ? ' <span class="badge muted">' + MK.escapeHTML(p.bootloader) + '</span>' : '') +
          '</td>' +
        '<td class="mono">' + MK.escapeHTML(p.hostname_template) + '</td>' +
        '<td>' + describeNetwork(p.network) + '</td>' +
        '<td>' + describeSubnet(p.subnet_id) + '</td>' +
        '<td>' + describeTargetDisk(p.target_disk) + '</td>' +
        '<td title="' + MK.escapeHTML(p.updated_at || "") + '">' +
          MK.fmtISO(p.updated_at) + '</td>' +
        '<td class="col-actions">' +
          '<button type="button" class="btn edit-btn" data-id="' + MK.escapeHTML(p.id) + '">编辑</button> ' +
          '<button type="button" class="btn delete-btn" data-id="' + MK.escapeHTML(p.id) + '" data-name="' + MK.escapeHTML(p.name) + '">删除</button>' +
        '</td>';
      tbody.appendChild(tr);
    }
    tbody.querySelectorAll(".edit-btn").forEach(function (b) {
      b.addEventListener("click", function () { editProfile(b.dataset.id); });
    });
    tbody.querySelectorAll(".delete-btn").forEach(function (b) {
      b.addEventListener("click", function () { deleteProfile(b.dataset.id, b.dataset.name); });
    });
  }

  function describeNetwork(nc) {
    if (!nc) return "-";
    let head;
    if (nc.method === "static") {
      const parts = ["静态 /" + (nc.prefix_len || "?")];
      if (nc.gateway) parts.push("网关=" + nc.gateway);
      if (nc.dns && nc.dns.length) parts.push("DNS=" + nc.dns.join(","));
      head = MK.escapeHTML(parts.join(" "));
    } else {
      head = "DHCP";
    }
    let tail;
    if (nc.bond) {
      tail = '<div class="muted">Bond ' + MK.escapeHTML(nc.bond.mode) +
        '（' + (nc.bond.slaves || []).length + ' slaves）</div>';
    } else {
      tail = '<div class="muted">网卡：' + MK.escapeHTML(nc.nic_selector || "auto") + '</div>';
    }
    return head + tail;
  }

  function describeTargetDisk(td) {
    if (!td) return "-";
    if (td.mode === "smallest") return "最小盘";
    return MK.escapeHTML(td.mode) + ': <span class="mono">' + MK.escapeHTML(td.value || "") + '</span>';
  }

  // ---- create / edit --------------------------------------------------

  async function editProfile(id) {
    MK.clearError();
    try {
      const p = await MK.apiGet("/profiles/" + encodeURIComponent(id));
      openProfileModal(p);
    } catch (err) {
      MK.flashError("获取失败：" + err.message);
    }
  }

  function openProfileModal(existing) {
    const isEdit = !!existing;
    const title = isEdit ? ("编辑配置：" + existing.name) : "新建配置";
    const body = renderForm(existing);
    const footer =
      '<button type="button" class="btn" data-modal-close>取消</button>' +
      '<button type="button" id="profile-save" class="btn btn-primary">' +
      (isEdit ? "保存" : "创建") + '</button>';
    const dlg = MK.openModal(MK.modalShell(title, body, footer));

    wireFormBehaviour(dlg, existing);

    dlg.querySelector("#profile-save").addEventListener("click", function () {
      submitForm(dlg, existing).catch(function (err) {
        showFormError(dlg, err.message);
      });
    });
  }

  function renderForm(existing) {
    const td = (existing && existing.target_disk) || { mode: "smallest", value: "" };
    const nc = (existing && existing.network) || { method: "dhcp", nic_selector: "auto" };
    const nicKind = nicKindOf(nc.nic_selector);
    const nicValue = nicValueOf(nc.nic_selector);
    const escapedName = existing ? MK.escapeHTML(existing.name) : "";
    const bond = nc.bond || null;
    const bondEnabled = !!bond;
    const bondMode = (bond && bond.mode) || "active-backup";
    const bondSlaves = (bond && bond.slaves) || [];
    const bondMiimon = (bond && bond.miimon) || 100;
    const bondPrimary = (bond && bond.primary) || "";
    const bondLACPRate = (bond && bond.lacp_rate) || "fast";
    const bondXmit = (bond && bond.xmit_hash_policy) || "layer3+4";

    return (
      '<div class="form-error" id="form-error" hidden></div>' +

      (existing
        ? '<div class="form-row"><label>名称</label>' +
          '<input type="text" value="' + escapedName + '" disabled>' +
          '<div class="muted">名称不可变。如需重命名请删除后重建。</div></div>'
        : '<div class="form-row"><label for="f-name">名称 <span class="required">*</span></label>' +
          '<input type="text" id="f-name" required maxlength="64" placeholder="ubuntu-default" autocomplete="off">' +
          '<div class="muted">字母、数字、点、横线、下划线，必须以字母或数字开头。</div></div>') +

      '<div class="form-row"><label for="f-desc">描述</label>' +
      '<textarea id="f-desc" rows="2" maxlength="1024">' +
      (existing ? MK.escapeHTML(existing.description || "") : "") + '</textarea></div>' +

      '<div class="form-row"><label for="f-hostname">主机名模板 <span class="required">*</span></label>' +
      '<input type="text" id="f-hostname" required maxlength="253" placeholder="node-{serial}" value="' +
      (existing ? MK.escapeHTML(existing.hostname_template) : "") + '">' +
      '<div class="muted">支持占位符：<code>{serial}</code>、<code>{uuid8}</code>、<code>{mac}</code>。</div></div>' +

      '<div class="form-row"><label for="f-osfamily">OS 家族</label>' +
      '<select id="f-osfamily">' +
        (function(f){ f = (existing && existing.os_family) ? existing.os_family : "any"; return [
          '<option value="any"'    + (f==="any"   ?" selected":"") + '>any（不限制）</option>',
          '<option value="ubuntu"' + (f==="ubuntu"?" selected":"") + '>ubuntu</option>',
          '<option value="debian"' + (f==="debian"?" selected":"") + '>debian</option>',
          '<option value="rhel"'   + (f==="rhel"  ?" selected":"") + '>rhel（Rocky 8/9、AlmaLinux、RHEL 8+）</option>',
          '<option value="rhel7"'  + (f==="rhel7" ?" selected":"") + '>rhel7（CentOS 7 / RHEL 7）</option>',
          '<option value="kylin"'     + (f==="kylin"    ?" selected":"") + '>kylin（银河麒麟）</option>',
          '<option value="openeuler"' + (f==="openeuler"?" selected":"") + '>openeuler（openEuler）</option>',
          '<option value="opensuse"'  + (f==="opensuse" ?" selected":"") + '>opensuse（openSUSE）</option>',
        ].join(""); })() +
      '</select>' +
      '<div class="muted">绑定时镜像的 family 必须与此匹配（any 不做限制）。</div></div>' +

      '<div class="form-row"><label for="f-net-renderer">网络渲染器</label>' +
      '<select id="f-net-renderer">' +
        (function(){ var r = (existing && existing.network_renderer) || ""; return [
          '<option value=""' + (r===""?" selected":"") + '>自动（按 OS 家族默认）</option>',
        ].join(""); })() +
      '</select>' +
      '<div class="muted" id="f-net-renderer-hint">留空则根据 OS 家族自动选择。</div></div>' +

      '<div class="form-row"><label for="f-bootloader">引导加载程序</label>' +
      '<select id="f-bootloader">' +
        (function(){ var b = (existing && existing.bootloader) || ""; return [
          '<option value=""' + (b===""?" selected":"") + '>自动（按 OS 家族默认）</option>',
        ].join(""); })() +
      '</select>' +
      '<div class="muted" id="f-bootloader-hint">留空则根据 OS 家族自动选择。</div></div>' +

      '<div class="form-row"><label for="f-chroot-dns">Chroot DNS</label>' +
      '<input type="text" id="f-chroot-dns" value="' + ((existing && existing.chroot_dns && existing.chroot_dns.join) ? MK.escapeHTML(existing.chroot_dns.join(", ")) : (existing && existing.chroot_dns ? MK.escapeHTML(existing.chroot_dns) : "")) + '" placeholder="223.5.5.5, 114.114.114.114">' +
      '<div class="muted">装机时写入目标 rootfs 的 /etc/resolv.conf，供 chroot 内 dnf 等命令解析镜像源。逗号或空格分隔的 IP。留空则使用默认（223.5.5.5 / 114.114.114.114）。</div></div>' +

      '<div class="form-row"><label for="f-subnet">默认子网</label>' +
      '<select id="f-subnet">' +
        (function () {
          const cur = (existing && existing.subnet_id) || "";
          let opts = '<option value=""' + (cur === "" ? " selected" : "") + '>— 不指定 —</option>';
          for (const s of subnetCache) {
            const sel = s.id === cur ? " selected" : "";
            const label = s.name + (s.cidr ? "  (" + s.cidr + ")" : "");
            opts += '<option value="' + MK.escapeHTML(s.id) + '"' + sel + '>' + MK.escapeHTML(label) + '</option>';
          }
          return opts;
        })() +
      '</select>' +
      '<div class="muted" id="f-subnet-hint"></div></div>' +

      '<div class="form-row"><label for="f-pass">Root 密码</label>' +
      '<div class="inline-row">' +
      '<input type="password" id="f-pass" autocomplete="new-password" placeholder="' +
      (existing ? "留空保持不变" : "留空使用默认（metalkit）") + '">' +
      '<button type="button" id="f-pass-hash" class="btn">计算 Hash</button>' +
      '<span id="f-pass-status" class="muted"></span>' +
      '</div>' +
      '<input type="hidden" id="f-pass-hash-val" value="">' +
      '<div class="muted">' +
      (existing
        ? '服务端使用 SHA-512 crypt 哈希，明文不会保存。留空保持原密码。'
        : '服务端使用 SHA-512 crypt 哈希，明文不会保存。留空则使用集群默认密码 <code>metalkit</code>（请装机后尽快修改）。') +
      '</div></div>' +

      '<fieldset class="form-fieldset"><legend>目标盘</legend>' +
      '<div class="form-row"><label for="f-td-mode">模式</label>' +
      '<select id="f-td-mode">' +
        '<option value="smallest"' + (td.mode === "smallest" ? " selected" : "") + '>smallest（最小盘）</option>' +
        '<option value="by-path"' + (td.mode === "by-path" ? " selected" : "") + '>by-path</option>' +
        '<option value="by-wwn"' + (td.mode === "by-wwn" ? " selected" : "") + '>by-wwn</option>' +
        '<option value="by-model"' + (td.mode === "by-model" ? " selected" : "") + '>by-model</option>' +
      '</select></div>' +
      '<div class="form-row" id="f-td-value-row"' +
        (td.mode === "smallest" ? ' hidden' : '') + '>' +
      '<label for="f-td-value">值</label>' +
      '<div class="inline-row">' +
      '<input type="text" id="f-td-value" value="' + MK.escapeHTML(td.value || "") + '" placeholder="例：0x5000c500abc / Samsung SSD 870 EVO 250GB">' +
      '<button type="button" id="f-td-pick" class="btn"' +
        (td.mode === "by-path" ? " disabled" : "") +
        ' title="从已纳管机器选目标盘">从机器选盘</button>' +
      '</div>' +
      '<div class="muted" id="f-td-help"></div>' +
      '</div>' +
      '</fieldset>' +

      '<fieldset class="form-fieldset"><legend>网络</legend>' +
      '<div class="form-row"><label for="f-net-method">方法</label>' +
      '<select id="f-net-method">' +
        '<option value="dhcp"' + (nc.method === "dhcp" ? " selected" : "") + '>DHCP</option>' +
        '<option value="static"' + (nc.method === "static" ? " selected" : "") + '>静态</option>' +
      '</select></div>' +

      '<div id="f-net-static" ' + (nc.method === "static" ? '' : 'hidden') + '>' +
        '<div class="form-row"><label for="f-net-prefix">前缀长度</label>' +
        '<input type="number" id="f-net-prefix" min="1" max="32" value="' + (nc.prefix_len || 24) + '"></div>' +
        '<div class="form-row"><label for="f-net-gw">网关（IPv4）</label>' +
        '<input type="text" id="f-net-gw" value="' + MK.escapeHTML(nc.gateway || "") + '" placeholder="10.0.0.1"></div>' +
        '<div class="form-row"><label for="f-net-dns">DNS（IPv4，逗号或空格分隔）</label>' +
        '<input type="text" id="f-net-dns" value="' + MK.escapeHTML((nc.dns || []).join(", ")) + '" placeholder="8.8.8.8, 1.1.1.1"></div>' +
      '</div>' +

      '<div class="form-row" id="f-vlan-row"><label for="f-vlan">VLAN ID（可选）</label>' +
      '<input type="number" id="f-vlan" min="1" max="4094" value="' + (nc.vlan || 0) + '" placeholder="留空表示不启用 VLAN">' +
      '<div class="muted">802.1Q VLAN tag（1–4094）。启用后 IP 配置会上移到 VLAN 子接口。' +
      '选中"默认子网"后此字段自动隐藏：VLAN 由 subnet.vlan_id 提供。</div></div>' +

      '<div class="form-row"><label for="f-nic-kind">网卡选择</label>' +
      '<div class="inline-row">' +
      '<select id="f-nic-kind"' + (bondEnabled ? " disabled" : "") + '>' +
        '<option value="auto"' + (nicKind === "auto" ? " selected" : "") + '>auto（自动）</option>' +
        '<option value="by-mac"' + (nicKind === "by-mac" ? " selected" : "") + '>by-mac</option>' +
        '<option value="by-name"' + (nicKind === "by-name" ? " selected" : "") + '>by-name</option>' +
      '</select>' +
      '<input type="text" id="f-nic-value" value="' + MK.escapeHTML(nicValue) + '" placeholder="aa:bb:cc:dd:ee:ff 或 eth0"' +
        ((nicKind === "auto" || bondEnabled) ? " disabled" : "") + '>' +
      '<button type="button" id="f-nic-pick" class="btn"' +
        ((nicKind !== "by-mac" || bondEnabled) ? " disabled" : "") +
        ' title="从已纳管机器选 MAC">从机器选 MAC</button>' +
      '</div>' +
      '<div class="muted" id="f-nic-bond-note"' + (bondEnabled ? '' : ' hidden') +
      '>启用 Bond 后，单网卡选择被忽略，物理网卡由 Bond slaves（接口名）决定。</div>' +
      '</div>' +

      '<div class="form-row"><label><input type="checkbox" id="f-bond-enable"' +
      (bondEnabled ? " checked" : "") + '> 启用网卡 Bond</label>' +
      '<div class="muted">802.3ad（LACP）需要上联交换机配 port-channel，metalkit 不管。</div></div>' +

      '<div id="f-bond-fields"' + (bondEnabled ? '' : ' hidden') + '>' +

      '<div class="form-row"><label for="f-bond-mode">模式</label>' +
      '<select id="f-bond-mode">' +
        '<option value="active-backup"' + (bondMode === "active-backup" ? " selected" : "") + '>active-backup（单边即可）</option>' +
        '<option value="802.3ad"' + (bondMode === "802.3ad" ? " selected" : "") + '>802.3ad（LACP）</option>' +
      '</select></div>' +

      '<div class="form-row"><label for="f-bond-slaves">Slaves（每行一个接口名，至少 2 个）<span class="required">*</span></label>' +
      '<textarea id="f-bond-slaves" rows="4" placeholder="eno1&#10;eno2">' +
      MK.escapeHTML(bondSlaves.join("\n")) + '</textarea>' +
      '<div><button type="button" id="f-bond-pick" class="btn">从机器选网卡（可多选）</button></div></div>' +

      '<div class="form-row"><label for="f-bond-miimon">Miimon (ms)</label>' +
      '<input type="number" id="f-bond-miimon" min="50" max="10000" value="' + bondMiimon + '"></div>' +

      '<div class="form-row" id="f-bond-primary-row"' + (bondMode === "active-backup" ? '' : ' hidden') + '>' +
      '<label for="f-bond-primary">主网卡（可选，需在 slaves 列表中）</label>' +
      '<input type="text" id="f-bond-primary" value="' + MK.escapeHTML(bondPrimary) + '" placeholder="eno1"></div>' +

      '<div class="form-row" id="f-bond-lacp-row"' + (bondMode === "802.3ad" ? '' : ' hidden') + '>' +
      '<label for="f-bond-lacp">LACP rate</label>' +
      '<select id="f-bond-lacp">' +
        '<option value="fast"' + (bondLACPRate === "fast" ? " selected" : "") + '>fast（每秒 LACPDU）</option>' +
        '<option value="slow"' + (bondLACPRate === "slow" ? " selected" : "") + '>slow（每 30 秒）</option>' +
      '</select></div>' +

      '<div class="form-row" id="f-bond-xmit-row"' + (bondMode === "802.3ad" ? '' : ' hidden') + '>' +
      '<label for="f-bond-xmit">Transmit hash policy</label>' +
      '<select id="f-bond-xmit">' +
        '<option value="layer2"' + (bondXmit === "layer2" ? " selected" : "") + '>layer2</option>' +
        '<option value="layer2+3"' + (bondXmit === "layer2+3" ? " selected" : "") + '>layer2+3</option>' +
        '<option value="layer3+4"' + (bondXmit === "layer3+4" ? " selected" : "") + '>layer3+4（推荐）</option>' +
      '</select></div>' +

      '</div>' +

      '</fieldset>'
    );
  }

  function nicKindOf(sel) {
    if (!sel || sel === "auto") return "auto";
    if (sel.indexOf("by-mac:") === 0) return "by-mac";
    if (sel.indexOf("by-name:") === 0) return "by-name";
    return "auto";
  }
  function nicValueOf(sel) {
    if (!sel || sel === "auto") return "";
    if (sel.indexOf("by-mac:") === 0) return sel.slice("by-mac:".length);
    if (sel.indexOf("by-name:") === 0) return sel.slice("by-name:".length);
    return "";
  }

  function wireFormBehaviour(dlg, existing) {
    // --- OS family change → reload components ---
    var osFamilySel = dlg.querySelector("#f-osfamily");
    var curRenderer = (existing && existing.network_renderer) || "";
    var curBootloader = (existing && existing.bootloader) || "";
    async function onOSFamilyChange() {
      var fam = osFamilySel ? osFamilySel.value : "any";
      var cs = await loadComponents(fam);
      // Read current selection before repopulating.
      var nrSel = dlg.querySelector("#f-net-renderer");
      var blSel = dlg.querySelector("#f-bootloader");
      var nrVal = nrSel ? nrSel.value : curRenderer;
      var blVal = blSel ? blSel.value : curBootloader;
      populateComponentDropdowns(cs, nrVal, blVal);
    }
    if (osFamilySel) {
      osFamilySel.addEventListener("change", onOSFamilyChange);
    }
    // Initial load of components for the current OS family.
    onOSFamilyChange();

    const tdMode = dlg.querySelector("#f-td-mode");
    const tdValueRow = dlg.querySelector("#f-td-value-row");
    const tdValue = dlg.querySelector("#f-td-value");
    const tdPick = dlg.querySelector("#f-td-pick");
    const tdHelp = dlg.querySelector("#f-td-help");
    function refreshTDHelp() {
      const m = tdMode.value;
      if (m === "by-path") {
        tdHelp.innerHTML = 'by-path 标识符（如 <code>pci-0000:01:00.0-scsi-0:0:0:0</code>）不在硬件清单里。请登录目标机器执行 <code>ls -l /dev/disk/by-path/</code> 自行获取。';
        if (tdPick) tdPick.disabled = true;
      } else if (m === "by-wwn") {
        tdHelp.textContent = '建议从已纳管机器选盘，自动填入 WWN（如 0x5000c500abc）。';
        if (tdPick) tdPick.disabled = false;
      } else if (m === "by-model") {
        tdHelp.textContent = '型号字符串需与 lsblk MODEL 字段精确匹配。建议从机器选盘自动填入。';
        if (tdPick) tdPick.disabled = false;
      } else {
        tdHelp.textContent = '';
        if (tdPick) tdPick.disabled = true;
      }
    }
    tdMode.addEventListener("change", function () {
      tdValueRow.hidden = (tdMode.value === "smallest");
      refreshTDHelp();
    });
    refreshTDHelp();

    if (tdPick) {
      tdPick.addEventListener("click", function () {
        openDiskPicker({
          mode: tdMode.value,
          onPick: function (val) {
            if (val) tdValue.value = val;
          },
        });
      });
    }

    const netMethod = dlg.querySelector("#f-net-method");
    const netStatic = dlg.querySelector("#f-net-static");
    const subnetSel = dlg.querySelector("#f-subnet");
    const subnetHint = dlg.querySelector("#f-subnet-hint");
    const vlanRow = dlg.querySelector("#f-vlan-row");

    // 静态区可见性 = method=static 且 没选 subnet。选了 subnet 时这些字段
    // （prefix / gateway / DNS）由 subnet 提供，重复输入只会带来不一致。
    function refreshStaticVisibility() {
      const subnetSelected = !!subnetSel.value;
      netStatic.hidden = (netMethod.value !== "static") || subnetSelected;
    }
    netMethod.addEventListener("change", refreshStaticVisibility);

    function refreshSubnetHint() {
      const s = subnetByID(subnetSel.value);
      if (!s) {
        subnetHint.textContent = "选填。指定后，使用该 profile 的 binding 默认从此子网取 CIDR / 网关 / DNS / VLAN；binding 仍可单独覆盖。";
        if (vlanRow) vlanRow.hidden = false;
        refreshStaticVisibility();
        return;
      }
      const parts = [];
      if (s.cidr) parts.push("CIDR " + s.cidr);
      if (s.gateway) parts.push("网关 " + s.gateway);
      if (s.dns && s.dns.length) parts.push("DNS " + s.dns.join(", "));
      parts.push("VLAN " + (s.vlan_id || "无"));
      subnetHint.textContent = parts.join(" · ");
      // 选中 subnet 时 VLAN/CIDR/网关/DNS 都由 subnet 提供，隐藏 profile 自己
      // 那几行；submit 时同步清零，避免落库 stale 值。
      if (vlanRow) vlanRow.hidden = true;
      refreshStaticVisibility();
    }
    subnetSel.addEventListener("change", refreshSubnetHint);
    refreshSubnetHint();

    const nicKind = dlg.querySelector("#f-nic-kind");
    const nicValue = dlg.querySelector("#f-nic-value");
    const nicPick = dlg.querySelector("#f-nic-pick");
    nicKind.addEventListener("change", function () {
      if (nicKind.value === "auto") {
        nicValue.value = "";
        nicValue.disabled = true;
      } else {
        nicValue.disabled = false;
      }
      // 选 MAC 按钮仅 by-mac 时启用 —— by-name 时硬件报告里的 MAC 帮不上忙。
      if (nicPick) nicPick.disabled = (nicKind.value !== "by-mac") || dlg.querySelector("#f-bond-enable").checked;
    });

    if (nicPick) {
      nicPick.addEventListener("click", function () {
        openNICPicker({ multi: false, onPick: function (macs) {
          if (!macs || macs.length === 0) return;
          nicValue.value = macs[0];
          // 选完后默认切到 by-mac（user UX：从「从机器选 MAC」点进来意图就是 by-mac）。
          if (nicKind.value !== "by-mac") {
            nicKind.value = "by-mac";
            nicValue.disabled = false;
          }
        }});
      });
    }

    const bondEnable = dlg.querySelector("#f-bond-enable");
    const bondFields = dlg.querySelector("#f-bond-fields");
    const bondNote = dlg.querySelector("#f-nic-bond-note");
    bondEnable.addEventListener("change", function () {
      const on = bondEnable.checked;
      bondFields.hidden = !on;
      bondNote.hidden = !on;
      // When bond is on, single-NIC selector is disabled (validator will
      // force nic_selector=auto anyway, but show the user we mean it).
      nicKind.disabled = on;
      nicValue.disabled = on || (nicKind.value === "auto");
      if (nicPick) nicPick.disabled = on || (nicKind.value !== "by-mac");
    });

    const bondPick = dlg.querySelector("#f-bond-pick");
    const bondSlavesTA = dlg.querySelector("#f-bond-slaves");
    if (bondPick) {
      bondPick.addEventListener("click", function () {
        openNICPicker({ multi: true, pickBy: "name", onPick: function (names) {
          if (!names || names.length === 0) return;
          // 合并：保留已填的接口名，添加新选的，去重，按出现顺序保留。
          const have = (bondSlavesTA.value || "")
            .split(/\r?\n/).map(function (x) { return x.trim(); })
            .filter(Boolean);
          const seen = {};
          const merged = [];
          for (const n of have.concat(names)) {
            if (n && !seen[n]) { seen[n] = true; merged.push(n); }
          }
          bondSlavesTA.value = merged.join("\n");
        }});
      });
    }

    const bondMode = dlg.querySelector("#f-bond-mode");
    const bondPrimaryRow = dlg.querySelector("#f-bond-primary-row");
    const bondLacpRow = dlg.querySelector("#f-bond-lacp-row");
    const bondXmitRow = dlg.querySelector("#f-bond-xmit-row");
    bondMode.addEventListener("change", function () {
      const ab = bondMode.value === "active-backup";
      bondPrimaryRow.hidden = !ab;
      bondLacpRow.hidden = ab;
      bondXmitRow.hidden = ab;
    });

    const passInput = dlg.querySelector("#f-pass");
    const passBtn = dlg.querySelector("#f-pass-hash");
    const passStatus = dlg.querySelector("#f-pass-status");
    const passHidden = dlg.querySelector("#f-pass-hash-val");
    passBtn.addEventListener("click", async function () {
      const pw = passInput.value;
      if (!pw) {
        passStatus.textContent = "请先输入密码";
        passStatus.className = "muted danger";
        return;
      }
      passBtn.disabled = true;
      passStatus.textContent = "计算中…";
      passStatus.className = "muted";
      try {
        const r = await MK.apiSend("POST", "/util/crypt-sha512", { password: pw });
        passHidden.value = r.hash;
        passStatus.textContent = "已生成 Hash ✓";
        passStatus.className = "ok";
        // Don't keep the plaintext lying around in the input.
        passInput.value = "";
      } catch (err) {
        passStatus.textContent = err.message;
        passStatus.className = "muted danger";
      } finally {
        passBtn.disabled = false;
      }
    });

    // If the user types a new password but doesn't hit Hash, clear stale hash
    // so we never submit a hash that doesn't match the typed password.
    passInput.addEventListener("input", function () {
      if (passInput.value) {
        passHidden.value = "";
        passStatus.textContent = "请点击「计算 Hash」";
        passStatus.className = "muted";
      }
    });

    // Edit mode: existing profile already has a stored hash on the server.
    // Empty hidden field means "don't change root_password_hash on save".
    if (existing) {
      passStatus.textContent = "服务端已有 Hash —— 留空保持不变";
      passStatus.className = "muted";
    }
  }

  function showFormError(dlg, msg) {
    const e = dlg.querySelector("#form-error");
    if (!e) return;
    e.textContent = msg;
    e.hidden = false;
    e.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }

  async function submitForm(dlg, existing) {
    const isEdit = !!existing;

    // Gather fields.
    const name = isEdit ? existing.name : dlg.querySelector("#f-name").value.trim();
    const description = dlg.querySelector("#f-desc").value.trim();
    const hostname = dlg.querySelector("#f-hostname").value.trim();
    const tdMode = dlg.querySelector("#f-td-mode").value;
    const tdValue = (dlg.querySelector("#f-td-value").value || "").trim();
    const netMethod = dlg.querySelector("#f-net-method").value;
    const prefixLen = parseInt(dlg.querySelector("#f-net-prefix").value, 10);
    const gw = (dlg.querySelector("#f-net-gw").value || "").trim();
    const dnsRaw = (dlg.querySelector("#f-net-dns").value || "").trim();
    // 选中默认子网时，VLAN 由 subnet.vlan_id 决定；profile 自带的 VLAN 字段
    // 已隐藏，这里强制置 0 避免持久化 stale 值。
    const subnetSelected = !!(dlg.querySelector("#f-subnet") && dlg.querySelector("#f-subnet").value);
    const vlan = subnetSelected ? 0 : (parseInt((dlg.querySelector("#f-vlan").value || "0"), 10) || 0);
    const nicKind = dlg.querySelector("#f-nic-kind").value;
    const nicValue = (dlg.querySelector("#f-nic-value").value || "").trim();
    const bondEnabled = dlg.querySelector("#f-bond-enable").checked;
    const bondMode = dlg.querySelector("#f-bond-mode").value;
    const bondSlavesRaw = dlg.querySelector("#f-bond-slaves").value || "";
    const bondMiimon = parseInt(dlg.querySelector("#f-bond-miimon").value, 10);
    const bondPrimary = (dlg.querySelector("#f-bond-primary").value || "").trim();
    const bondLACP = dlg.querySelector("#f-bond-lacp").value;
    const bondXmit = dlg.querySelector("#f-bond-xmit").value;
    const passHash = dlg.querySelector("#f-pass-hash-val").value;
    const passInput = dlg.querySelector("#f-pass").value;
    const osFamily = (dlg.querySelector("#f-osfamily") && dlg.querySelector("#f-osfamily").value) || "any";
    const subnetID = (dlg.querySelector("#f-subnet") && dlg.querySelector("#f-subnet").value) || "";
    const networkRenderer = (dlg.querySelector("#f-net-renderer") && dlg.querySelector("#f-net-renderer").value) || "";
    const bootloader = (dlg.querySelector("#f-bootloader") && dlg.querySelector("#f-bootloader").value) || "";
    const chrootDNSRaw = (dlg.querySelector("#f-chroot-dns") && dlg.querySelector("#f-chroot-dns").value) || "";
    const chrootDNS = chrootDNSRaw.trim();
    // chroot_dns is sent as a single comma/space-separated string; the
    // server validates and stores it. Empty string = use installer
    // defaults. Always send the field on create (so it's stored as ""),
    // and on edit only when the user touched it (non-empty string OR
    // explicitly cleared). We use null on edit to mean "unchanged".
    const chrootDNSPayload = isEdit ? chrootDNS : chrootDNS;

    // Client-side validation.
    if (!isEdit && !PROFILE_NAME_RE.test(name)) {
      throw new Error("名称：1-64 字符 [A-Za-z0-9._-]，必须以字母或数字开头");
    }
    if (!hostname) {
      throw new Error("主机名模板必填");
    }
    if (tdMode !== "smallest" && !tdValue) {
      throw new Error("mode=" + tdMode + " 时 target_disk.value 必填");
    }

    if (passInput && !passHash) {
      throw new Error("已输入密码但未点击「计算 Hash」");
    }
    // 新建时允许留空：后端会用集群默认密码（metalkit）补齐。

    // Build network.
    let network;
    let nicSelector = "auto";
    if (!bondEnabled) {
      if (nicKind === "by-mac") {
        if (!MAC_RE.test(nicValue)) throw new Error("nic_selector by-mac：MAC 格式错误");
        nicSelector = "by-mac:" + nicValue.toLowerCase();
      } else if (nicKind === "by-name") {
        if (!nicValue || nicValue.length > IFNAME_MAX || /[ \t\n/]/.test(nicValue)) {
          throw new Error("nic_selector by-name：接口名错误");
        }
        nicSelector = "by-name:" + nicValue;
      }
    }
    if (netMethod === "static") {
      if (subnetSelected) {
        // subnet 提供 prefix_len / gateway / DNS / VLAN —— 不填、不校验、不发。
        network = {
          method: "static",
          nic_selector: nicSelector,
        };
      } else {
        if (!(prefixLen >= 1 && prefixLen <= 32)) throw new Error("prefix_len 必须在 1..32");
        if (!gw) throw new Error("static 必须填网关");
        const dnsList = dnsRaw.split(/[\s,]+/).filter(Boolean);
        network = {
          method: "static",
          prefix_len: prefixLen,
          gateway: gw,
          dns: dnsList,
          nic_selector: nicSelector,
          vlan: vlan || undefined,
        };
      }
    } else {
      network = { method: "dhcp", nic_selector: nicSelector, vlan: vlan || undefined };
    }

    if (bondEnabled) {
      const slaves = bondSlavesRaw.split(/[\s,]+/).filter(Boolean);
      if (slaves.length < 2) throw new Error("Bond slaves 至少 2 个网卡");
      if (slaves.length > 8) throw new Error("Bond slaves 最多 8 个网卡");
      const seen = {};
      for (const m of slaves) {
        if (!m || m.length > IFNAME_MAX || /[ \t\n/]/.test(m)) throw new Error("Bond slaves 包含非法接口名：" + m);
        if (seen[m]) throw new Error("Bond slaves 包含重复接口名：" + m);
        seen[m] = true;
      }
      if (!(bondMiimon >= 50 && bondMiimon <= 10000)) {
        throw new Error("Bond miimon 必须在 50..10000");
      }
      const bond = {
        mode: bondMode,
        slaves: slaves,
        miimon: bondMiimon,
      };
      if (bondMode === "active-backup") {
        if (bondPrimary) {
          if (!bondPrimary || bondPrimary.length > IFNAME_MAX || /[ \t\n/]/.test(bondPrimary)) throw new Error("Bond primary 格式错误");
          if (!seen[bondPrimary]) throw new Error("Bond primary 必须在 slaves 中");
          bond.primary = bondPrimary;
        }
      } else {
        bond.lacp_rate = bondLACP;
        bond.xmit_hash_policy = bondXmit;
      }
      network.bond = bond;
    }

    // Build target_disk.
    const target_disk = tdMode === "smallest"
      ? { mode: "smallest" }
      : { mode: tdMode, value: tdValue };

    // Submit.
    try {
      if (isEdit) {
        const body = {
          description: description,
          hostname_template: hostname,
          target_disk: target_disk,
          network: network,
          os_family: osFamily,
          subnet_id: subnetID,
          network_renderer: networkRenderer,
          bootloader: bootloader,
          chroot_dns: chrootDNSPayload,
        };
        if (passHash) body.root_password_hash = passHash;
        await MK.apiSend("PUT", "/profiles/" + encodeURIComponent(existing.id), body);
      } else {
        const body = {
          name: name,
          description: description,
          hostname_template: hostname,
          target_disk: target_disk,
          network: network,
          os_family: osFamily,
          subnet_id: subnetID,
          network_renderer: networkRenderer,
          bootloader: bootloader,
          chroot_dns: chrootDNSPayload,
        };
        // 留空 → 不带字段，后端补集群默认密码 hash。
        if (passHash) body.root_password_hash = passHash;
        await MK.apiSend("POST", "/profiles", body);
      }
      MK.closeModal();
      MK.flashSuccess("配置已保存");
      loadProfiles();
    } catch (err) {
      throw err;
    }
  }

  // ---- delete ---------------------------------------------------------

  async function deleteProfile(id, name) {
    if (!confirm('删除配置「' + name + '」？被 binding 引用的配置无法删除（外键约束）。')) {
      return;
    }
    try {
      await MK.apiSend("DELETE", "/profiles/" + encodeURIComponent(id));
      MK.flashSuccess("配置已删除");
      loadProfiles();
    } catch (err) {
      MK.flashError("删除失败：" + err.message);
    }
  }

  // openNICPicker shows a two-step modal: pick a machine → pick one or more
  // NICs. opts:
  //   multi:  true → multi-select via checkboxes; false → single-select via radio
  //   pickBy: "mac" (default, for by-mac selector) or "name" (for bond slaves)
  //   onPick: function(values[])  invoked when the user clicks 确定
  //
  // Profile is a shared template — the picked values only fill the form fields;
  // the chosen machine is *not* persisted on the profile. This is purely UX
  // assistance so the operator doesn't have to type identifiers by hand.
  function openNICPicker(opts) {
    opts = opts || {};
    const pickBy = opts.pickBy || "mac";
    const title = pickBy === "name"
      ? (opts.multi ? "选网卡（可多选）" : "选网卡（单选）")
      : (opts.multi ? "选 MAC（可多选）" : "选 MAC（单选）");
    const body =
      '<div class="form-error error-banner" id="picker-error" hidden></div>' +
      '<div class="form-row"><label for="picker-machine">机器</label>' +
      '<select id="picker-machine"><option value="">— 选择机器 —</option></select>' +
      '<div class="muted">仅已上报硬件清单的机器有 NIC 数据。</div></div>' +
      '<div id="picker-nics" class="muted">请先选机器。</div>';
    const footer =
      '<button type="button" class="btn" data-modal-close>取消</button>' +
      '<button type="button" id="picker-confirm" class="btn btn-primary" disabled>确定</button>';
    const dlg = MK.openModal(MK.modalShell(title, body, footer));

    // 拉机器列表填下拉。
    MK.apiGet("/machines").then(function (machines) {
      const list = Array.isArray(machines) ? machines : (machines && machines.machines) || [];
      const sel = dlg.querySelector("#picker-machine");
      list.sort(function (a, b) {
        return String(a.product || a.uuid || "").localeCompare(String(b.product || b.uuid || ""));
      });
      for (const m of list) {
        const opt = document.createElement("option");
        opt.value = m.uuid;
        opt.textContent = (m.product || "未知型号") + " — " + String(m.uuid).slice(0, 8);
        sel.appendChild(opt);
      }
      sel.addEventListener("change", function () { loadNICsForMachine(dlg, sel.value, opts); });
    }).catch(function (err) {
      const pe = dlg.querySelector("#picker-error");
      pe.textContent = "加载机器列表失败：" + err.message;
      pe.hidden = false;
      pe.scrollIntoView({ block: "nearest", behavior: "smooth" });
    });

    dlg.querySelector("#picker-confirm").addEventListener("click", function () {
      const checks = dlg.querySelectorAll('input[name="picker-nic"]:checked');
      const macs = [];
      checks.forEach(function (el) { if (el.value) macs.push(el.value); });
      if (macs.length === 0) return;
      MK.closeModal();
      if (typeof opts.onPick === "function") opts.onPick(macs);
    });
  }

  async function loadNICsForMachine(dlg, uuid, opts) {
    const errEl = dlg.querySelector("#picker-error");
    const container = dlg.querySelector("#picker-nics");
    const confirmBtn = dlg.querySelector("#picker-confirm");
    errEl.hidden = true;
    confirmBtn.disabled = true;
    if (!uuid) {
      container.textContent = "请先选机器。";
      return;
    }
    container.textContent = "加载中…";
    let rep = null;
    try {
      rep = await MK.apiGet("/machines/" + encodeURIComponent(uuid));
    } catch (err) {
      container.textContent = "";
      errEl.textContent = "拉取硬件清单失败：" + err.message;
      errEl.hidden = false;
      errEl.scrollIntoView({ block: "nearest", behavior: "smooth" });
      return;
    }
    const nics = (rep && Array.isArray(rep.nics)) ? rep.nics : [];
    if (nics.length === 0) {
      container.textContent = "该机器没有 NIC 数据。";
      return;
    }
    // 排序：先按 link 状态（up 优先），再按 PCI 地址 / 名称，方便操作员一眼挑出业务网卡。
    nics.sort(function (a, b) {
      if (!!b.link !== !!a.link) return b.link ? 1 : -1;
      return String(a.name || "").localeCompare(String(b.name || ""));
    });

    container.innerHTML = "";
    const table = document.createElement("table");
    table.className = "data-table";
    const thead = document.createElement("thead");
    thead.innerHTML = '<tr><th></th><th>名称</th><th>MAC</th><th>型号 / 驱动</th><th>Link</th><th>速度</th></tr>';
    table.appendChild(thead);
    const tbody = document.createElement("tbody");
    table.appendChild(tbody);
    for (const n of nics) {
      const mac = String(n.mac || "").toLowerCase();
      if (!/^[0-9a-f]{2}(:[0-9a-f]{2}){5}$/.test(mac)) continue;  // skip garbage / virtual
      const tr = document.createElement("tr");
      const inputType = opts.multi ? "checkbox" : "radio";
      const speed = n.speed_mbps ? (n.speed_mbps >= 1000 ? (n.speed_mbps / 1000) + " Gbps" : n.speed_mbps + " Mbps") : "-";
      const model = [n.driver, n.firmware_version].filter(Boolean).join(" ");
      const pickVal = (opts && opts.pickBy) === "name" ? (n.name || "") : mac;
      tr.innerHTML =
        '<td><input type="' + inputType + '" name="picker-nic" value="' + MK.escapeHTML(pickVal) + '"></td>' +
        '<td class="mono">' + MK.escapeHTML(n.name || "?") + '</td>' +
        '<td class="mono">' + MK.escapeHTML(mac) + '</td>' +
        '<td>' + MK.escapeHTML(model || "-") + '</td>' +
        '<td>' + (n.link ? '<span class="badge badge-ok">up</span>' : '<span class="badge">down</span>') + '</td>' +
        '<td>' + MK.escapeHTML(speed) + '</td>';
      tbody.appendChild(tr);
    }
    container.appendChild(table);
    // 任何 NIC 勾选后启用确定按钮。
    container.querySelectorAll('input[name="picker-nic"]').forEach(function (el) {
      el.addEventListener("change", function () {
        confirmBtn.disabled = !container.querySelector('input[name="picker-nic"]:checked');
      });
    });
  }
  // openDiskPicker shows a two-step modal: pick a machine → pick one disk;
  // fills the target_disk.value field by the chosen mode (by-wwn → wwn,
  // by-model → model). by-path is not pickable because inventory does not
  // capture /dev/disk/by-path/* symlinks. opts:
  //   mode:   "by-wwn" | "by-model"
  //   onPick: function(value)
  function openDiskPicker(opts) {
    opts = opts || {};
    const mode = opts.mode || "by-wwn";
    const body =
      '<div class="form-error error-banner" id="dpicker-error" hidden></div>' +
      '<div class="form-row"><label for="dpicker-machine">机器</label>' +
      '<select id="dpicker-machine"><option value="">— 选择机器 —</option></select>' +
      '<div class="muted">仅已上报硬件清单的机器有磁盘数据。已过滤可移动盘 / 分区 / loop。</div></div>' +
      '<div id="dpicker-disks" class="muted">请先选机器。</div>';
    const footer =
      '<button type="button" class="btn" data-modal-close>取消</button>' +
      '<button type="button" id="dpicker-confirm" class="btn btn-primary" disabled>确定</button>';
    const dlg = MK.openModal(MK.modalShell("从机器选盘（mode=" + mode + "）", body, footer));

    MK.apiGet("/machines").then(function (machines) {
      const list = Array.isArray(machines) ? machines : (machines && machines.machines) || [];
      const sel = dlg.querySelector("#dpicker-machine");
      list.sort(function (a, b) {
        return String(a.product || a.uuid || "").localeCompare(String(b.product || b.uuid || ""));
      });
      for (const m of list) {
        const opt = document.createElement("option");
        opt.value = m.uuid;
        opt.textContent = (m.product || "未知型号") + " — " + String(m.uuid).slice(0, 8);
        sel.appendChild(opt);
      }
      sel.addEventListener("change", function () { loadDisksForMachine(dlg, sel.value, mode); });
    }).catch(function (err) {
      const pe = dlg.querySelector("#dpicker-error");
      pe.textContent = "加载机器列表失败：" + err.message;
      pe.hidden = false;
      pe.scrollIntoView({ block: "nearest", behavior: "smooth" });
    });

    dlg.querySelector("#dpicker-confirm").addEventListener("click", function () {
      const checked = dlg.querySelector('input[name="dpicker-disk"]:checked');
      if (!checked || !checked.value) return;
      MK.closeModal();
      if (typeof opts.onPick === "function") opts.onPick(checked.value);
    });
  }

  function fmtDiskSize(bytes) {
    if (!bytes || bytes <= 0) return "-";
    const gb = bytes / (1024 * 1024 * 1024);
    if (gb >= 1024) return (gb / 1024).toFixed(1) + " TiB";
    if (gb >= 100) return gb.toFixed(0) + " GiB";
    return gb.toFixed(1) + " GiB";
  }

  async function loadDisksForMachine(dlg, uuid, mode) {
    const errEl = dlg.querySelector("#dpicker-error");
    const container = dlg.querySelector("#dpicker-disks");
    const confirmBtn = dlg.querySelector("#dpicker-confirm");
    errEl.hidden = true;
    confirmBtn.disabled = true;
    if (!uuid) { container.textContent = "请先选机器。"; return; }
    container.textContent = "加载中…";
    let rep = null;
    try {
      rep = await MK.apiGet("/machines/" + encodeURIComponent(uuid));
    } catch (err) {
      container.textContent = "";
      errEl.textContent = "拉取硬件清单失败：" + err.message;
      errEl.hidden = false;
      errEl.scrollIntoView({ block: "nearest", behavior: "smooth" });
      return;
    }
    const all = (rep && Array.isArray(rep.disks)) ? rep.disks : [];
    // 与装机 picker 口径一致：只显示 type=disk 且非 removable 的物理盘。
    const disks = all.filter(function (d) { return d && d.type === "disk" && !d.removable; });
    if (disks.length === 0) {
      container.textContent = "该机器没有可用磁盘数据。";
      return;
    }
    // 按 size 升序，习惯上系统盘是最小的那一块。
    disks.sort(function (a, b) { return (a.size_bytes || 0) - (b.size_bytes || 0); });

    container.innerHTML = "";
    const table = document.createElement("table");
    table.className = "data-table";
    const thead = document.createElement("thead");
    thead.innerHTML = '<tr><th></th><th>路径</th><th>容量</th><th>型号</th><th>WWN</th><th>类型</th></tr>';
    table.appendChild(thead);
    const tbody = document.createElement("tbody");
    table.appendChild(tbody);
    for (const d of disks) {
      // 根据 mode 决定行能否选：by-wwn 需要 wwn，by-model 需要 model。
      const wwn = (d.wwn || "").trim();
      const model = (d.model || "").trim();
      let value = "";
      if (mode === "by-wwn") value = wwn;
      else if (mode === "by-model") value = model;
      const disabled = !value;
      const tx = [d.transport, d.rotational ? "HDD" : "SSD"].filter(Boolean).join(" · ").toUpperCase();
      const tr = document.createElement("tr");
      tr.innerHTML =
        '<td><input type="radio" name="dpicker-disk" value="' + MK.escapeHTML(value) + '"' +
          (disabled ? ' disabled title="本盘缺少该 mode 所需字段"' : '') + '></td>' +
        '<td class="mono">' + MK.escapeHTML(d.path || d.kname || "?") + '</td>' +
        '<td>' + MK.escapeHTML(fmtDiskSize(d.size_bytes)) + '</td>' +
        '<td>' + (model ? MK.escapeHTML(model) : '<span class="muted">-</span>') + '</td>' +
        '<td class="mono">' + MK.escapeHTML(wwn || '-') + '</td>' +
        '<td>' + MK.escapeHTML(tx || '-') + '</td>';
      tbody.appendChild(tr);
    }
    container.appendChild(table);
    container.querySelectorAll('input[name="dpicker-disk"]').forEach(function (el) {
      el.addEventListener("change", function () {
        confirmBtn.disabled = !container.querySelector('input[name="dpicker-disk"]:checked');
      });
    });
  }
})();
