// The /terminal gate must fire on EVERY hosted apex, not just the first one.
//
// Run: node terminal_hosted_domain.test.js
//
// THE BUG THIS PINS. isHostedHost() suffix-matched ONE hardcoded apex:
//
//     const HOSTED_SUFFIX = '.hive.kubestellar.io';
//
// and the gate ran only `if (isHosted || DASHBOARD_TOKEN)`. A hosted spoke is
// provisioned with HIVE_DASHBOARD_TOKEN deliberately UNSET (identity comes from
// the hub cookie), so on any host the constant does not know — the rebranded
// hive.hivecommons.dev fleet, a future apex, a vanity alias — BOTH branches are
// false and the gate is skipped entirely: an anonymous GET /terminal and an
// anonymous WebSocket upgrade were proxied straight to ttyd. That is an
// unauthenticated shell in a container holding the GitHub App private key, every
// agent token and the hub secret. Observed live on a hosted spoke at
// *.hive.hivecommons.dev: `curl https://<spoke>/terminal/?arg=hive-<agent>`
// returned ttyd's page and the ws handshake returned 101 with live pane bytes,
// with no cookie of any kind.
//
// The existing terminal gate tests (terminal_cookie_verify.test.js and friends)
// all address the spoke at *.hive.kubestellar.io, which is exactly the one host
// the broken constant covered — so nothing exercised the failing-open case.
// This file is the same shape, addressed at the second apex.

import { WebSocket, WebSocketServer } from 'ws';
import { createServer, request as httpRequest } from 'http';
import { spawn } from 'child_process';
import path from 'path';
import crypto from 'crypto';
import { fileURLToPath } from 'url';
import assert from 'assert';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19141;
const GO_PORT = 19142;
const TTYD_PORT = 19143;
const HIVE_ID = 'hive-rebrand';
// The apex the fleet actually runs on today. Deliberately NOT the one the old
// constant named: set this to '.hive.kubestellar.io' and the test passes against
// the broken code.
const HOSTED_HOST = `${HIVE_ID}.hive.hivecommons.dev`;

const MASTER = 'rebrand-test-master-secret';
const INFO_SESSION_ED25519_SEED = 'hive-session-ed25519-v1';

function privFromSeed(seedHex) {
  const pkcs8 = Buffer.concat([
    Buffer.from('302e020100300506032b657004220420', 'hex'),
    Buffer.from(seedHex, 'hex'),
  ]);
  return crypto.createPrivateKey({ key: pkcs8, format: 'der', type: 'pkcs8' });
}

// mintCookie mirrors the hub's mintHubUserCookieValueV3.
function mintCookie(username) {
  const seed = crypto.createHmac('sha256', MASTER).update(INFO_SESSION_ED25519_SEED).digest('hex');
  const iat = Math.floor(Date.now() / 1000);
  const claims = { u: username, iat, exp: iat + 3600, sid: crypto.randomBytes(32).toString('hex') };
  const body = Buffer.from(JSON.stringify(claims)).toString('base64url');
  const sig = crypto.sign(null, Buffer.from(body), privFromSeed(seed));
  return `${body}.v3.${sig.toString('base64url')}`;
}

function mockBackend(port, body) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end(body); });
    s.listen(port, () => resolve(s));
  });
}

function mockTtydWS(port) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end('ttyd'); });
    const wss = new WebSocketServer({ server: s });
    wss.on('connection', ws => ws.send('shell'));
    s.listen(port, () => resolve(s));
  });
}

