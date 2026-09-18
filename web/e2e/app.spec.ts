import { expect, test, type Page } from "@playwright/test";

declare global {
  interface Window {
    __kbSync?: { disconnect(): void; reconnect(): void; status(): string; docs(): unknown[] };
  }
}

// Each browser context holds its own local key, so each test signs in with a
// fresh identity and builds the workspace it needs.

async function signIn(page: Page): Promise<string> {
  await page.goto("/app/");
  await page.getByTestId("signin-key").click();
  const me = page.getByTestId("me-did");
  await expect(me).toBeVisible();
  return (await me.getAttribute("title")) ?? "";
}

async function createWorkspace(page: Page, name: string): Promise<void> {
  await page.getByTestId("new-workspace-name").fill(name);
  await page.getByTestId("new-workspace-submit").click();
  await expect(page.getByTestId("project-name")).toBeVisible();
  await expect(page.getByTestId("workspace-name")).toHaveText(name);
}

async function createPage(page: Page, title: string): Promise<string> {
  await page.getByTestId("new-page-title").fill(title);
  await page.getByTestId("new-page-submit").click();
  await expect(page.getByTestId("page-title")).toHaveValue(title);
  await expect(page.getByTestId("block-editor")).toBeVisible();
  return page.url();
}

async function focusBlock(page: Page, index: number): Promise<void> {
  await page.getByTestId("block").nth(index).locator(".preview, .editor-host").first().click();
  await expect(page.getByTestId("block").nth(index).getByTestId("block-editor")).toBeVisible();
  await page.keyboard.press("End");
}

test("sign in with a local key, create a workspace and a page, edit it from two tabs", async ({
  page,
  context,
}) => {
  const did = await signIn(page);
  expect(did.startsWith("did:key:z6Mk")).toBeTruthy();
  await createWorkspace(page, `E2E ${Date.now()}`);
  const pageUrl = await createPage(page, "Hello world");

  // The first block is created and focused automatically once the doc is open.
  await page.keyboard.type("First line");
  await page.keyboard.press("Enter");
  await page.keyboard.type("Second line");
  await expect(page.getByTestId("block")).toHaveCount(2);
  await expect(page.getByTestId("sync-pill")).toHaveText(/Saved/);

  // A second tab of the same browser: same identity, its own sync client.
  const other = await context.newPage();
  await other.goto(pageUrl);
  await expect(other.getByTestId("block")).toHaveCount(2);
  await expect(other.getByTestId("blocks")).toContainText("First line");
  await expect(other.getByTestId("blocks")).toContainText("Second line");

  await focusBlock(other, 1);
  await other.keyboard.type(" from tab two");
  await expect(page.getByTestId("blocks")).toContainText("Second line from tab two");
  await expect(page.getByTestId("presence").locator(".avatar")).toHaveCount(1);

  // Cut the connection: edits queue locally and flush on reconnect.
  await page.evaluate(() => window.__kbSync?.disconnect());
  await focusBlock(page, 0);
  await page.keyboard.type(" offline");
  await expect(page.getByTestId("sync-pill")).toHaveText(/Offline/);
  await page.evaluate(() => window.__kbSync?.reconnect());
  await expect(page.getByTestId("sync-pill")).toHaveText(/Saved/, { timeout: 30_000 });
  await expect(other.getByTestId("blocks")).toContainText("First line offline");

  // Indent the second block under the first and check it survives a reload.
  await focusBlock(page, 1);
  await page.keyboard.press("Tab");
  await expect(page.getByTestId("block").nth(1)).toHaveCSS("padding-left", "24px");
  await expect(page.getByTestId("sync-pill")).toHaveText(/Saved/);
  await page.reload();
  await expect(page.getByTestId("blocks")).toContainText("First line offline");
  await expect(page.getByTestId("blocks")).toContainText("Second line from tab two");
  await expect(page.getByTestId("block").nth(1)).toHaveCSS("padding-left", "24px");
  await other.close();
});

test("invite a second identity and collaborate across browsers", async ({ browser, page }) => {
  const ownerDid = await signIn(page);
  const workspaceName = `Shared ${Date.now()}`;
  await createWorkspace(page, workspaceName);
  const pageUrl = await createPage(page, "Team notes");
  await page.keyboard.type("Owner wrote this");
  await expect(page.getByTestId("sync-pill")).toHaveText(/Saved/);

  // The guest signs in with its own key in a separate browser profile.
  const guestCtx = await browser.newContext();
  const guest = await guestCtx.newPage();
  const guestDid = await signIn(guest);
  expect(guestDid).not.toBe(ownerDid);

  // The owner invites the guest as editor and hands over the accept link.
  await page.getByTestId("toggle-members").click();
  await page.getByTestId("invite-did").fill(guestDid);
  await page.getByTestId("invite-submit").click();
  const link = await page.getByTestId("invite-link").inputValue();
  expect(link).toContain("/app/invite/");

  await guest.goto(link);
  await expect(guest.getByTestId("workspace-name")).toHaveText(workspaceName);
  await guest.goto(pageUrl);
  await expect(guest.getByTestId("blocks")).toContainText("Owner wrote this");
  await focusBlock(guest, 0);
  await guest.keyboard.type(" and the guest added this");
  await expect(page.getByTestId("blocks")).toContainText("Owner wrote this and the guest added this");
  await expect(page.getByTestId("presence").locator(".avatar")).toHaveCount(1);
  // Closing the guest's browser announces the departure at once (no TTL wait).
  await guestCtx.close();
  await expect(page.getByTestId("presence").locator(".avatar")).toHaveCount(0);
});

test("today's journal opens a dated page and work items get keys", async ({ page }) => {
  await signIn(page);
  await createWorkspace(page, `Journal ${Date.now()}`);
  await page.getByTestId("journal-today").click();
  await expect(page.getByTestId("page-title")).toHaveValue(/^\d{4}-\d{2}-\d{2}$/);
  await page.getByText("‹ Documents").click();
  await page.getByTestId("tab-work").click();
  await page.getByTestId("new-page-title").fill("Ship the demo");
  await page.getByTestId("new-page-submit").click();
  await expect(page.getByTestId("page-title")).toHaveValue("Ship the demo");
  await expect(page.locator(".page-head .key")).toHaveText(/^[A-Z]+-\d+$/);
  await page.getByText("‹ Work").click();
  await expect(page.getByTestId("page-list")).toContainText("Ship the demo");
});
