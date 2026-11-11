// What the Generations page says about each generation, apart from the component.
import type { Generation, GenerationList, GenerationStatus } from "../api";

type Tone = "neutral" | "ok" | "warn" | "bad" | "info" | "accent";

export const STATUS: Record<GenerationStatus, { label: string; tone: Tone; what: string }> = {
  live: { label: "Live", tone: "accent", what: "Being served now." },
  previous: {
    label: "Previous",
    tone: "ok",
    what: "Kept so you can roll back. The next successful build replaces it.",
  },
  kept: { label: "Kept", tone: "neutral", what: "Complete, but not the rollback slot." },
  building: { label: "Building", tone: "info", what: "A build is writing it now." },
  incoming: {
    label: "Copied in",
    tone: "info",
    what: "Copied from another machine and not activated here yet. Never deleted automatically.",
  },
  invalid: {
    label: "Invalid",
    tone: "bad",
    what: "Incomplete or damaged. It cannot be made live.",
  },
};

/** The word on the activate button: going back to an older one is a rollback. */
export function activateLabel(g: Generation, live: Generation | undefined): string {
  if (g.status === "previous") return "Roll back to this";
  if (live && g.created_at < live.created_at) return "Roll back to this";
  return "Make live";
}

/** Bytes two or more generations share (terrain reused by hard link), counted once on disk. */
export function sharedBytes(disk: GenerationList["disk"]): number {
  if (!disk) return 0;
  return Math.max(0, disk.sum_bytes - disk.bytes);
}

export function sizeOf(g: Generation): number {
  return g.bytes ?? g.manifest.sizes.total;
}

/** Why a generation cannot be deleted or activated, when the server says it cannot. */
export function whyNot(g: Generation, action: "activate" | "delete"): string | undefined {
  if (action === "delete") {
    if (g.can_delete) return undefined;
    if (g.status === "live")
      return "The live generation cannot be deleted. Make another one live first.";
    if (g.status === "building") return "A build is writing this generation.";
    return "This server cannot delete generations.";
  }
  if (g.can_activate) return undefined;
  if (g.status === "live") return "Already live.";
  if (g.status === "building") return "Not finished yet.";
  if (g.status === "invalid" || g.problems?.length) return "It is incomplete or damaged.";
  return "A build is running. Try again when it finishes.";
}
