const http = require('http');

const server = http.createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
    return;
  }

  res.writeHead(200, { 'content-type': 'text/plain' });
  res.end('Stripe co-op eval fixture');
});

if (require.main === module) {
  server.listen(4242, () => {
    console.log('fixture listening on http://localhost:4242');
    server.close();
  });
}

module.exports = server;
