package web

const indexHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no">
  <title>tclaude</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/css/xterm.min.css">
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    html, body { height: 100%; background: #000; overflow: hidden; }
    #terminal { height: 100%; width: 100%; }
    #status {
      position: fixed; top: 8px; right: 8px;
      padding: 4px 10px; border-radius: 4px;
      font-family: monospace; font-size: 12px;
      color: #fff; background: rgba(80,80,80,0.8);
      z-index: 10; transition: opacity 0.3s;
    }
    #status.connected { background: rgba(0,120,0,0.8); }
    #status.disconnected { background: rgba(180,0,0,0.8); }
    #extra-keys {
      position: fixed; bottom: 8px; left: 8px; right: 8px;
      display: none; gap: 4px;
      justify-content: center;
      z-index: 11;
    }
    #extra-keys.visible { display: flex; }
    .has-input-bar #extra-keys { bottom: 56px; }
    .extra-key {
      height: 44px; min-width: 44px;
      padding: 0 14px;
      display: flex; align-items: center; justify-content: center;
      border-radius: 6px;
      font-family: monospace; font-size: 14px; font-weight: bold;
      color: #fff; background: rgba(80,80,120,0.85);
      border: 1px solid rgba(255,255,255,0.2);
      cursor: pointer;
      touch-action: manipulation;
      -webkit-tap-highlight-color: transparent;
      user-select: none;
    }
    .extra-key:active { background: rgba(100,100,160,0.95); }
    .extra-key.esc { background: rgba(180,40,40,0.85); }
    .extra-key.esc:active { background: rgba(220,60,60,0.95); }
    #keys-toggle {
      position: fixed; bottom: 8px; right: 8px;
      width: 44px; height: 44px;
      display: flex; align-items: center; justify-content: center;
      border-radius: 6px;
      font-size: 20px;
      color: #fff; background: rgba(80,80,80,0.7);
      border: 1px solid rgba(255,255,255,0.2);
      z-index: 12; cursor: pointer;
      touch-action: manipulation;
      -webkit-tap-highlight-color: transparent;
      user-select: none;
    }
    #keys-toggle:active { background: rgba(100,100,100,0.9); }
    .has-input-bar #keys-toggle { bottom: 56px; }
    #input-bar {
      display: none;
      position: fixed; bottom: 0; left: 0; right: 0;
      gap: 4px; padding: 6px;
      background: rgba(30,30,50,0.95);
      border-top: 1px solid rgba(255,255,255,0.15);
      z-index: 10;
    }
    #mobile-input {
      flex: 1; min-width: 0;
      padding: 8px 12px; border-radius: 6px;
      border: 1px solid rgba(255,255,255,0.2);
      background: rgba(0,0,0,0.5);
      color: #e0e0e0;
      font-family: monospace; font-size: 16px;
      outline: none;
    }
    .input-btn {
      padding: 8px 14px; border-radius: 6px;
      border: 1px solid rgba(255,255,255,0.2);
      background: rgba(80,80,120,0.85);
      color: #fff; font-size: 16px;
      cursor: pointer;
      touch-action: manipulation;
      -webkit-tap-highlight-color: transparent;
      user-select: none;
    }
    .input-btn:active { background: rgba(100,100,160,0.95); }
  </style>
