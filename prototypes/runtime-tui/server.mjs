import { createReadStream } from "node:fs";
import { createServer } from "node:http";
import { fileURLToPath } from "node:url";

const indexPath = fileURLToPath(new URL("./index.html", import.meta.url));

createServer((_request, response) => {
  response.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  createReadStream(indexPath).pipe(response);
}).listen(8781, "127.0.0.1", () => {
  console.log("Runtime TUI prototype: http://127.0.0.1:8781/?variant=A");
});
