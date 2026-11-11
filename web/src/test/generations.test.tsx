import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Generation } from "../api";
import { activateLabel, sharedBytes, whyNot } from "../lib/generations";
import { renderApp } from "./util";

const gen = (over: Partial<Generation>): Generation => ({
  id: "g",
  created_at: "2026-11-05T00:00:00Z",
  status: "kept",
  manifest: {
    regions: [],
    sizes: { basemap: 1, terrain: 1, routing: 1, total: 3 },
    timings: {},
    checks: [],
  },
  ...over,
});

describe("generation rules", () => {
  it("calls going back to an older generation a rollback", () => {
    const live = gen({ id: "new", status: "live", created_at: "2026-11-08T00:00:00Z" });
    expect(activateLabel(gen({ status: "previous" }), live)).toBe("Roll back to this");
    expect(activateLabel(gen({ status: "kept", created_at: "2026-11-01T00:00:00Z" }), live)).toBe(
      "Roll back to this",
    );
    expect(
      activateLabel(gen({ status: "incoming", created_at: "2026-11-10T00:00:00Z" }), live),
    ).toBe("Make live");
    expect(activateLabel(gen({ status: "incoming" }), undefined)).toBe("Make live");
  });

  it("counts shared terrain once", () => {
    expect(sharedBytes({ bytes: 9, sum_bytes: 12, free_bytes: 0, total_bytes: 0 })).toBe(3);
    expect(sharedBytes({ bytes: 12, sum_bytes: 12, free_bytes: 0, total_bytes: 0 })).toBe(0);
    expect(sharedBytes(undefined)).toBe(0);
  });

  it("explains what cannot be done", () => {
    expect(whyNot(gen({ status: "live" }), "delete")).toMatch(/live generation cannot be deleted/);
    expect(whyNot(gen({ status: "building" }), "delete")).toMatch(/build is writing/);
    expect(whyNot(gen({ status: "previous", can_delete: true }), "delete")).toBeUndefined();
    expect(whyNot(gen({ status: "invalid", problems: ["x"] }), "activate")).toMatch(/incomplete/);
    expect(whyNot(gen({ status: "previous" }), "activate")).toMatch(/build is running/);
  });
});

describe("Generations page", () => {
  it("lists every generation with its state and offers only what the server allows", async () => {
    renderApp("/generations", { authed: true, oddGenerations: true });
    const live = await screen.findByRole("region", { name: "Generation 20261101-030012" });
    expect(within(live).getByText("Live")).toBeInTheDocument();
    expect(within(live).queryByRole("button", { name: /Delete/ })).not.toBeInTheDocument();
    expect(
      within(live).queryByRole("button", { name: /Make live|Roll back/ }),
    ).not.toBeInTheDocument();

    const prev = screen.getByRole("region", { name: "Generation 20261025-030009" });
    expect(within(prev).getByText("Previous")).toBeInTheDocument();
    expect(within(prev).getByRole("button", { name: /Roll back to this/ })).toBeInTheDocument();

    const copy = screen.getByRole("region", { name: "Generation 20261110-181500" });
    expect(within(copy).getByText("Copied in")).toBeInTheDocument();
    expect(within(copy).getByRole("button", { name: /Make live/ })).toBeInTheDocument();

    const bad = screen.getByRole("region", { name: "Generation 20261018-030007" });
    expect(within(bad).getByText("Invalid")).toBeInTheDocument();
    expect(within(bad).getByText("valhalla/tiles.tar is missing")).toBeInTheDocument();
    expect(
      within(bad).queryByRole("button", { name: /Make live|Roll back/ }),
    ).not.toBeInTheDocument();
    expect(within(bad).getByRole("button", { name: /Delete/ })).toBeEnabled();

    // disk: per generation and in total, shared terrain counted once
    expect(screen.getByText(/shared between them/)).toBeInTheDocument();
  });

  it("rolls back after a confirmation, and the old live one becomes the rollback", async () => {
    const { user } = renderApp("/generations");
    const prev = await screen.findByRole("region", { name: "Generation 20261025-030009" });
    await user.click(within(prev).getByRole("button", { name: /Roll back to this/ }));
    const dialog = await screen.findByRole("dialog", { name: "Roll back to this?" });
    expect(dialog).toHaveTextContent("without downtime");
    await user.click(within(dialog).getByRole("button", { name: "Roll back to this" }));

    expect(await screen.findByText(/Generation 20261025-030009 is live/)).toBeInTheDocument();
    await waitFor(() =>
      expect(
        within(screen.getByRole("region", { name: "Generation 20261025-030009" })).getByText(
          "Live",
        ),
      ).toBeInTheDocument(),
    );
    const old = screen.getByRole("region", { name: "Generation 20261101-030012" });
    expect(within(old).getByText("Previous")).toBeInTheDocument();
  });

  it("deletes a generation only after a confirmation", async () => {
    const { user } = renderApp("/generations");
    const prev = await screen.findByRole("region", { name: "Generation 20261025-030009" });
    await user.click(within(prev).getByRole("button", { name: /Delete/ }));
    const dialog = await screen.findByRole("dialog", { name: "Delete this generation?" });
    expect(dialog).toHaveTextContent("nothing to roll back to");
    // cancelling keeps it
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("region", { name: "Generation 20261025-030009" })).toBeInTheDocument();

    await user.click(
      within(screen.getByRole("region", { name: "Generation 20261025-030009" })).getByRole(
        "button",
        {
          name: /Delete/,
        },
      ),
    );
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete generation" }),
    );
    await waitFor(() =>
      expect(
        screen.queryByRole("region", { name: "Generation 20261025-030009" }),
      ).not.toBeInTheDocument(),
    );
    expect(screen.getByText(/was deleted/)).toBeInTheDocument();
    expect(screen.getByText("None")).toBeInTheDocument(); // no rollback left
  });

  it("shows what is wrong when the server refuses an activation", async () => {
    const { user } = renderApp("/generations", { authed: true, oddGenerations: true });
    // The damaged one offers no button; force the refusal through the copy by breaking it.
    const copy = await screen.findByRole("region", { name: "Generation 20261110-181500" });
    const { breakGeneration } = await import("../mocks/server");
    breakGeneration("20261110-181500", ["basemap.pmtiles does not open"]);
    await user.click(within(copy).getByRole("button", { name: /Make live/ }));
    await user.click(
      within(await screen.findByRole("dialog")).getByRole("button", { name: "Make live" }),
    );
    expect(await screen.findByText(/live generation is unchanged/)).toBeInTheDocument();
    expect(screen.getByText("basemap.pmtiles does not open")).toBeInTheDocument();
  });

  it("first run: explains where generations come from", async () => {
    renderApp("/generations", { authed: true, empty: true });
    expect(await screen.findByText("No generations yet")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Choose regions and build" })).toBeInTheDocument();
  });
});
