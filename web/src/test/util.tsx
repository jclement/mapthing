import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { AppShell } from "../App";
import { installMock, type MockOptions } from "../mocks/server";

export function renderApp(path = "/", opts: MockOptions = { authed: true }) {
  installMock({ tickMs: 20, ...opts });
  const user = userEvent.setup();
  const utils = render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell />
    </MemoryRouter>,
  );
  return { user, ...utils };
}
