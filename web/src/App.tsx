import { useCallback, useEffect, useState } from "react";
import { BrowserRouter, Navigate, Route, Routes } from "react-router";
import { ApiError, api, setUnauthorizedHandler } from "./api";
import { Layout } from "./components/Layout";
import { Spinner } from "./components/ui";
import { EventsProvider } from "./lib/events";
import { BuildsPage } from "./pages/Builds";
import { KeysPage } from "./pages/Keys";
import { LoginPage } from "./pages/Login";
import { MapPage } from "./pages/Map";
import { RegionsPage } from "./pages/Regions";
import { SchedulePage } from "./pages/Schedule";
import { StatsPage } from "./pages/Stats";
import { SystemPage } from "./pages/System";

type Auth = "checking" | "in" | "out";

/** Everything below the router; tests render this inside a MemoryRouter. */
export function AppShell() {
  const [auth, setAuth] = useState<Auth>("checking");

  useEffect(() => {
    setUnauthorizedHandler(() => setAuth("out"));
    api
      .system()
      .then(() => setAuth("in"))
      .catch((e) => setAuth(e instanceof ApiError && e.status === 401 ? "out" : "in"));
    return () => setUnauthorizedHandler(null);
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setAuth("out");
    }
  }, []);

  if (auth === "checking") return <Spinner label="Connecting" />;
  if (auth === "out") return <LoginPage onLogin={() => setAuth("in")} />;

  return (
    <EventsProvider>
      <Routes>
        <Route element={<Layout onLogout={logout} />}>
          <Route index element={<MapPage />} />
          <Route path="regions" element={<RegionsPage />} />
          <Route path="builds" element={<BuildsPage />} />
          <Route path="schedule" element={<SchedulePage />} />
          <Route path="keys" element={<KeysPage />} />
          <Route path="stats" element={<StatsPage />} />
          <Route path="system" element={<SystemPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </EventsProvider>
  );
}

export function App() {
  return (
    <BrowserRouter basename="/admin">
      <AppShell />
    </BrowserRouter>
  );
}
