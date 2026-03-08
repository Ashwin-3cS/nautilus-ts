/**
 * Example Nautilus server.
 *
 * This file is a starting point — add your routes below.
 */

import { boot } from "./nautilus.ts";

const { app, ctx } = await boot({ port: 3000 });

// Add your routes here:
// app.post("/my_endpoint", async (c) => {
//   const body = await c.req.json();
//   const sig = ctx.sign(ctx.blake2b256(data));
//   return c.json({ signature: ctx.toHex(sig) });
// });

export default { port: 3000, hostname: "127.0.0.1", fetch: app.fetch };
