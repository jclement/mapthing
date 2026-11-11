import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { RefreshPolicy } from "../api";
import {
  inWindow,
  lastCheck,
  nextRefresh,
  policySummary,
  refreshCost,
  resolvePolicy,
  validWindow,
} from "../lib/refresh";
import { renderApp } from "./util";

const def = { cron: "0 3 * * 0", enabled: true, window: "", only_changed: true };
const wed = new Date("2026-11-04T12:00:00Z"); // a Wednesday
const resolve = (
  own: Parameters<typeof resolvePolicy>[0],
  d = def,
  handled = wed,
  now = wed,
): RefreshPolicy => resolvePolicy(own, d, handled, now);

describe("refresh settings", () => {
  it("a region without a setting follows the default schedule", () => {
    const p = resolve({ mode: "default", only_changed: false });
    expect(p.effective).toEqual({
      cron: "0 3 * * 0",
      window: "",
      only_changed: true, // the default's, not its own
      inherited: true,
    });
    expect(p.next_due).toBe("2026-11-08T03:00:00.000Z");
    expect(p.due).toBe(false);
    expect(policySummary(p)).toBe("At 03:00, only on Sunday (default schedule)");
  });

  it("the default schedule off means its followers are not refreshed", () => {
    const p = resolve({ mode: "default", only_changed: true }, { ...def, enabled: false });
    expect(p.effective.cron).toBeNull();
    expect(p.next_due).toBeNull();
    expect(nextRefresh(p)).toBe("never");
    expect(policySummary(p)).toMatch(/default schedule is off/);
    // a region with its own setting is not affected
    expect(
      resolve({ mode: "daily", only_changed: true }, { ...def, enabled: false }).next_due,
    ).toBe("2026-11-05T03:00:00.000Z");
  });

  it("daily, weekly, monthly and a cron of its own", () => {
    expect(resolve({ mode: "daily", only_changed: true }).next_due).toBe(
      "2026-11-05T03:00:00.000Z",
    );
    expect(resolve({ mode: "weekly", only_changed: true }).next_due).toBe(
      "2026-11-08T03:00:00.000Z",
    );
    expect(resolve({ mode: "monthly", only_changed: true }).next_due).toBe(
      "2026-12-01T03:00:00.000Z",
    );
    const c = resolve({ mode: "cron", cron: "30 14 * * 3", only_changed: false });
    expect(c.next_due).toBe("2026-11-04T14:30:00.000Z");
    expect(c.effective.only_changed).toBe(false);
    expect(policySummary(c)).toBe("At 14:30, only on Wednesday");
    const never = resolve({ mode: "never", only_changed: true });
    expect(never.next_due).toBeNull();
    expect(policySummary(never)).toBe("Never");
  });

  it("a build window delays a due refresh until it opens", () => {
    const noon = new Date("2026-11-05T12:00:00Z");
    const p = resolve(
      { mode: "cron", cron: "0 12 * * *", only_changed: true },
      { ...def, window: "22:00-06:00" },
      wed,
      noon,
    );
    expect(p).toMatchObject({ due: true, waiting: true, next_build: "2026-11-05T22:00:00.000Z" });
    expect(nextRefresh(p, noon.getTime())).toMatch(
      /^due, waiting for its build window \(in 10 h\)/,
    );
    expect(policySummary(p)).toMatch(/builds 22:00–06:00 UTC$/);
    // its own window wins over the default's
    const own = resolve(
      { mode: "daily", window: "13:00-15:00", only_changed: true },
      { ...def, window: "22:00-06:00" },
    );
    expect(own.effective.window).toBe("13:00-15:00");
    expect(inWindow("22:00-06:00", new Date("2026-11-05T23:30:00Z"))).toBe(true);
    expect(inWindow("22:00-06:00", new Date("2026-11-05T05:59:00Z"))).toBe(true);
    expect(inWindow("22:00-06:00", new Date("2026-11-05T06:00:00Z"))).toBe(false);
    expect(inWindow("", new Date())).toBe(true);
    expect(validWindow("22:00-06:00")).toBe(true);
    expect(validWindow("")).toBe(true);
    expect(validWindow("03:00-03:00")).toBe(false);
    expect(validWindow("night")).toBe(false);
  });

  it("says what the last check found and what a refresh costs", () => {
    const now = Date.parse("2026-11-04T12:00:00Z");
    const p = resolve({ mode: "daily", only_changed: true });
    expect(lastCheck(p, now)).toBe("not checked yet");
    expect(
      lastCheck({ ...p, last_checked_at: "2026-11-04T10:00:00Z", last_check: "unchanged" }, now),
    ).toBe("2 h ago: nothing new");
    expect(
      lastCheck({ ...p, last_checked_at: "2026-11-04T10:00:00Z", last_check: "changed" }, now),
    ).toBe("2 h ago: newer data available");
    expect(
      lastCheck({ ...p, last_checked_at: "2026-11-04T10:00:00Z", last_check: "error" }, now),
    ).toBe("2 h ago: could not check");
    expect(refreshCost({ download_bytes: 512e6, build_seconds: 6000 })).toBe(
      "512 MB download + ~1 h 40 min build",
    );
    expect(refreshCost({ download_bytes: 0, build_seconds: null })).toBe(
      "download size unknown + a build",
    );
  });
});