// startProxy spawns the real server.js in the shape a hub-provisioned spoke
// runs in: a hub master (so the session key resolves), a hive id, an authorized
// user list — and NO dashboard token, which is what removes the second half of
// the `isHosted || DASHBOARD_TOKEN` guard.
function startProxy() {
  return new Promise((resolve, reject) => {
    const proc = spawn('node', ['server.js'], {
      cwd: __dirname,
      env: {
        ...process.env,
        HIVE_PROXY_PORT: String(PROXY_PORT),
        HIVE_API_PORT: String(GO_PORT),
        HIVE_TTYD_PORT: String(TTYD_PORT),
        HIVE_DASHBOARD_TOKEN: '',
        HIVE_STATIC_DIR: __dirname,
        HIVE_HUB_SECRET: MASTER,
        HIVE_ID,
        HIVE_AUTHORIZED_USERS: 'alice:owner',
        NODE_ENV: 'test',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', d => {
      if (!started && d.toString().includes('hive-proxy')) { started = true; resolve(proc); }
    });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

// http.request, not fetch: fetch() drops a custom Host header, which would make
// the proxy see 127.0.0.1 and the whole test pass vacuously.
function terminalHTTP(cookie, host = HOSTED_HOST, reqPath = '/terminal') {
  return new Promise((resolve, reject) => {
    const headers = { Host: host };
    if (cookie !== null) headers.Cookie = `hive_hub_user=${cookie}`;
    const req = httpRequest(
      { host: '127.0.0.1', port: PROXY_PORT, path: reqPath, method: 'GET', headers },
      res => { res.resume(); resolve(res.statusCode); },
    );
    req.on('error', reject);
    req.end();
  });
}

function terminalWS(cookie, host = HOSTED_HOST) {
  return new Promise(resolve => {
    const headers = { Host: host };
    if (cookie !== null) headers.Cookie = `hive_hub_user=${cookie}`;
    const ws = new WebSocket(`ws://127.0.0.1:${PROXY_PORT}/terminal`, { headers });
    const done = opened => { try { ws.close(); } catch {} resolve(opened); };
    ws.on('open', () => done(true));
    ws.on('error', () => done(false));
    ws.on('close', () => done(false));
    setTimeout(() => done(false), 4000);
  });
}

let proxy, go, ttyd;
try {
  go = await mockBackend(GO_PORT, 'go');
  ttyd = await mockTtydWS(TTYD_PORT);
  proxy = await startProxy();
  await new Promise(r => setTimeout(r, 400));

  assert.equal(await terminalHTTP(null), 401,
    `anonymous GET /terminal at ${HOSTED_HOST} must be denied — an unknown hosted apex must not skip the gate`);
  assert.ok(!(await terminalWS(null)),
    `anonymous WebSocket /terminal at ${HOSTED_HOST} must be refused — the upgrade reaches the same ttyd`);
  console.log('  ✓ anonymous terminal denied on the rebranded hosted apex (HTTP + WS)');

  assert.equal(await terminalHTTP('forged-cookie-value'), 401,
    'a forged cookie must be denied on the rebranded apex too');
  console.log('  ✓ forged cookie denied on the rebranded hosted apex');

  // A hub-assigned VANITY host under no known apex still belongs to a hosted
  // spoke: IS_HOSTED (resolved session key) has to gate it on its own.
  assert.equal(await terminalHTTP(null, 'spoke-vanity.example.net'), 401,
    'a hosted spoke reached at a vanity host must still gate /terminal');
  console.log('  ✓ anonymous terminal denied at a vanity host on a hosted spoke');

  // FAIL CLOSED, not open: a bare Service IP, a forged Host and a bare
  // localhost are all "unrecognized", and every one of them must be gated
  // rather than waved past. This is the property the single hardcoded suffix
  // could not express.
  for (const weird of ['10.42.0.7:8080', 'attacker.example', 'localhost:8080']) {
    assert.equal(await terminalHTTP(null, weird), 401,
      `an unrecognized Host (${weird}) must fail CLOSED, not skip the gate`);
  }
  console.log('  ✓ unrecognized / forged Host headers fail closed');

  // CONTROL: the SSO handoff lane (/terminal?code=...) is deliberately NOT
  // gated here — the Go API redeems the code and mints the assertion cookie —
  // and the fix must not start intercepting it. That is the path the
  // dashboard's terminal button drives, so a regression here is an outage for
  // every legitimate operator.
  assert.equal(await terminalHTTP(null, HOSTED_HOST, '/terminal?code=abc123'), 200,
    'the ?code= handoff must still reach the Go API unmolested');
  console.log('  ✓ SSO handoff (?code=) lane still reaches the API');

  // CONTROL: the fix must not lock out a real, authorized hub user.
  const good = mintCookie('alice');
  assert.equal(await terminalHTTP(good), 200,
    'a valid hub-signed, per-hive-authorized cookie must still reach the terminal');
  assert.ok(await terminalWS(good),
    'a valid hub-signed, per-hive-authorized cookie must still open the WS');
  console.log('  ✓ valid hub-signed + authorized cookie still granted (HTTP + WS)');

  console.log('PASS terminal_hosted_domain.test.js');
} finally {
  if (proxy) proxy.kill();
  if (go) go.close();
  if (ttyd) ttyd.close();
}
