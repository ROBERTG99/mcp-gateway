import { expect, test } from "@playwright/test";
import { adminURL, expectAccessible, holdRefund, signIn, tabTo, token } from "./helpers.js";

// FR-6, FR-12: a held refund is approved by the approver persona using the keyboard only, and the
// decision is audited; the security reviewer sees no decision buttons and the API refuses it.

test("approver approves a held refund with the keyboard only", async ({ page }) => {
  const id = await holdRefund();
  await signIn(page, "approver");
  await page.goto("/console/approvals");
  await expect(page.getByRole("link", { name: `Review approval ${id}` })).toBeVisible();
  await expectAccessible(page);

  await tabTo(page, `a[href="/console/approvals/${id}"]`);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { name: "What will execute" })).toBeVisible();
  await expect(page.locator("pre")).toContainText("e2e: damaged item");
  await expectAccessible(page);

  await tabTo(page, "#note");
  await page.keyboard.type("Verified with the customer by phone.");
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Approve" })).toBeFocused();
  await page.keyboard.press("Enter");

  await expect(page.getByRole("status")).toContainText("Approved");
  await expect(page.locator("h1 .status")).toHaveText("approved");
  await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
  await expectAccessible(page);

  // The decision is in the audit log, attributed to the approver.
  await page.goto(`/console/audit?approval_id=${id}`);
  await expect(page.locator("tr.row", { hasText: "mcpgw.approval.approve" })).toContainText("jo.approver");
});

test("security reviewer sees no decision buttons and the endpoint returns 403", async ({ page, request }) => {
  const id = await holdRefund();
  await signIn(page, "security-reviewer");
  await page.goto(`/console/approvals/${id}`);
  await expect(page.getByRole("heading", { name: "What will execute" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Reject" })).toHaveCount(0);
  await expectAccessible(page);

  const reviewer = await token("*", "sam.security", ["mcpgw:audit:read"]);
  const resp = await request.post(`${adminURL}/admin/approvals/${id}:approve`, {
    headers: { Authorization: `Bearer ${reviewer}` },
    data: { note: "not mine to approve" },
  });
  expect(resp.status()).toBe(403);
  expect(resp.headers()["content-type"]).toContain("application/problem+json");
});

test("approver rejects with a required reason", async ({ page }) => {
  const id = await holdRefund();
  await signIn(page, "approver");
  await page.goto(`/console/approvals/${id}`);
  await tabTo(page, "#reason");
  await page.keyboard.type("Duplicate of an earlier refund.");
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Reject" })).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("status")).toContainText("Rejected");
  await expect(page.locator("h1 .status")).toHaveText("rejected");
});
