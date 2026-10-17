import { mkdirSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";
import { expectAccessible, holdRefund, signIn, type Persona } from "./helpers.js";

// §13: every console page passes axe (no serious or critical violations) at 375, 768 and 1280 px
// in both themes. With SCREENSHOTS=1 (make docs) the renders are stored in docs/images/.

const images = new URL("../../docs/images/", import.meta.url).pathname;
const screenshots = process.env.SCREENSHOTS === "1";
if (screenshots) mkdirSync(images, { recursive: true });

type Screen = { name: string; persona: Persona | null; open: (page: Page, approvalID: string) => Promise<void> };

const screens: Screen[] = [
  { name: "signin", persona: null, open: async (page) => void (await page.goto("/console/signin")) },
  { name: "audit", persona: "security-reviewer", open: async (page) => void (await page.goto("/console/audit")) },
  {
    name: "audit-event",
    persona: "security-reviewer",
    open: async (page) => {
      await page.goto("/console/audit");
      await page.locator("tr.row a").first().click();
      await expect(page.locator(".hash").first()).toBeVisible();
    },
  },
  { name: "approvals", persona: "approver", open: async (page) => void (await page.goto("/console/approvals")) },
  {
    name: "approval",
    persona: "approver",
    open: async (page, id) => void (await page.goto(`/console/approvals/${id}`)),
  },
  {
    name: "policy",
    persona: "platform-engineer",
    open: async (page) => {
      await page.goto("/console/policy?example=refund-large");
      await page.getByRole("button", { name: "Evaluate" }).click();
      await expect(page.locator("#result-h .status")).toHaveText("hold");
    },
  },
  {
    name: "upstreams",
    persona: "platform-engineer",
    open: async (page) => void (await page.goto("/console/upstreams")),
  },
];

let approvalID = "";
test.beforeAll(async () => {
  approvalID = await holdRefund();
});

for (const theme of ["light", "dark"] as const) {
  for (const width of [375, 768, 1280]) {
    test.describe(`${theme} ${width}px`, () => {
      test.use({ colorScheme: theme, viewport: { width, height: 900 } });
      for (const s of screens) {
        test(`${s.name} is accessible`, async ({ page }) => {
          if (s.persona) await signIn(page, s.persona);
          await s.open(page, approvalID);
          await expect(page.locator("main")).toHaveAttribute("aria-busy", "false");
          const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
          expect(overflow, "no horizontal page scroll").toBeLessThanOrEqual(0);
          await expectAccessible(page);
          if (screenshots && (width === 1280 || theme === "light")) {
            await page.screenshot({ path: `${images}console-${s.name}-${theme}-${width}.png`, fullPage: true });
          }
        });
      }
    });
  }
}

test("error state shows a banner with a way out", async ({ page }) => {
  await signIn(page, "security-reviewer");
  await page.goto("/console/audit?limit=bogus");
  const alert = page.getByRole("alert");
  await expect(alert).toBeVisible();
  await expect(alert.getByRole("link", { name: "Reset filters" })).toBeVisible();
  await expectAccessible(page);
});

test("empty state explains what to do", async ({ page }) => {
  await signIn(page, "security-reviewer");
  await page.goto("/console/audit?tool=nope.nothing");
  await expect(page.getByText("No audit events match these filters.")).toBeVisible();
  await expectAccessible(page);
});

for (const width of [375, 768]) {
  test.describe(`${width}px lists`, () => {
    test.use({ viewport: { width, height: 900 } });

    // Rows stack as labelled cards: no list table overflows sideways, and the chip is on screen.
    test("every list column is visible without horizontal scrolling", async ({ page }) => {
      const noSidewaysScroll = async () => {
        const overflow = await page.locator(".table-wrap").evaluate((el) => el.scrollWidth - el.clientWidth);
        expect(overflow).toBeLessThanOrEqual(0);
      };
      await holdRefund();
      await signIn(page, "approver");
      for (const path of ["/console/approvals", "/console/audit", "/console/upstreams"]) {
        await page.goto(path);
        await noSidewaysScroll();
      }
      await expect(page.locator("main .chip", { hasText: "untrusted output" }).first()).toBeInViewport();
    });
  });
}

// The selected status filter and policy example look different from the others, not only to
// assistive technology (aria-current).
test("selected filter and example are visibly marked", async ({ page }) => {
  await signIn(page, "approver");
  for (const path of ["/console/approvals", "/console/policy?example=refund-small"]) {
    await page.goto(path);
    const current = page.locator(".tabs a[aria-current]").first();
    const other = page.locator(".tabs a:not([aria-current])").first();
    const style = (loc: typeof current) =>
      loc.evaluate((el) => {
        const cs = getComputedStyle(el);
        return `${cs.borderTopColor}|${cs.backgroundColor}|${cs.fontWeight}`;
      });
    expect(await style(current)).not.toEqual(await style(other));
  }
  await expectAccessible(page);
});
