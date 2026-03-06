/**
 * Example Nautilus server.
 *
 * This is where your application logic goes.
 * The platform (VSOCK, NSM, networking) is handled by the framework.
 */

import { Nautilus } from "./nautilus.ts";

const app = new Nautilus();

// Example: sign arbitrary data
app.post("/process_data", async (req, ctx) => {
  const body = await req.arrayBuffer();
  const data = new Uint8Array(body);

  // Sign the data with the enclave's ephemeral key
  const signature = ctx.sign(ctx.blake2b256(data));

  return Response.json({
    data_hash: ctx.toHex(ctx.blake2b256(data)),
    signature: ctx.toHex(signature),
    public_key: ctx.publicKey,
  });
});

app.start();
