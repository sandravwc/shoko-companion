// ==UserScript==
// @name         Shoko -> Syncplay
// @namespace    shoko-companion
// @version      0.1
// @description  Floating panel on Shoko WebUI series pages: launch Syncplay+mpv via local shokod
// @match        http://192.168.1.106:8111/*
// @grant        none
// ==/UserScript==
(() => {
  const DAEMON = 'http://127.0.0.1:7373';
  let panel, lastSeries;

  const css = `
    #shokod{position:fixed;right:12px;bottom:12px;z-index:99999;max-height:60vh;overflow:auto;
      background:#1c1c24;color:#eee;font:13px/1.4 sans-serif;border:1px solid #555;border-radius:6px;padding:6px 8px;min-width:260px}
    #shokod h4{margin:0 0 4px;font-size:13px}
    #shokod div{display:flex;gap:6px;align-items:center;padding:1px 0}
    #shokod span{flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
    #shokod .w{opacity:.5}
    #shokod button{cursor:pointer;background:#333;color:#eee;border:1px solid #666;border-radius:3px;padding:0 6px}
    #shokod button:hover{background:#555}`;
  document.head.append(Object.assign(document.createElement('style'), { textContent: css }));

  const launch = (id, mode, btn) =>
    fetch(`${DAEMON}/syncplay?episode=${id}&mode=${mode}`)
      .then(r => r.text()).then(t => (btn.textContent = t.startsWith('launched') ? 'ok' : 'err'))
      .catch(() => (btn.textContent = 'daemon?'));

  async function render(series) {
    panel ??= document.body.appendChild(Object.assign(document.createElement('div'), { id: 'shokod' }));
    panel.innerHTML = '<h4>syncplay</h4>';
    let eps;
    try { eps = await (await fetch(`${DAEMON}/episodes?series=${series}`)).json(); }
    catch { panel.innerHTML += '<div>shokod not running</div>'; return; }
    for (const e of eps) {
      const row = document.createElement('div');
      row.innerHTML = `<span class="${e.Watched ? 'w' : ''}">${String(e.Number).padStart(2, '0')} ${e.Name}</span>`;
      for (const mode of ['ep', 'series']) {
        const b = Object.assign(document.createElement('button'), { textContent: mode });
        b.onclick = () => launch(e.ID, mode, b);
        row.append(b);
      }
      panel.append(row);
    }
  }

  setInterval(() => {
    const m = location.href.match(/\/series\/(\d+)/);
    const series = m?.[1];
    if (series === lastSeries) return;
    lastSeries = series;
    if (series) render(series); else { panel?.remove(); panel = null; }
  }, 500);
})();
