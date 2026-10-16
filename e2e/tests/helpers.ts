import { randomUUID } from "node:crypto";
import AxeBuilder from "@axe-core/playwright";
import { expect, type Page } from "@playwright/test";

export const issuerURL = process.env.DEV_ISSUER_URL ?? "http://localhost:8212";
export const mcpURL = process.env.MCP_URL ?? "http://localhost:8210/mcp";
export const adminURL = process.env.CONSOLE_URL ?? "http://localhost:8200";

export type Persona = "platform-engineer" | "security-reviewer" | "approver";

/** Signs in through the console's persona form. */
export async function signIn(page: Page, persona: Persona): Promise<void> {
  await page.goto("/console/signin");
  await page.locator(`button[name="persona"][value="${persona}"]`).click();
  await expect(page.getByText("Signed in as")).toBeVisible();
}

/** Mints a token from the dev issuer (synthetic tenants and agents only). */
export async function token(tenant: string, agent: string, scopes: string[]): Promise<string> {
  const resp = await fetch(`${issuerURL}/token`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant, agent, scopes }),
  });
  expect(resp.status).toBe(200);
  const body = (await resp.json()) as { access_token: string };
  return body.access_token;
}

type RPCResult = { result?: { structuredContent?: Record<string, unknown> }; error?: { code: number } };

/** Parses a JSON or text/event-stream JSON-RPC response. */
async function rpcBody(resp: Response): Promise<RPCResult | undefined> {
  const text = await resp.text();
  if ((resp.headers.get("content-type") ?? "").includes("text/event-stream")) {
    const data = text
      .split("\n")
      .filter((l) => l.startsWith("data:"))
      .map((l) => l.slice(5).trim())
      .pop();
    return data ? (JSON.parse(data) as RPCResult) : undefined;
  }
  return text ? (JSON.parse(text) as RPCResult) : undefined;
}

/**
 * Calls orders.refund_order for 500.00 as agent-support of t_alpha over MCP; policy holds it, and
 * the gateway answers with an approval_required result. Returns the approval id.
 */
export async function holdRefund(): Promise<string> {
  const bearer = await token("t_alpha", "agent-support", ["orders:read", "orders:write"]);
  const headers: Record<string, string> = {
    Authorization: `Bearer ${bearer}`,
    "Content-Type": "application/json",
    Accept: "application/json, text/event-stream",
  };
  const post = (body: unknown) => fetch(mcpURL, { method: "POST", headers, body: JSON.stringify(body) });
  const init = await post({
    jsonrpc: "2.0",
    id: 1,
    method: "initialize",
    params: { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "e2e", version: "0" } },
  });
  expect(init.status).toBe(200);
  await rpcBody(init);
  const session = init.headers.get("mcp-session-id");
  if (session) {
    headers["Mcp-Session-Id"] = session;
    headers["Mcp-Protocol-Version"] = "2025-11-25";
  }
  await (await post({ jsonrpc: "2.0", method: "notifications/initialized" })).text();
  const call = await post({
    jsonrpc: "2.0",
    id: 2,
    method: "tools/call",
    params: {
      name: "orders.refund_order",
      arguments: { order_id: "O-00042", amount_cents: 50000, reason: "e2e: damaged item" },
      _meta: { idempotency_key: `e2e-${randomUUID()}` },
    },
  });
  const res = await rpcBody(call);
  const held = res?.result?.structuredContent;
  expect(held?.status, JSON.stringify(res)).toBe("approval_required");
  return held?.approval_id as string;
}

/** Fails on serious or critical axe violations (WCAG 2.2 AA rule set). */
export async function expectAccessible(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  const blocking = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical");
  expect(blocking.map((v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`)).toEqual(
    [],
  );
}

/** Presses Tab until the element matching selector has focus (keyboard-only navigation). */
export async function tabTo(page: Page, selector: string, maxTabs = 80): Promise<void> {
  for (let i = 0; i < maxTabs; i++) {
    if (await page.locator(selector).evaluate((el) => el === document.activeElement)) return;
    await page.keyboard.press("Tab");
  }
  throw new Error(`could not reach ${selector} with Tab`);
}
