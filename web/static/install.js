// Kasten „Agent installieren“ unter „Einstellungen“: fertige Befehle mit Server-URL
// und Zertifikats-Fingerabdruck zum Kopieren. Das Token steht bewusst nicht darin,
// die Skripte fragen es ab. Nutzt el() aus app.js und updateStatus aus updates.js.
"use strict";

const INSTALL_REPO = "Daywalker91/Monitoring-Admin-Tool"; // falls der Server kein Repo meldet

// Fingerabdruck lesbar: AA:BB:… in vier Zeilen à 8 Byte (passt auch aufs Handy).
function fmtFingerprint(fp) {
  return (fp.toUpperCase().match(/.{16}/g) || []).map(l => l.match(/../g).join(":")).join("\n");
}

function installCommands(st) {
  const tls = location.protocol === "https:";
  const ws = new URL("ws/agent", location.href);
  ws.protocol = tls ? "wss:" : "ws:";
  ws.search = ws.hash = "";
  const repo = st?.repo && st.repo !== "off" ? st.repo : INSTALL_REPO;
  const dl = "https://github.com/" + repo + "/releases/latest/download/";
  const fp = tls && /^[0-9a-f]{64}$/.test(st?.tls_fingerprint || "") ? st.tls_fingerprint : "";
  return {
    tls, fp,
    win: '$f = "$env:TEMP\\install-agent.ps1"; Invoke-WebRequest -UseBasicParsing ' + dl + "install-agent.ps1 -OutFile $f; " +
      "powershell -ExecutionPolicy Bypass -File $f -Server " + ws.href + (fp ? " -Fingerprint " + fp : ""),
    linux: "curl -fsSLO " + dl + "install-agent.sh && sudo sh install-agent.sh --server " + ws.href +
      (fp ? " --fingerprint " + fp : ""),
  };
}

function renderInstallPanel() {
  const c = installCommands(updateStatus);
  const key = JSON.stringify(c);
  const box = document.getElementById("install-cmds");
  if (box.dataset.key === key) return; // sonst geht beim Auto-Refresh eine Markierung verloren
  box.dataset.key = key;
  box.replaceChildren();
  for (const [title, cmd] of [["Windows (Administrator-PowerShell)", c.win], ["Linux / Raspberry Pi", c.linux]]) {
    const pre = el("pre", cmd, "cmd");
    const btn = el("button", "Kopieren", "secondary");
    btn.type = "button";
    btn.addEventListener("click", () => copyCommand(pre, btn));
    box.append(el("h3", title), pre, btn);
  }
  const info = el("dl", null, "info");
  if (c.fp) {
    info.append(el("dt", "Zertifikat SHA-256"), el("dd", fmtFingerprint(c.fp), "fingerprint"));
  } else {
    info.append(el("dt", "Hinweis"), el("dd", c.tls
      ? "Fingerabdruck unbekannt – server.crt mit -Ca bzw. --ca übergeben."
      : "Ohne TLS (ws://) – nur für Tests im LAN.", "muted"));
  }
  box.append(info);
}

async function copyCommand(pre, btn) {
  try {
    await navigator.clipboard.writeText(pre.textContent);
    btn.textContent = "✓ Kopiert";
  } catch {
    // ohne Clipboard-API (z. B. http): Text markieren, Strg+C bleibt dem Nutzer
    getSelection().selectAllChildren(pre);
    btn.textContent = "Markiert – Strg+C";
  }
  setTimeout(() => { btn.textContent = "Kopieren"; }, 2000);
}
