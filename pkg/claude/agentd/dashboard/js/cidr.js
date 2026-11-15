// cidr.js -- reads a CIDR the way the daemon does (Go's netip.ParsePrefix,
// then Masked), so the sandbox profile editor can tell the operator what a
// row really means before saving it. The daemon keeps every CIDR as its
// network address: 192.168.1.42/24 is stored as 192.168.1.0/24, the network
// it covers, not the one address. No DOM.

function parseIPv4(text) {
  const parts = text.split('.');
  if (parts.length !== 4) return null;
  const out = [];
  for (const part of parts) {
    // netip refuses leading zeros (ambiguous octal) and anything but digits.
    if (!/^(0|[1-9]\d{0,2})$/.test(part)) return null;
    const n = Number(part);
    if (n > 255) return null;
    out.push(n);
  }
  return out;
}

function parseIPv6(text) {
  if (text.includes('%')) return null; // a zone is not allowed in a prefix
  let body = text;
  // An embedded IPv4 tail (::ffff:1.2.3.4) is the last two words.
  const lastColon = body.lastIndexOf(':');
  if (body.slice(lastColon + 1).includes('.')) {
    const v4 = parseIPv4(body.slice(lastColon + 1));
    if (!v4) return null;
    const word = (hi, lo) => ((hi << 8) | lo).toString(16);
    body = `${body.slice(0, lastColon + 1)}${word(v4[0], v4[1])}:${word(v4[2], v4[3])}`;
  }
  const halves = body.split('::');
  if (halves.length > 2) return null;
  const groups = (half) => (half === '' ? [] : half.split(':'));
  const hex = (g) => (/^[0-9a-f]{1,4}$/i.test(g) ? parseInt(g, 16) : null);
  const head = groups(halves[0]).map(hex);
  const rest = halves.length === 2 ? groups(halves[1]).map(hex) : [];
  if ([...head, ...rest].some((w) => w === null)) return null;
  const total = head.length + rest.length;
  if (halves.length === 2 ? total > 7 : total !== 8) return null;
  const all = halves.length === 2 ? [...head, ...new Array(8 - total).fill(0), ...rest] : head;
  return all.flatMap((w) => [w >> 8, w & 0xff]);
}

function formatIPv6(bytes) {
  const words = [];
  for (let i = 0; i < 16; i += 2) words.push((bytes[i] << 8) | bytes[i + 1]);
  // RFC 5952, as netip prints it: the longest run of two or more zero words
  // (the first on a tie) becomes '::'.
  let best = -1; let bestLen = 0;
  for (let i = 0; i < 8;) {
    if (words[i] !== 0) { i += 1; continue; }
    let j = i;
    while (j < 8 && words[j] === 0) j += 1;
    if (j - i > bestLen && j - i >= 2) { best = i; bestLen = j - i; }
    i = j;
  }
  const hex = (ws) => ws.map((w) => w.toString(16)).join(':');
  if (best < 0) return hex(words);
  return `${hex(words.slice(0, best))}::${hex(words.slice(best + bestLen))}`;
}

// cidrNetwork reads one CIDR: null when it is not one, else { network, bits,
// single, hostBits }. network is the prefix the daemon stores, hostBits
// whether the text named an address inside it rather than the network
// itself, and single the prefix length that names one address (32 or 128).
export function cidrNetwork(cidr) {
  const text = String(cidr || '');
  const slash = text.indexOf('/');
  if (slash < 0) return null;
  const addr = text.slice(0, slash);
  const bitsText = text.slice(slash + 1);
  if (!/^(0|[1-9]\d{0,2})$/.test(bitsText)) return null;
  const bits = Number(bitsText);
  const v4 = addr.includes(':') ? null : parseIPv4(addr);
  const bytes = v4 || (addr.includes(':') ? parseIPv6(addr) : null);
  if (!bytes) return null;
  const width = bytes.length * 8;
  if (bits > width) return null;
  const masked = bytes.map((b, i) => {
    const keep = Math.max(0, Math.min(8, bits - i * 8));
    return b & ((0xff << (8 - keep)) & 0xff);
  });
  const hostBits = masked.some((b, i) => b !== bytes[i]);
  // The daemon unmaps an IPv4-mapped prefix (::ffff:a.b.c.d/96+) after
  // masking it and stores the IPv4 network, so name that one.
  const mapped = !v4 && bits >= 96 && masked.slice(0, 10).every((b) => b === 0) &&
    masked[10] === 0xff && masked[11] === 0xff;
  if (mapped) return { network: `${masked.slice(12).join('.')}/${bits - 96}`, bits, single: 128, hostBits };
  const network = `${v4 ? masked.join('.') : formatIPv6(masked)}/${bits}`;
  return { network, bits, single: width, hostBits };
}
