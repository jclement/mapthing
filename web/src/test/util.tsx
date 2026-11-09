import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { AppShell } from "../App";
import { installMock, type MockOptions } from "../mocks/server";

export function renderApp(path = "/", opts: MockOptions = { authed: true }) {
  // Deterministic backend: no network jitter, and builds advance only when a test calls
  // finishBuilds(), so nothing races the assertions however loaded the machine is.
  installMock({ tickMs: 0, latencyMs: 0, ...opts });
  const user = userEvent.setup();
  const utils = render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell />
    </MemoryRouter>,
  );
  return { user, ...utils };
}
