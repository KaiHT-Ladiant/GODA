async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || res.statusText);
  }
  return res.json();
}

function $(id) {
  return document.getElementById(id);
}

async function refresh() {
  const s = await api("/api/status");
  $("googleStatus").textContent = s.googleLoggedIn ? "Google: 로그인됨" : "Google: 로그인 필요";
  $("todoStatus").textContent = s.todomateLoggedIn
    ? `Todomate: 로그인됨 (${s.todomateEmail || ""})`
    : "Todomate: 로그인 필요";
  $("dryRun").checked = !!s.dryRun;
  $("interval").value = s.intervalSeconds || 60;
  $("logs").textContent = s.logs || "";
  $("logs").scrollTop = $("logs").scrollHeight;
  const parts = [];
  if (s.busy) parts.push("작업 중");
  if (s.polling) parts.push("폴링 중");
  if (s.dryRun) parts.push("dry-run");
  $("runState").textContent = parts.join(" · ") || "대기";
}

$("btnGoogle").onclick = async () => {
  await api("/api/google/login", { method: "POST", body: "{}" });
  await refresh();
};

$("btnTodo").onclick = () => {
  $("todoDialog").showModal();
};

$("todoForm").addEventListener("close", async () => {
  if ($("todoDialog").returnValue !== "default") return;
  await api("/api/todomate/login", {
    method: "POST",
    body: JSON.stringify({
      email: $("todoEmail").value,
      password: $("todoPassword").value,
    }),
  });
  $("todoPassword").value = "";
  await refresh();
});

$("todoSubmit").addEventListener("click", () => {
  $("todoDialog").returnValue = "default";
});

$("btnOnce").onclick = async () => {
  await saveSettings();
  await api("/api/sync/once", { method: "POST", body: "{}" });
  await refresh();
};

$("btnStart").onclick = async () => {
  await saveSettings();
  await api("/api/poll/start", { method: "POST", body: "{}" });
  await refresh();
};

$("btnStop").onclick = async () => {
  await api("/api/poll/stop", { method: "POST", body: "{}" });
  await refresh();
};

$("btnSaveSettings").onclick = saveSettings;

async function saveSettings() {
  await api("/api/settings", {
    method: "POST",
    body: JSON.stringify({
      dryRun: $("dryRun").checked,
      intervalSeconds: Number($("interval").value || 60),
    }),
  });
}

refresh();
setInterval(refresh, 1500);
