// metalkit operator UI — settings page.
//
// Reads/writes the DHCP config via /api/v1/settings/dhcp. The endpoint
// overlays SQLite overrides on top of config.yaml defaults, so what the
// form shows is exactly what the controller will use after the next
// restart.
//
// Hot-reload is intentionally not implemented: the embedded DHCP server
// can't swap pools without rebinding UDP/67. After save we show a banner
// reminding the operator to restart the controller.

(function () {
  "use strict";

  const IPV4_RE = /^(25[0-5]|2[0-4]\d|1\d\d|\d{1,2})(\.(25[0-5]|2[0-4]\d|1\d\d|\d{1,2})){3}$/;

  document.addEventListener("DOMContentLoaded", function () {
    document.getElementById("save-btn").addEventListener("click", onSave);
    document.getElementById("reload-btn").addEventListener("click", loadSettings);
    document.getElementById("f-mode").addEventListener("change", refreshPoolFieldsState);
    loadSettings();
  });

  async function loadSettings() {
    hideFormError();
    document.getElementById("restart-banner").hidden = true;
    document.getElementById("applied-banner").hidden = true;
    const loading = document.getElementById("settings-loading");
    const form = document.getElementById("dhcp-form");
    loading.hidden = false;
    form.hidden = true;
    try {
      const s = await MK.apiGet("/settings/dhcp");
      populate(s);
      loading.hidden = true;
      form.hidden = false;
    } catch (err) {
      MK.flashError("加载 DHCP 设置失败：" + err.message);
      loading.hidden = true;
    }
  }

  function populate(s) {
    document.getElementById("f-mode").value = s.mode || "proxy";
    document.getElementById("f-start").value = s.start || "";
    document.getElementById("f-end").value = s.end || "";
    document.getElementById("f-mask").value = s.netmask || "";
    document.getElementById("f-gw").value = s.gateway || "";
    document.getElementById("f-dns").value = (s.dns || []).join(", ");
    document.getElementById("f-lease").value = s.lease_hours || 24;
    document.getElementById("f-exclude").value = (s.exclude || []).join(", ");
    refreshPoolFieldsState();
  }

  // Disable the pool fieldset in proxy mode — the controller ignores those
  // values and the visual cue matches the server-side behaviour.
  function refreshPoolFieldsState() {
    const mode = document.getElementById("f-mode").value;
    document.getElementById("pool-fields").disabled = (mode !== "full");
  }

  async function onSave() {
    hideFormError();
    document.getElementById("restart-banner").hidden = true;
    document.getElementById("applied-banner").hidden = true;
    try {
      const body = collectForm();
      const res = await MK.apiSend("PUT", "/settings/dhcp", body);
      // Server signals whether the hot-reload took. restart_required:false
      // means the running DHCP server already picked up the new pool — no
      // operator action needed. true means the SQLite rows are saved but a
      // controller restart is required to apply them.
      if (res && res.restart_required) {
        MK.flashSuccess("DHCP 设置已保存（需重启）");
        document.getElementById("restart-banner").hidden = false;
      } else {
        MK.flashSuccess("DHCP 设置已生效");
        document.getElementById("applied-banner").hidden = false;
      }
      if (res) populate(res);
      window.scrollTo({ top: 0, behavior: "smooth" });
    } catch (err) {
      showFormError(err.message);
    }
  }

  function collectForm() {
    const mode = document.getElementById("f-mode").value;
    const out = { mode: mode };

    if (mode === "proxy") {
      // Server clears pool fields in proxy mode regardless, but sending
      // them anyway is fine — keeps the wire request stable.
      out.start = "";
      out.end = "";
      out.netmask = "";
      out.gateway = "";
      out.dns = [];
      out.lease_hours = 0;
      out.exclude = [];
      return out;
    }

    const start = document.getElementById("f-start").value.trim();
    const end = document.getElementById("f-end").value.trim();
    const mask = document.getElementById("f-mask").value.trim();
    const gw = document.getElementById("f-gw").value.trim();
    const dnsRaw = (document.getElementById("f-dns").value || "").trim();
    const lease = parseInt(document.getElementById("f-lease").value || "0", 10);
    const excludeRaw = (document.getElementById("f-exclude").value || "").trim();

    if (!IPV4_RE.test(start)) throw new Error("起始 IP 必须是合法 IPv4");
    if (!IPV4_RE.test(end)) throw new Error("结束 IP 必须是合法 IPv4");
    if (!IPV4_RE.test(mask)) throw new Error("子网掩码必须是合法 IPv4 掩码（如 255.255.255.0）");
    if (!IPV4_RE.test(gw)) throw new Error("网关必须是合法 IPv4");
    if (lease <= 0) throw new Error("租约时长必须大于 0");

    const dnsList = dnsRaw.split(/[\s,]+/).filter(Boolean);
    for (const d of dnsList) {
      if (!IPV4_RE.test(d)) throw new Error("DNS 包含非法 IPv4：" + d);
    }
    const excludeList = excludeRaw.split(/[\s,]+/).filter(Boolean);
    for (const e of excludeList) {
      if (!IPV4_RE.test(e)) throw new Error("排除 IP 包含非法 IPv4：" + e);
    }

    out.start = start;
    out.end = end;
    out.netmask = mask;
    out.gateway = gw;
    out.dns = dnsList;
    out.lease_hours = lease;
    out.exclude = excludeList;
    return out;
  }

  function showFormError(msg) {
    const e = document.getElementById("form-error");
    if (!e) return;
    e.textContent = msg;
    e.hidden = false;
    e.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }

  function hideFormError() {
    const e = document.getElementById("form-error");
    if (e) e.hidden = true;
  }
})();