</head>
<body>
  <div id="extra-keys">
    <div class="extra-key esc" data-seq="\x1b">ESC</div>
    <div class="extra-key" data-key="left">&#9664;</div>
    <div class="extra-key" data-key="down">&#9660;</div>
    <div class="extra-key" data-key="up">&#9650;</div>
    <div class="extra-key" data-key="right">&#9654;</div>
  </div>
  <div id="keys-toggle">&#8943;</div>
  <div id="status">connecting...</div>
  <div id="terminal"></div>
  <div id="input-bar">
    <input type="text" id="mobile-input" placeholder="Type here..." autocomplete="off" autocorrect="on" autocapitalize="off" spellcheck="true">
    <button class="input-btn" id="tab-btn">&#8677;</button>
    <button class="input-btn" id="send-btn">&#9166;</button>
  </div>

  <script src="https://cdn.jsdelivr.net/npm/@xterm/xterm@6.0.0/lib/xterm.min.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/@xterm/addon-fit@0.11.0/lib/addon-fit.min.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/@xterm/addon-web-links@0.12.0/lib/addon-web-links.min.js"></script>
  <script>
    const term = new Terminal({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: '"Cascadia Code", "Fira Code", "SF Mono", Menlo, monospace',
      theme: {
        background: '#1a1a2e',
        foreground: '#e0e0e0',
        cursor: '#e0e0e0',
        selectionBackground: 'rgba(255,255,255,0.2)',
      },
      allowProposedApi: true,
    });

    const fitAddon = new FitAddon.FitAddon();
    const webLinksAddon = new WebLinksAddon.WebLinksAddon();
    term.loadAddon(fitAddon);
    term.loadAddon(webLinksAddon);
    term.open(document.getElementById('terminal'));
    fitAddon.fit();

    const status = document.getElementById('status');
    let ws;
    let reconnectTimer;

    function connect() {
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      ws = new WebSocket(proto + '//' + location.host + '/ws');
      ws.binaryType = 'arraybuffer';

      ws.onopen = () => {
        status.textContent = 'connected';
        status.className = 'connected';
        // Fade out status after 2s
        setTimeout(() => { status.style.opacity = '0'; }, 2000);
        // Send initial size
        sendResize();
      };

      ws.onmessage = (e) => {
        if (e.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(e.data));
        } else {
          term.write(e.data);
        }
      };

      ws.onclose = () => {
        status.textContent = 'disconnected - reconnecting...';
        status.className = 'disconnected';
        status.style.opacity = '1';
        reconnectTimer = setTimeout(connect, 2000);
      };

      ws.onerror = () => {
        ws.close();
      };
    }

    function sendResize() {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({
          type: 'resize',
          cols: term.cols,
          rows: term.rows,
        }));
      }
    }

    // Terminal input -> WebSocket
    term.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data));
      }
    });

    // Handle resize
    window.addEventListener('resize', () => {
      fitAddon.fit();
      sendResize();
    });

    // Also send resize when fit changes dimensions
    term.onResize(() => {
      sendResize();
    });

    // Toggle extra keys panel
    const extraKeys = document.getElementById('extra-keys');
    document.getElementById('keys-toggle').addEventListener('click', (e) => {
      e.preventDefault();
      extraKeys.classList.toggle('visible');
    });

    // Extra keys: ESC and arrow buttons
    const keySeqs = {left: '\x1b[D', down: '\x1b[B', up: '\x1b[A', right: '\x1b[C'};
    document.querySelectorAll('.extra-key').forEach((btn) => {
      btn.addEventListener('click', (e) => {
        e.preventDefault();
        const seq = btn.dataset.seq || keySeqs[btn.dataset.key];
        if (seq && ws && ws.readyState === WebSocket.OPEN) {
          ws.send(new TextEncoder().encode(seq));
        }
        // Don't focus terminal on touch devices - avoids opening virtual keyboard
        if (!isTouchDevice) term.focus();
      });
    });

    // Mobile input bar - shown on touch devices for proper IME/autocorrect support
    const isTouchDevice = 'ontouchstart' in window || navigator.maxTouchPoints > 0;
    if (isTouchDevice) {
      const inputBar = document.getElementById('input-bar');
      inputBar.style.display = 'flex';
      document.body.classList.add('has-input-bar');
      // Make room for the input bar
      const barHeight = 52;
      document.getElementById('terminal').style.height = 'calc(100% - ' + barHeight + 'px)';
      fitAddon.fit();
      sendResize();
    }

    const mobileInput = document.getElementById('mobile-input');

    function sendMobileInput() {
      const text = mobileInput.value;
      if (ws && ws.readyState === WebSocket.OPEN) {
        if (text) {
          ws.send(new TextEncoder().encode(text));
        }
        ws.send(new TextEncoder().encode('\r'));
      }
      mobileInput.value = '';
      mobileInput.focus();
    }

    mobileInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        sendMobileInput();
      }
    });

    document.getElementById('send-btn').addEventListener('click', (e) => {
      e.preventDefault();
      sendMobileInput();
    });

    document.getElementById('tab-btn').addEventListener('click', (e) => {
      e.preventDefault();
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode('\t'));
      }
      mobileInput.focus();
    });

    // Two-finger scroll sends mouse wheel escape sequences to tmux
    const termEl = document.getElementById('terminal');
    let twoFingerY = null;
    termEl.addEventListener('touchstart', (e) => {
      if (e.touches.length === 2) {
        twoFingerY = (e.touches[0].clientY + e.touches[1].clientY) / 2;
        e.preventDefault();
      }
    }, { passive: false });
    termEl.addEventListener('touchmove', (e) => {
      if (e.touches.length !== 2 || twoFingerY === null) return;
      e.preventDefault();
      const y = (e.touches[0].clientY + e.touches[1].clientY) / 2;
      const dy = twoFingerY - y;
      if (Math.abs(dy) < 10) return;
      twoFingerY = y;
      // SGR mouse wheel: \x1b[<64;1;1M = scroll up, \x1b[<65;1;1M = scroll down
      const seq = dy > 0 ? '\x1b[<65;1;1M' : '\x1b[<64;1;1M';
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(seq));
      }
    }, { passive: false });
    termEl.addEventListener('touchend', (e) => {
      if (e.touches.length < 2) twoFingerY = null;
    }, { passive: false });

    connect();
  </script>
</body>
</html>`
