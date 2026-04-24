async function loadDevices() {
  const tbody = document.getElementById("device-rows");
  try {
    const res = await fetch("/api/devices");
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const devices = await res.json();
    if (!devices || devices.length === 0) {
      tbody.innerHTML = `<tr><td colspan="5" class="muted">No devices yet. <a href="/enroll.html">Enroll one.</a></td></tr>`;
      return;
    }
    tbody.innerHTML = devices.map((d) => {
      const status = d.online
        ? `<span class="dot online"></span>online`
        : `<span class="dot offline"></span>offline`;
      const last = d.last_seen_at ? new Date(d.last_seen_at * 1000).toLocaleString() : "—";
      const shellHref = `/session?device=${encodeURIComponent(d.id)}&kind=shell`;
      return `<tr>
        <td>${escapeHtml(d.name)}</td>
        <td class="muted">${escapeHtml(d.platform || "—")}</td>
        <td>${status}</td>
        <td class="muted">${last}</td>
        <td>${d.online
          ? `<a class="button" href="${shellHref}">Shell</a>`
          : `<button disabled>Offline</button>`}</td>
      </tr>`;
    }).join("");
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="5" class="error">Failed to load: ${escapeHtml(err.message)}</td></tr>`;
  }
}

function escapeHtml(s) {
  return String(s)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

loadDevices();
setInterval(loadDevices, 3000);
