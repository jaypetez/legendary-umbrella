const form = document.getElementById("enroll-form");
const userCode = form.elements["user_code"];
const name = form.elements["name"];
const lookup = document.getElementById("lookup-result");
const result = document.getElementById("approve-result");

const params = new URLSearchParams(location.search);
if (params.get("user_code")) {
  userCode.value = params.get("user_code").toUpperCase();
}

async function doLookup() {
  const code = userCode.value.trim().toUpperCase();
  if (code.length < 8) { lookup.textContent = ""; return; }
  try {
    const res = await fetch(`/api/enroll/lookup?user_code=${encodeURIComponent(code)}`);
    if (!res.ok) {
      lookup.textContent = res.status === 404 ? "No pending enrollment with that code." : `Lookup failed (HTTP ${res.status}).`;
      return;
    }
    const info = await res.json();
    if (info.claimed) {
      lookup.textContent = "Already claimed.";
    } else if (info.approved) {
      lookup.textContent = "Already approved — waiting for device to collect its token.";
    } else {
      lookup.textContent = `Pending device: ${info.hostname || "(unknown host)"} (${info.platform || "?"})`;
      if (!name.value && info.hostname) name.value = info.hostname;
    }
  } catch (err) {
    lookup.textContent = `Lookup failed: ${err.message}`;
  }
}

userCode.addEventListener("input", () => {
  userCode.value = userCode.value.toUpperCase();
  doLookup();
});
if (userCode.value) doLookup();

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  result.textContent = "";
  try {
    const res = await fetch("/api/enroll/approve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        user_code: userCode.value.trim().toUpperCase(),
        name: name.value.trim(),
      }),
    });
    if (res.status === 204) {
      result.className = "success";
      result.textContent = "Approved. The device should come online shortly.";
      setTimeout(() => { location.href = "/"; }, 1200);
      return;
    }
    const txt = await res.text();
    result.className = "error";
    result.textContent = txt || `HTTP ${res.status}`;
  } catch (err) {
    result.className = "error";
    result.textContent = err.message;
  }
});