describe("Schedule page", () => {
  it("shows each region's setting, next refresh, last check, data date and cost", async () => {
    renderApp("/schedule");
    const texas = await screen.findByRole("listitem", { name: "Texas" });
    expect(texas).toHaveTextContent("(default schedule)");
    expect(texas).toHaveTextContent(/Next refresh: in \d+ d/);
    expect(texas).toHaveTextContent(/Last check: 3 d ago: newer data available/);
    expect(texas).toHaveTextContent(/data of /);
    expect(texas).toHaveTextContent(/A refresh costs: .* download \+ ~.* build/);
    const alberta = screen.getByRole("listitem", { name: "Alberta" });
    expect(alberta).toHaveTextContent("Daily, builds 22:00–06:00 UTC");
    expect(alberta).toHaveTextContent(/nothing new/);
    const permian = screen.getByRole("listitem", { name: "Permian core" });
    expect(permian).toHaveTextContent("Never");
    expect(permian).toHaveTextContent("Next refresh: never");
    // why one region's refresh rebuilds everything
    expect(screen.getByText(/over all\s+regions together/)).toBeInTheDocument();
    // what the scheduler decided lately
    expect(screen.getByText(/nothing new \(Alberta unchanged/)).toBeInTheDocument();
  });

  it("changes one region's refresh without touching the others", async () => {
    const { user } = renderApp("/schedule");
    const texas = await screen.findByRole("listitem", { name: "Texas" });
    await user.click(within(texas).getByRole("button", { name: "Refresh settings for Texas" }));
    const dialog = await screen.findByRole("dialog", { name: "Refresh Texas" });
    await user.click(within(dialog).getByRole("radio", { name: /^Monthly/ }));
    // its own options appear only for a setting of its own
    await user.click(
      within(dialog).getByRole("switch", { name: "Only when the source has something new" }),
    );
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    const after = screen.getByRole("listitem", { name: "Texas" });
    expect(after).toHaveTextContent("Monthly");
    expect(after).toHaveTextContent("even if unchanged");
    expect(screen.getByRole("listitem", { name: "Alberta" })).toHaveTextContent("Daily");

    // a cron of its own is validated before it can be saved
    await user.click(within(after).getByRole("button", { name: "Refresh settings for Texas" }));
    const again = await screen.findByRole("dialog", { name: "Refresh Texas" });
    await user.click(within(again).getByRole("radio", { name: /^Custom/ }));
    const cron = within(again).getByLabelText("Cron expression (UTC)");
    await user.clear(cron);
    await user.type(cron, "not a cron");
    expect(within(again).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.clear(cron);
    await user.type(cron, "0 2 * * 1-5");
    await user.click(within(again).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByRole("listitem", { name: "Texas" })).toHaveTextContent(
      "At 02:00, Monday through Friday",
    );
  });

  it("the default's window and only-when-changed are saved with it", async () => {
    const { user } = renderApp("/schedule");
    await screen.findByRole("listitem", { name: "Texas" });
    const save = screen.getByRole("button", { name: "Save default" });
    expect(save).toBeDisabled();
    await user.click(screen.getByRole("switch", { name: "Limit builds to certain hours" }));
    expect(screen.getByLabelText("Window opens (UTC)")).toHaveValue("22:00");
    await user.click(save);
    expect(await screen.findByRole("button", { name: "Saved" })).toBeInTheDocument();
    // regions on the default now build in the window
    await waitFor(() =>
      expect(screen.getByRole("listitem", { name: "Texas" })).toHaveTextContent(
        "builds 22:00–06:00 UTC",
      ),
    );
  });

  it("checks sources on request and shows what it found", async () => {
    const { user } = renderApp("/schedule");
    const alberta = await screen.findByRole("listitem", { name: "Alberta" });
    expect(alberta).toHaveTextContent(/Last check: \d+ h ago/);
    await user.click(screen.getByRole("button", { name: "Check sources now" }));
    await waitFor(() =>
      expect(screen.getByRole("listitem", { name: "Alberta" })).toHaveTextContent(
        "Last check: just now: nothing new",
      ),
    );
    expect(screen.getByRole("listitem", { name: "Texas" })).toHaveTextContent(
      "just now: newer data available",
    );
    // a region set to never is not asked about
    expect(screen.getByRole("listitem", { name: "Permian core" })).toHaveTextContent(
      "Last check: not checked yet",
    );
  });

  it("first run: nothing to schedule yet", async () => {
    renderApp("/schedule", { authed: true, empty: true });
    expect(await screen.findByText("No regions yet")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Check sources now" })).toBeDisabled();
  });
});
