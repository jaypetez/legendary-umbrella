/* global Terminal, FitAddon */

const params = new URLSearchParams(location.search);
const deviceID = params.get("device");
const kind = params.get("kind") || "shell";

const statusEl = document.getElementById("status");
function setStatus(text, cls) {
  statusEl.textContent = text;
  statusEl.className = cls || "";
}

if (!deviceID) {
  setStatus("Missing ?device= parameter", "error");
  throw new Error("no device id");
}

// --- xterm.js ---------------------------------------------------------------

const term = new Terminal({
  fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
  fontSize: 13,
  cursorBlink: true,
  theme: {
    background: "#0a0c0f",
    foreground: "#e6e8eb",
    cursor: "#6ea8ff",
  },
});
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();

// --- WebRTC + signaling -----------------------------------------------------

const wsScheme = location.protocol === "https:" ? "wss" : "ws";
const ws = new WebSocket(`${wsScheme}://${location.host}/ws/session`);
ws.binaryType = "arraybuffer";

let pc = null;
let shellDC = null;
let ctlDC = null;
let sessionID = null;
let terminated = false;

function send(envelope) {
  if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(envelope));
}

function endSession(reason) {
  if (terminated) return;
  terminated = true;
  send({ type: "session.end", session_id: sessionID || undefined, data: { reason } });
  if (pc) try { pc.close(); } catch {}
  try { ws.close(); } catch {}
}

ws.addEventListener("open", () => {
  send({ type: "session.open", data: { device_id: deviceID, kind } });
});

ws.addEventListener("close", () => {
  if (!terminated) setStatus("Signaling disconnected", "error");
});

ws.addEventListener("message", async (ev) => {
  let env;
  try { env = JSON.parse(ev.data); } catch { return; }
  switch (env.type) {
    case "session.ready":
      sessionID = env.session_id;
      setStatus("Negotiating…");
      break;
    case "session.offer":
      sessionID = env.session_id || sessionID;
      if (!pc) createPC();
      await pc.setRemoteDescription({ type: "offer", sdp: env.data.sdp });
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      send({
        type: "session.answer",
        session_id: env.session_id,
        data: { sdp: answer.sdp },
      });
      break;
    case "session.candidate":
      if (!pc) return;
      try {
        await pc.addIceCandidate({
          candidate: env.data.candidate,
          sdpMid: env.data.sdpMid,
          sdpMLineIndex: env.data.sdpMLineIndex,
        });
      } catch (e) { console.warn("addIceCandidate", e); }
      break;
    case "session.end":
      setStatus(`Session ended${env.data?.reason ? `: ${env.data.reason}` : ""}`, "error");
      terminated = true;
      try { pc && pc.close(); } catch {}
      break;
  }
});

async function createPC() {
  pc = new RTCPeerConnection({
    iceServers: [{ urls: "stun:stun.l.google.com:19302" }],
  });
  pc.ondatachannel = (ev) => {
    const dc = ev.channel;
    if (dc.label === "shell") {
      shellDC = dc;
      dc.binaryType = "arraybuffer";
      dc.onopen = () => {
        setStatus("Connected", "connected");
        sendResize();
        term.focus();
      };
      dc.onmessage = (msg) => {
        const data = msg.data instanceof ArrayBuffer
          ? new Uint8Array(msg.data)
          : msg.data;
        term.write(data);
      };
      dc.onclose = () => setStatus("Session closed");
    } else if (dc.label === "shell_ctl") {
      ctlDC = dc;
    }
  };
  pc.onicecandidate = (ev) => {
    if (!ev.candidate || !sessionID) return;
    const c = ev.candidate;
    send({
      type: "session.candidate",
      session_id: sessionID,
      data: {
        candidate: c.candidate,
        sdpMid: c.sdpMid || "",
        sdpMLineIndex: c.sdpMLineIndex ?? 0,
      },
    });
  };
  pc.onconnectionstatechange = () => {
    if (["failed", "disconnected", "closed"].includes(pc.connectionState)) {
      setStatus(`Peer ${pc.connectionState}`, "error");
    }
  };
}

function sendResize() {
  if (!ctlDC || ctlDC.readyState !== "open") return;
  ctlDC.send(JSON.stringify({
    type: "resize",
    rows: term.rows,
    cols: term.cols,
  }));
}

term.onData((data) => {
  if (shellDC && shellDC.readyState === "open") {
    shellDC.send(new TextEncoder().encode(data));
  }
});

const resizeObserver = new ResizeObserver(() => {
  fit.fit();
  sendResize();
});
resizeObserver.observe(document.getElementById("term-wrap"));

window.addEventListener("beforeunload", () => endSession("tab closed"));
