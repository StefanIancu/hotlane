import express from "express";

const VERSION = "v1";

const app = express();

app.get("/health", (_req, res) => {
  res.status(200).send("ok\n");
});

app.get("/", (_req, res) => {
  res.type("text/plain").send(`hello from bench-node-ts ${VERSION}\n`);
});

app.listen(3000, () => {
  console.log(`bench-node-ts ${VERSION} listening on :3000`);
});
