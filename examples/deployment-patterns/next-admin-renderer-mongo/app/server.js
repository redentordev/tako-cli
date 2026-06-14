const http = require('http');
const net = require('net');

const role = process.env.SERVICE_ROLE || 'admin';
const hostname = process.env.HOSTNAME || 'local';
const runtimeToken = process.env.RUNTIME_API_TOKEN || '';
const mongoUrl = process.env.MONGO_URL || 'mongodb://mongo:27017/jardin_fixture';
const adminUrl = (process.env.ADMIN_URL || 'http://admin:3000').replace(/\/$/, '');
const port = Number(process.env.PORT || 3000);

function mongoTarget() {
  try {
    const parsed = new URL(mongoUrl);
    return {
      host: parsed.hostname || 'mongo',
      port: Number(parsed.port || 27017),
    };
  } catch {
    return {host: 'mongo', port: 27017};
  }
}

function checkTCP({host, port}, timeoutMs = 1200) {
  return new Promise((resolve, reject) => {
    const socket = net.createConnection({host, port});
    const timer = setTimeout(() => {
      socket.destroy();
      reject(new Error(`timeout connecting to ${host}:${port}`));
    }, timeoutMs);

    socket.once('connect', () => {
      clearTimeout(timer);
      socket.end();
      resolve();
    });
    socket.once('error', (error) => {
      clearTimeout(timer);
      reject(error);
    });
  });
}

async function adminHealth() {
  if (!runtimeToken) {
    throw new Error('runtime token is not configured');
  }
  await checkTCP(mongoTarget());
}

async function rendererHealth() {
  const response = await fetch(`${adminUrl}/api/health`, {
    headers: {'x-runtime-api-token': runtimeToken},
  });
  if (!response.ok) {
    throw new Error(`admin health returned ${response.status}`);
  }
}

async function health() {
  if (role === 'renderer') {
    await rendererHealth();
    return;
  }
  await adminHealth();
}

function json(res, status, body) {
  res.writeHead(status, {'content-type': 'application/json'});
  res.end(`${JSON.stringify(body)}\n`);
}

function text(res, status, body) {
  res.writeHead(status, {'content-type': 'text/plain'});
  res.end(`${body}\n`);
}

function tokenAccepted(req) {
  return runtimeToken !== '' && req.headers['x-runtime-api-token'] === runtimeToken;
}

async function handler(req, res) {
  if (req.url === '/api/health') {
    try {
      await health();
      json(res, 200, {ok: true, role, hostname});
    } catch (error) {
      json(res, 503, {ok: false, role, error: error.message});
    }
    return;
  }

  if (req.url === '/api/runtime') {
    if (!tokenAccepted(req)) {
      json(res, 401, {ok: false, error: 'runtime token rejected'});
      return;
    }
    json(res, 200, {
      ok: true,
      role,
      hostname,
      tokenAccepted: true,
      tokenValueExposed: false,
    });
    return;
  }

  if (req.url === '/api/platform/domains/ask') {
    json(res, 200, {ok: true, allowed: true});
    return;
  }

  if (req.url === '/api/render') {
    const response = await fetch(`${adminUrl}/api/runtime`, {
      headers: {'x-runtime-api-token': runtimeToken},
    });
    const body = await response.json();
    json(res, response.ok ? 200 : 502, {
      ok: response.ok,
      role,
      hostname,
      adminReachable: response.ok,
      admin: body,
    });
    return;
  }

  text(res, 200, `${role} ${hostname}`);
}

http.createServer((req, res) => {
  handler(req, res).catch((error) => {
    json(res, 500, {ok: false, role, error: error.message});
  });
}).listen(port, '0.0.0.0', () => {
  console.log(`next-admin-renderer-mongo ${role} listening on ${port}`);
});

