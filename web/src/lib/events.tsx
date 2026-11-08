import { createContext, type ReactNode, useContext, useEffect, useMemo, useState } from "react";
import { api, type Job, type LogLine, subscribeEvents } from "../api";

interface EventsState {
  /** Latest known state of every job seen (from the initial list and the SSE stream). */
  jobs: Record<string, Job>;
  /** Log tail per job id (most recent last, capped). */
  logs: Record<string, LogLine[]>;
  /** Bumps whenever a new generation goes live, so pages can refetch. */
  generationTick: number;
  connected: boolean;
  /** The running job if any, else the oldest queued one. */
  active: Job | undefined;
}

const Ctx = createContext<EventsState>({
  jobs: {},
  logs: {},
  generationTick: 0,
  connected: false,
  active: undefined,
});

const LOG_CAP = 500;

export function EventsProvider({ children }: { children: ReactNode }) {
  const [jobs, setJobs] = useState<Record<string, Job>>({});
  const [logs, setLogs] = useState<Record<string, LogLine[]>>({});
  const [generationTick, setGenerationTick] = useState(0);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    let live = true;
    api
      .jobs()
      .then(({ jobs: list }) => {
        if (!live) return;
        setJobs((cur) => {
          const next = { ...cur };
          for (const j of list) if (!next[j.id]) next[j.id] = j;
          return next;
        });
      })
      .catch(() => {});
    const off = subscribeEvents((e) => {
      setConnected(true);
      if (e.type === "job") setJobs((cur) => ({ ...cur, [e.data.id]: e.data }));
      else if (e.type === "log")
        setLogs((cur) => {
          const prev = cur[e.data.job_id] ?? [];
          const next = prev.length >= LOG_CAP ? prev.slice(-LOG_CAP + 1) : prev.slice();
          next.push(e.data);
          return { ...cur, [e.data.job_id]: next };
        });
      else if (e.type === "generation") setGenerationTick((t) => t + 1);
    });
    return () => {
      live = false;
      off();
    };
  }, []);

  const value = useMemo<EventsState>(() => {
    const all = Object.values(jobs);
    const active =
      all.find((j) => j.status === "running") ??
      all
        .filter((j) => j.status === "queued")
        .sort((a, b) => a.created_at.localeCompare(b.created_at))[0];
    return { jobs, logs, generationTick, connected, active };
  }, [jobs, logs, generationTick, connected]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useEvents() {
  return useContext(Ctx);
}
