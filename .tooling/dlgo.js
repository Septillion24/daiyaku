// Download Go portable zip using Node's TLS stack (schannel is broken system-wide)
const https = require('https');
const fs = require('fs');
const path = require('path');

const url = process.argv[2];
const out = process.argv[3];

function get(u, redirects = 0) {
  if (redirects > 10) { console.error('too many redirects'); process.exit(1); }
  https.get(u, res => {
    if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
      const next = new URL(res.headers.location, u).toString();
      res.resume();
      return get(next, redirects + 1);
    }
    if (res.statusCode !== 200) { console.error('HTTP', res.statusCode); process.exit(1); }
    const total = parseInt(res.headers['content-length'] || '0', 10);
    let recv = 0, lastPct = -1;
    const f = fs.createWriteStream(out);
    res.on('data', c => {
      recv += c.length;
      if (total) {
        const pct = Math.floor(recv / total * 100);
        if (pct !== lastPct && pct % 10 === 0) { console.log('  ' + pct + '% (' + recv + '/' + total + ')'); lastPct = pct; }
      }
    });
    res.pipe(f);
    f.on('finish', () => f.close(() => console.log('DONE', out, recv, 'bytes')));
  }).on('error', e => { console.error('ERR', e.message); process.exit(1); });
}
get(url);
