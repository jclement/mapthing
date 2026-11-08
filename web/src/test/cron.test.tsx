import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { previewCron } from "../lib/cron";
import { CronEditor } from "../pages/Schedule";

describe("previewCron", () => {
  const from = new Date("2026-11-08T12:00:00Z"); // a Sunday

  it("describes and lists the next three runs in UTC", () => {
    const p = previewCron("0 3 * * 0", 3, from);
    expect(p.ok).toBe(true);
    if (!p.ok) return;
    expect(p.description).toMatch(/03:00/);
    expect(p.description).toMatch(/Sunday/);
    expect(p.description).toMatch(/UTC/);
    expect(p.next.map((d) => d.toISOString())).toEqual([
      "2026-11-15T03:00:00.000Z",
      "2026-11-22T03:00:00.000Z",
      "2026-11-29T03:00:00.000Z",
    ]);
  });

  it("handles monthly and custom expressions", () => {
    const p = previewCron("30 2 1 * *", 2, from);
    expect(p.ok && p.next.map((d) => d.toISOString())).toEqual([
      "2026-12-01T02:30:00.000Z",
      "2027-01-01T02:30:00.000Z",
    ]);
  });

  it("rejects malformed expressions", () => {
    expect(previewCron("0 3 * *").ok).toBe(false);
    expect(previewCron("61 3 * * *").ok).toBe(false);
    expect(previewCron("hello world foo bar baz").ok).toBe(false);
  });
});

describe("CronEditor", () => {
  function Harness() {
    const [v, setV] = useState("0 3 * * 0");
    return <CronEditor value={v} onChange={setV} />;
  }

  it("switches presets and previews custom expressions", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    expect(screen.getByRole("radio", { name: "Weekly" })).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByRole("radio", { name: "Daily" }));
    const input = screen.getByLabelText("Cron expression (UTC)");
    expect(input).toHaveValue("0 3 * * *");
    expect(screen.getByTestId("cron-preview")).toHaveTextContent("Next runs");

    await user.clear(input);
    await user.type(input, "15 4 * * 1-5");
    expect(screen.getByRole("radio", { name: "Custom" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByTestId("cron-preview")).toHaveTextContent(/04:15/);
    expect(screen.getByTestId("cron-preview")).toHaveTextContent(/Monday through Friday/);

    await user.clear(input);
    await user.type(input, "nonsense");
    expect(screen.queryByTestId("cron-preview")).not.toBeInTheDocument();
    expect(screen.getByText(/five fields/)).toBeInTheDocument();
  });
});
