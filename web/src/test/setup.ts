import "@testing-library/jest-dom/vitest";
import { cleanup, configure } from "@testing-library/react";
import { afterEach, vi } from "vitest";

// jsdom has no WebGL: replace the map with a placeholder in every test.
vi.mock("../components/MapCanvas", () => ({
  MapCanvas: () => null,
  blankStyle: () => ({ version: 8, sources: {}, layers: [] }),
}));

// findBy*/waitFor poll until this deadline. Nothing in the tests waits on a real timer
// (see util.tsx), so this only absorbs a slow, loaded machine; it never hides a hang for long.
configure({ asyncUtilTimeout: 10_000 });

afterEach(() => cleanup());
