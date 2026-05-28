// metalkit operator UI — subnets page.
//
// Subnets are a small, shared catalogue of network-segment-level attributes:
// CIDR, gateway, DNS, optional VLAN. Bindings will reference subnets so
// operators don't re-type gateway/DNS per machine. Form is intentionally
// simpler than profiles — no NIC selectors, no bond, no target disk.

(function () {
  "use strict";

  const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
  const IPV4_RE = /^(25[0-5]|2[0-4]\d|1\d\d|\d{1,2})(\.(25[0-5]|2[0-4]\d|1\d\d|\d{1,2})){3}$/;
  const CIDR_RE = /^(25[0-5]|2[0-4]\d|1\d\d|\d{1,2})(\.(25[0-5]|2[0-4]\d|1\d\d|\d{1,2})){3}\/(3[0-2]|[12]?\d)$/;

  document.addEventListener("DOMContentLoaded", function () {
    document.getElementById("refresh-btn").addEventListener("click", loadSubnets);
    document.getElementById("new-subnet-btn").addEventListener("click", function () {
      openSubnetModal(null);
    });
    loadSubnets();
  });

  async function loadSubnets() {
    MK.clearError();
    const loading = document.getElementById("subnets-loading");
    const empty = document.getElementById("subnets-empty");
    const wrap = document.getElementById("subnets-wrap");
    loading.hidden = false;
    empty.hidden = true;
    wrap.hidden = true;
    try {
      const list = await MK.apiGet("/subnets");
      renderSubnets(list || []);
    } catch (err) {
      MK.flashError("加载子网失败：" + err.message);
      loading.hidden = true;
    }
  }

  function renderSubnets(list) {
    const loading = document.getElementById("subnets-loading");
    const empty = document.getElementById("subnets-empty");
    const wrap = document.getElementById("subnets-wrap");
    const count = document.getElementById("subnets-count");
    loading.hidden = true;
    count.textContent = "共 " + list.length + " 个子网";
    if (list.length === 0) {
      empty.hidden = false;
      return;
    }
    wrap.hidden = false;
    const tbody = document.getElementById("subnets-tbody");
    tbody.innerHTML = "";
    for (const sn of list) {
      const tr = document.createElement("tr");
      tr.innerHTML =
        '<td><strong>' + MK.escapeHTML(sn.name) + '</strong>' +
        '<div class="muted mono">' + MK.escapeHTML(sn.id) + '</div></td>' +
        '<td class="mono">' + MK.escapeHTML(sn.cidr) + '</td>' +
        '<td class="mono">' + MK.escapeHTML(sn.gateway) + '</td>' +
        '<td class="mono">' + MK.escapeHTML((sn.dns || []).join(", ") || "-") + '</td>' +
        '<td>' + (sn.vlan_id ? '<span class="badge">VLAN ' + sn.vlan_id + '</span>' : '<span class="muted">untagged</span>') + '</td>' +
        '<td>' + MK.escapeHTML(sn.description || "") + '</td>' +
        '<td title="' + MK.escapeHTML(sn.updated_at || "") + '">' +
          MK.fmtISO(sn.updated_at) + '</td>' +
        '<td class="col-actions">' +
          '<button type="button" class="btn edit-btn" data-id="' + MK.escapeHTML(sn.id) + '">编辑</button> ' +
          '<button type="button" class="btn delete-btn" data-id="' + MK.escapeHTML(sn.id) + '" data-name="' + MK.escapeHTML(sn.name) + '">删除</button>' +
        '</td>';
      tbody.appendChild(tr);
    }
    tbody.querySelectorAll(".edit-btn").forEach(function (b) {
      b.addEventListener("click", function () { editSubnet(b.dataset.id); });
    });
    tbody.querySelectorAll(".delete-btn").forEach(function (b) {
      b.addEventListener("click", function () { deleteSubnet(b.dataset.id, b.dataset.name); });
    });
  }

  async function editSubnet(id) {
    MK.clearError();
    try {
      const sn = await MK.apiGet("/subnets/" + encodeURIComponent(id));
      openSubnetModal(sn);
    } catch (err) {
      MK.flashError("获取失败：" + err.message);
    }
  }

  function openSubnetModal(existing) {
    const isEdit = !!existing;
    const title = isEdit ? ("编辑子网：" + existing.name) : "新建子网";
    const body = renderForm(existing);
    const footer =
      '<button type="button" class="btn" data-modal-close>取消</button>' +
      '<button type="button" id="subnet-save" class="btn btn-primary">' +
      (isEdit ? "保存" : "创建") + '</button>';
    const dlg = MK.openModal(MK.modalShell(title, body, footer));
    dlg.querySelector("#subnet-save").addEventListener("click", function () {
      submitForm(dlg, existing).catch(function (err) {
        showFormError(dlg, err.message);
      });
    });
  }

  function renderForm(existing) {
    const escapedName = existing ? MK.escapeHTML(existing.name) : "";
    const dnsStr = existing && existing.dns ? existing.dns.join(", ") : "";
    return (
      '<div class="form-error" id="form-error" hidden></div>' +

      (existing
        ? '<div class="form-row"><label>名称</label>' +
          '<input type="text" value="' + escapedName + '" disabled>' +
          '<div class="muted">名称不可变。如需重命名请删除后重建。</div></div>'
        : '<div class="form-row"><label for="f-name">名称 <span class="required">*</span></label>' +
          '<input type="text" id="f-name" required maxlength="64" placeholder="prod-10-0" autocomplete="off">' +
          '<div class="muted">字母、数字、点、横线、下划线，必须以字母或数字开头。</div></div>') +

      '<div class="form-row"><label for="f-desc">描述</label>' +
      '<textarea id="f-desc" rows="2" maxlength="1024" placeholder="例：生产业务网 10.0.0.0/24">' +
      (existing ? MK.escapeHTML(existing.description || "") : "") + '</textarea></div>' +

      '<div class="form-row"><label for="f-cidr">CIDR <span class="required">*</span></label>' +
      '<input type="text" id="f-cidr" required placeholder="10.0.0.0/24" value="' +
      (existing ? MK.escapeHTML(existing.cidr) : "") + '">' +
      '<div class="muted">IPv4 CIDR，如 <code>192.168.10.0/24</code>。主机位会被自动归零。</div></div>' +

      '<div class="form-row"><label for="f-gw">网关 <span class="required">*</span></label>' +
      '<input type="text" id="f-gw" required placeholder="10.0.0.1" value="' +
      (existing ? MK.escapeHTML(existing.gateway) : "") + '">' +
      '<div class="muted">必须位于 CIDR 内，且不能是网络地址或广播地址。</div></div>' +

      '<div class="form-row"><label for="f-dns">DNS（IPv4，逗号或空格分隔）</label>' +
      '<input type="text" id="f-dns" placeholder="1.1.1.1, 8.8.8.8" value="' +
      MK.escapeHTML(dnsStr) + '">' +
      '<div class="muted">可留空。仅支持 IPv4。</div></div>' +

      '<div class="form-row"><label for="f-vlan">VLAN ID（可选）</label>' +
      '<input type="number" id="f-vlan" min="0" max="4094" value="' +
      ((existing && existing.vlan_id) || 0) + '" placeholder="0 / 留空表示不打标">' +
      '<div class="muted">802.1Q VLAN tag，1–4094。0 表示 untagged。</div></div>'
    );
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
    const name = isEdit ? existing.name : dlg.querySelector("#f-name").value.trim();
    const description = dlg.querySelector("#f-desc").value.trim();
    const cidr = dlg.querySelector("#f-cidr").value.trim();
    const gateway = dlg.querySelector("#f-gw").value.trim();
    const dnsRaw = (dlg.querySelector("#f-dns").value || "").trim();
    const vlan = parseInt((dlg.querySelector("#f-vlan").value || "0"), 10) || 0;

    if (!isEdit && !NAME_RE.test(name)) {
      throw new Error("名称：1-64 字符 [A-Za-z0-9._-]，必须以字母或数字开头");
    }
    if (!CIDR_RE.test(cidr)) {
      throw new Error("CIDR 格式错误，例：192.168.10.0/24");
    }
    if (!IPV4_RE.test(gateway)) {
      throw new Error("网关必须是合法 IPv4 地址");
    }
    const dnsList = dnsRaw.split(/[\s,]+/).filter(Boolean);
    for (const d of dnsList) {
      if (!IPV4_RE.test(d)) throw new Error("DNS 包含非法 IPv4：" + d);
    }
    if (vlan !== 0 && (vlan < 1 || vlan > 4094)) {
      throw new Error("VLAN ID 必须为 0 或 1..4094");
    }

    if (isEdit) {
      const body = {
        description: description,
        cidr: cidr,
        gateway: gateway,
        dns: dnsList,
        vlan_id: vlan,
      };
      await MK.apiSend("PUT", "/subnets/" + encodeURIComponent(existing.id), body);
    } else {
      const body = {
        name: name,
        description: description,
        cidr: cidr,
        gateway: gateway,
        dns: dnsList,
      };
      if (vlan) body.vlan_id = vlan;
      await MK.apiSend("POST", "/subnets", body);
    }
    MK.closeModal();
    MK.flashSuccess("子网已保存");
    loadSubnets();
  }

  async function deleteSubnet(id, name) {
    if (!confirm('删除子网「' + name + '」？被 binding 引用的子网无法删除。')) {
      return;
    }
    try {
      await MK.apiSend("DELETE", "/subnets/" + encodeURIComponent(id));
      MK.flashSuccess("子网已删除");
      loadSubnets();
    } catch (err) {
      MK.flashError("删除失败：" + err.message);
    }
  }
})();
