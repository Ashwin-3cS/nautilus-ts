/**
 * Example Nautilus server.
 *
 * This file is intentionally minimal and intended as a starting point.
 * Add your business routes here.
 */

import { Nautilus } from "./nautilus.ts";

const app = new Nautilus();

// Add your routes here:
// app.post("/my_endpoint", async (req, ctx) => { ... });

app.start();
