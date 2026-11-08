import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";

// jsdom has no WebGL: replace the map with a placeholder in every test.
vi.mock("../components/MapCanvas", () => ({
  MapCanvas: () => null,
  blankStyle: () => ({ version: 8, sources: {}, layers: [] }),
}));

afterEach(() => cleanup());
