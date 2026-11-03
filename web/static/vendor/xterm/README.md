# xterm.js (eingebunden für das Web-Terminal, M11)

Unveränderte Dateien aus den npm-Paketen, lokal eingebunden (kein Node-Build, kein CDN).
Lizenz: MIT (siehe `LICENSE` und `LICENSE.addon-fit`).

| Datei | Paket | Version | SHA-256 |
|-------|-------|---------|---------|
| `xterm.js`, `xterm.css` | `@xterm/xterm` | 5.5.0 | `1f991ac3…f7495` (js), `ba8e6985…af2b6` (css) |
| `addon-fit.js` | `@xterm/addon-fit` | 0.10.0 | `bdaefa37…c97089` |

Die Archive wurden gegen den `integrity`-Wert der npm-Registry geprüft (sha512).

Der Standard-Renderer (DOM) legt eigene `<style>`-Elemente für Zellmaße und Farben an. Die
Content-Security-Policy des Servers erlaubt deshalb `style-src-elem 'self' 'unsafe-inline'`;
Inline-Styles in Attributen und Skripte bleiben verboten. Das Canvas-Addon (0.7.0) wurde
verworfen: Es zeichnete auf HiDPI-Bildschirmen (Handy, Windows-Skalierung) doppelt so große
Zeichen in ein zu kleines Canvas.

Aktualisieren: neue Archive von `https://registry.npmjs.org/<paket>/-/<name>-<version>.tgz`
laden, `dist.integrity` vergleichen, `package/lib/*.js` bzw. `package/css/xterm.css` kopieren,
Tabelle anpassen.
