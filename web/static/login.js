// Anmeldung an der GUI. Nach Erfolg setzt der Server ein HttpOnly-Session-Cookie.
"use strict";

const form = document.getElementById("login");
const msg = document.getElementById("msg");

form.addEventListener("submit", async ev => {
  ev.preventDefault();
  const button = form.querySelector("button");
  button.disabled = true;
  msg.hidden = true;
  try {
    const resp = await fetch("api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: form.username.value.trim(), password: form.password.value }),
    });
    if (resp.ok) {
      location.replace("./");
      return;
    }
    let text = (await resp.text()).trim();
    if (resp.status === 503) {
      text = "Es ist noch kein Benutzer angelegt. Auf dem Server ausführen: mat-server user add <name>";
    }
    throw new Error(text || "HTTP " + resp.status);
  } catch (e) {
    msg.textContent = e.message.charAt(0).toUpperCase() + e.message.slice(1);
    msg.hidden = false;
    form.password.value = "";
    form.password.focus();
  } finally {
    button.disabled = false;
  }
});
