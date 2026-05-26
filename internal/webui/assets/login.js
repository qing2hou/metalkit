(function () {
  "use strict";

  const params = new URLSearchParams(window.location.search);
  const next = params.get("next") || "/ui/";

  const form = document.getElementById("login-form");
  const userInput = document.getElementById("login-user");
  const passInput = document.getElementById("login-pass");
  const submitBtn = form.querySelector(".login-submit");
  const banner = document.getElementById("error-banner");

  function showError(msg) {
    banner.textContent = msg;
    banner.hidden = false;
  }

  function clearError() {
    banner.textContent = "";
    banner.hidden = true;
  }

  function flashLoggedOut() {
    const div = document.createElement("div");
    div.className = "flash";
    div.textContent = "已退出登录";
    document.body.appendChild(div);
    setTimeout(function () {
      div.style.transition = "opacity 0.3s ease";
      div.style.opacity = "0";
      setTimeout(function () { div.remove(); }, 300);
    }, 2500);
  }

  if (params.get("logout") === "1") {
    flashLoggedOut();
  }

  form.addEventListener("submit", async function (ev) {
    ev.preventDefault();
    clearError();
    const username = userInput.value;
    const password = passInput.value;
    submitBtn.disabled = true;
    const originalText = submitBtn.textContent;
    submitBtn.textContent = "登录中…";
    try {
      const resp = await fetch("/api/v1/auth/login", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "Accept": "application/json",
        },
        body: JSON.stringify({ username: username, password: password }),
      });
      if (resp.ok) {
        window.location.href = next;
        return;
      }
      let msg = "登录失败 (HTTP " + resp.status + ")";
      try {
        const j = await resp.json();
        if (j && j.error) msg = j.error;
      } catch (_) { /* tolerate */ }
      showError(msg);
      passInput.value = "";
      userInput.focus();
      userInput.select();
    } catch (e) {
      showError("网络错误：" + e.message);
    } finally {
      submitBtn.disabled = false;
      submitBtn.textContent = originalText;
    }
  });
})();
