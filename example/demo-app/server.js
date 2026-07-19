// Dependency-free demo app for hotlane development and e2e smoke tests.
// The root route reads message.txt per request so that runtime filesystem
// drift (someone editing the live container) is observable behavior.
const http = require("http");
const fs = require("fs");

const VERSION = "v1";

http
  .createServer((req, res) => {
    if (req.url === "/health") {
      res.writeHead(200, { "content-type": "text/plain" });
      res.end("ok\n");
      return;
    }
    const message = fs.readFileSync("message.txt", "utf8").trim();
    res.writeHead(200, { "content-type": "text/plain" });
    res.end(`${message} from demo-app ${VERSION}\n`);
  })
  .listen(3000, () => console.log(`demo-app ${VERSION} listening on :3000`));
