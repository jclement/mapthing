import {
  Activity,
  CalendarClock,
  Gauge,
  KeyRound,
  Layers,
  LogOut,
  Map as MapIcon,
  Monitor,
  Moon,
  Server,
  Sun,
} from "lucide-react";
import { NavLink, Outlet, useLocation } from "react-router";
import { useEvents } from "../lib/events";
import { percent } from "../lib/format";
import { useTheme } from "../lib/theme";
import { ErrorBoundary } from "./ErrorBoundary";
import { cx } from "./ui";

const NAV = [
  { to: "/", label: "Map", icon: MapIcon, end: true },
  { to: "/regions", label: "Regions", icon: Layers },
  { to: "/builds", label: "Builds", icon: Activity },
  { to: "/schedule", label: "Schedule", icon: CalendarClock },
  { to: "/keys", label: "API keys", icon: KeyRound },
  { to: "/stats", label: "Stats", icon: Gauge },
  { to: "/system", label: "System", icon: Server },
];

function Logo() {
  return (
    <svg viewBox="0 0 32 32" className="size-7 shrink-0" aria-hidden="true">
      <rect width="32" height="32" rx="8" className="fill-accent" />
      <path d="M6 22l6-10 5 7 3-4 6 7z" className="fill-accent-fg" />
      <circle cx="22" cy="10" r="2.5" className="fill-accent-fg" />
    </svg>
  );
}

export function Layout({ onLogout }: { onLogout: () => void }) {
  const { pref, set } = useTheme();
  const { active } = useEvents();
  const loc = useLocation();
  const fullBleed = loc.pathname === "/";
  const nextTheme = pref === "system" ? "light" : pref === "light" ? "dark" : "system";
  const ThemeIcon = pref === "system" ? Monitor : pref === "light" ? Sun : Moon;

  return (
    <div className="flex h-full flex-col md:flex-row">
      {/* Sidebar (md+) */}
      <aside className="hidden w-14 shrink-0 flex-col border-r border-line bg-panel md:flex lg:w-52">
        <div className="flex h-14 items-center gap-2.5 px-3.5">
          <Logo />
          <span className="hidden text-[15px] font-semibold tracking-tight lg:inline">
            mapthing
          </span>
        </div>
        <nav className="flex-1 space-y-0.5 px-2 py-2">
          {NAV.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              title={label}
              className={({ isActive }) =>
                cx(
                  "flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[13px] font-medium transition-colors",
                  isActive
                    ? "bg-accent-soft text-accent-strong"
                    : "text-muted hover:bg-sunken hover:text-fg",
                )
              }
            >
              <Icon className="size-4 shrink-0" />
              <span className="hidden lg:inline">{label}</span>
              {to === "/builds" && active && (
                <span className="ml-auto hidden text-[11px] tnum text-accent lg:inline">
                  {active.status === "running" ? percent(active.progress) : "queued"}
                </span>
              )}
            </NavLink>
          ))}
        </nav>
        {active && (
          <NavLink
            to="/builds"
            className="mx-2 mb-2 hidden rounded-lg border border-line bg-sunken p-2.5 text-xs lg:block"
          >
            <div className="flex items-center justify-between">
              <span className="font-medium">Building</span>
              <span className="tnum text-muted">{percent(active.progress)}</span>
            </div>
            <div className="mt-1.5 h-1 overflow-hidden rounded-full bg-line">
              <div
                className="progress-live h-full bg-accent"
                style={{ width: percent(active.progress) }}
              />
            </div>
            <div className="mt-1 truncate text-faint">
              {active.stages.find((s) => s.status === "running")?.name ?? "waiting"}
            </div>
          </NavLink>
        )}
        <div className="flex items-center gap-1 border-t border-line p-2 lg:justify-between">
          <button
            type="button"
            onClick={() => set(nextTheme)}
            title={`Theme: ${pref}`}
            aria-label={`Theme: ${pref}`}
            className="grid size-8 place-items-center rounded-lg text-muted hover:bg-sunken hover:text-fg"
          >
            <ThemeIcon className="size-4" />
          </button>
          <button
            type="button"
            onClick={onLogout}
            title="Sign out"
            aria-label="Sign out"
            className="hidden size-8 place-items-center rounded-lg text-muted hover:bg-sunken hover:text-fg lg:grid"
          >
            <LogOut className="size-4" />
          </button>
        </div>
      </aside>

      {/* Mobile top bar */}
      <header className="flex h-12 shrink-0 items-center justify-between border-b border-line bg-panel px-3 md:hidden">
        <div className="flex items-center gap-2">
          <Logo />
          <span className="font-semibold tracking-tight">mapthing</span>
          {active && (
            <NavLink to="/builds" className="ml-2 text-xs tnum text-accent">
              building {percent(active.progress)}
            </NavLink>
          )}
        </div>
        <div className="flex items-center">
          <button
            type="button"
            onClick={() => set(nextTheme)}
            aria-label={`Theme: ${pref}`}
            className="grid size-8 place-items-center rounded-lg text-muted"
          >
            <ThemeIcon className="size-4" />
          </button>
          <button
            type="button"
            onClick={onLogout}
            aria-label="Sign out"
            className="grid size-8 place-items-center rounded-lg text-muted"
          >
            <LogOut className="size-4" />
          </button>
        </div>
      </header>

      <main className={cx("relative min-h-0 min-w-0 flex-1", !fullBleed && "bg-bg")}>
        <ErrorBoundary key={loc.pathname}>
          <Outlet />
        </ErrorBoundary>
      </main>

      {/* Mobile bottom nav */}
      <nav className="flex shrink-0 justify-around border-t border-line bg-panel pb-[env(safe-area-inset-bottom)] md:hidden">
        {NAV.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            aria-label={label}
            className={({ isActive }) =>
              cx(
                "flex flex-1 flex-col items-center gap-0.5 py-1.5 text-[10px]",
                isActive ? "text-accent" : "text-faint",
              )
            }
          >
            <Icon className="size-[18px]" />
            <span className="truncate">{label.replace("API keys", "Keys")}</span>
          </NavLink>
        ))}
      </nav>
    </div>
  );
}
