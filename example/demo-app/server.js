// Dependency-free demo app for hotlane development and e2e smoke tests.
const http = require("http");

const VERSION = "v1";

http
  .createServer((req, res) => {
    if (req.url === "/health") {
      res.writeHead(200, { "content-type": "text/plain" });
      res.end("ok\n");
      return;
    }
    res.writeHead(200, { "content-type": "text/plain" });
    res.end(`hello from demo-app ${VERSION}\n`);
  })
  .listen(3000, () => console.log(`demo-app ${VERSION} listening on :3000`));
