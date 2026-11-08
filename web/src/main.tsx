import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./index.css";

async function boot() {
  // Mock backend for development without a server. VITE_MOCK is a build-time constant,
  // so this import (and every fixture) is dropped from the production bundle.
  if (import.meta.env.VITE_MOCK) {
    const { installMock } = await import("./mocks/server");
    const q = new URLSearchParams(location.search);
    installMock({ empty: q.has("empty"), authed: q.has("authed") || !q.has("login") });
  }
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}

boot();
