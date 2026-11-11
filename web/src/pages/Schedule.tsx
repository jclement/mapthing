import { CalendarClock, RefreshCw, Save, Settings2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router";
import { api, type RefreshMode, type RefreshSetting, type Region, type Schedule } from "../api";
import {
  Badge,
  Button,
  Card,
  cx,
  Empty,
  ErrorNote,
  Field,
  Input,
  Modal,
  Page,
  PageHeader,
  Segmented,
  Spinner,
  Switch,
} from "../components/ui";
import { CRON_PRESETS, presetFor, previewCron } from "../lib/cron";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import {
  lastCheck,
  MODE_HELP,
  MODE_LABEL,
  nextRefresh,
  policySummary,
  refreshCost,
  validWindow,
  windowWords,
} from "../lib/refresh";
import { useApi } from "../lib/useApi";

type PresetId = (typeof CRON_PRESETS)[number]["id"] | "custom";

export function CronEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [mode, setMode] = useState<PresetId>(() => presetFor(value) as PresetId);
  const preview = useMemo(() => previewCron(value), [value]);
  return (
    <div className="space-y-3">
      <Segmented<PresetId>
        label="Frequency"
        value={mode}
        onChange={(m) => {
          setMode(m);
          const p = CRON_PRESETS.find((x) => x.id === m);
          if (p) onChange(p.cron);
        }}
        options={[
          ...CRON_PRESETS.map((p) => ({ value: p.id as PresetId, label: p.label })),
          { value: "custom", label: "Custom" },
        ]}
      />
      <Field
        label="Cron expression (UTC)"
        hint="minute hour day-of-month month day-of-week"
        error={preview.ok ? undefined : preview.error}
      >
        {(id) => (
          <Input
            id={id}
            value={value}
            className="font-mono"
            spellCheck={false}
            onChange={(e) => {
              setMode("custom");
              onChange(e.target.value);
            }}
          />
        )}
      </Field>
      {preview.ok && (
        <div className="rounded-lg bg-sunken px-3 py-2.5" data-testid="cron-preview">
          <div className="text-sm font-medium">{preview.description}</div>
          <div className="mt-1.5 text-xs text-muted">Next runs:</div>
          <ul className="tnum mt-0.5 space-y-0.5 text-xs">
            {preview.next.map((d) => (
              <li key={d.toISOString()}>
                {d.toISOString().replace("T", " ").slice(0, 16)} UTC{" "}
                <span className="text-faint">
                  (
                  {d.toLocaleString(undefined, {
                    weekday: "short",
                    hour: "2-digit",
                    minute: "2-digit",
                  })}{" "}
                  local, {fmt.relTime(d.toISOString())})
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

/** "HH:MM-HH:MM" as two time inputs, with a switch for "any time". */
export function WindowEditor({
  value,
  onChange,
  anyLabel,
}: {
  value: string;
  onChange: (v: string) => void;
  anyLabel: string;
}) {
  const [from, till] = value ? value.split("-") : ["22:00", "06:00"];
  const pad = (t: string) => (t.length === 4 ? `0${t}` : t);
  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-4">
        <div>
          <div className="text-sm font-medium">Build window</div>
          <div className="text-xs text-muted">
            {value ? "A refresh that comes due outside these hours waits for them." : anyLabel}
          </div>
        </div>
        <Switch
          checked={!!value}
          onChange={(on) => onChange(on ? "22:00-06:00" : "")}
          label="Limit builds to certain hours"
        />
      </div>
      {value && (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted">Only start builds between</span>
          <input
            type="time"
            aria-label="Window opens (UTC)"
            value={pad(from)}
            onChange={(e) => e.target.value && onChange(`${e.target.value}-${pad(till)}`)}
            className="h-8 rounded-lg border border-line-strong bg-panel px-2 text-[13px] text-fg"
          />
          <span className="text-muted">and</span>
          <input
            type="time"
            aria-label="Window closes (UTC)"
            value={pad(till)}
            onChange={(e) => e.target.value && onChange(`${pad(from)}-${e.target.value}`)}
            className="h-8 rounded-lg border border-line-strong bg-panel px-2 text-[13px] text-fg"
          />
          <span className="text-muted">UTC</span>
          {!validWindow(value) && (
            <span className="text-xs text-bad">The two times must differ.</span>
          )}
        </div>
      )}
    </div>
  );
}

const MODES: RefreshMode[] = ["default", "daily", "weekly", "monthly", "cron", "never"];

function RefreshDialog({
  region,
  defaults,
  onClose,
  onSaved,
}: {
  region: Region;
  defaults: Schedule;
  onClose: () => void;
  onSaved: (r: Region) => void;
}) {
  const p = region.refresh_policy;
  const [mode, setMode] = useState<RefreshMode>(p?.mode ?? "default");
  const [cron, setCron] = useState(p?.cron || "0 2 * * 1-5");
  const [window, setWindow] = useState(p?.window ?? "");
  const [onlyChanged, setOnlyChanged] = useState(p?.only_changed ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const own = mode !== "default" && mode !== "never";
  const valid = (mode !== "cron" || previewCron(cron).ok) && validWindow(window);

  async function save() {
    setBusy(true);
    setError(undefined);
    const body: RefreshSetting = { mode };
    if (mode === "cron") body.cron = cron.trim();
    if (own) {
      body.window = window;
      body.only_changed = onlyChanged;
    }
    try {
      onSaved(await api.setRegionRefresh(region.id, body));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={`Refresh ${region.name}`}
      size="lg"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!valid} onClick={save}>
            Save
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        {error && <ErrorNote error={error} />}
        <fieldset>
          <legend className="mb-2 text-xs font-medium text-muted">How often</legend>
          <div className="grid gap-2 sm:grid-cols-2">
            {MODES.map((m) => (
              <label
                key={m}
                className={cx(
                  "flex cursor-pointer gap-2.5 rounded-lg border px-3 py-2",
                  mode === m ? "border-accent/50 bg-accent-soft/50" : "border-line hover:bg-sunken",
                )}
              >
                <input
                  type="radio"
                  name="refresh-mode"
                  className="mt-0.5 accent-(--color-accent)"
                  checked={mode === m}
                  onChange={() => setMode(m)}
                />
                <span>
                  <span className="block text-sm font-medium">{MODE_LABEL[m]}</span>
                  <span className="block text-xs text-muted">
                    {m === "default"
                      ? defaults.enabled
                        ? `Now: ${previewCron(defaults.cron).ok ? (previewCron(defaults.cron) as { description: string }).description : defaults.cron}.`
                        : "The default schedule is off, so: not refreshed."
                      : MODE_HELP[m]}
                  </span>
                </span>
              </label>
            ))}
          </div>
        </fieldset>
        {mode === "cron" && <CronEditor value={cron} onChange={setCron} />}
        {own && (
          <>
            <WindowEditor
              value={window}
              onChange={setWindow}
              anyLabel={
                defaults.window
                  ? `Uses the default window (${windowWords(defaults.window)}).`
                  : "Builds may start at any time."
              }
            />
            <div className="flex items-center justify-between gap-4">
              <div>
                <div className="text-sm font-medium">Only when the source has something new</div>
                <div className="text-xs text-muted">
                  Off rebuilds on every schedule time, whether or not Geofabrik has published a
                  newer extract.
                </div>
              </div>
              <Switch
                checked={onlyChanged}
                onChange={setOnlyChanged}
                label="Only when the source has something new"
              />
            </div>
          </>
        )}
        <p className="rounded-lg bg-sunken px-3 py-2 text-xs text-muted">
          A refresh downloads this region's extract again ({refreshCost(region.refresh_cost)}). The
          map and the routing graph are always built over all regions together, so routes cross
          region borders; regions that are not due keep their download, and the elevation tiles are
          reused.
        </p>
      </div>
    </Modal>
  );
}

export function SchedulePage() {
  const { generationTick } = useEvents();
  const s = useApi(() => api.schedule(), [generationTick]);
  const regions = useApi(() => api.regions(), [generationTick], 60000);
  const [draft, setDraft] = useState<Schedule>();
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string>();
  const [editing, setEditing] = useState<Region>();

  useEffect(() => {
    if (s.data) setDraft(s.data);
  }, [s.data]);

  const same = (a: Schedule, b: Schedule) =>
    a.cron === b.cron &&
    a.enabled === b.enabled &&
    (a.window ?? "") === (b.window ?? "") &&
    (a.only_changed ?? true) === (b.only_changed ?? true);
  const dirty = draft && s.data && !same(draft, s.data);
  const valid = draft ? previewCron(draft.cron).ok && validWindow(draft.window ?? "") : false;

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(undefined);
    try {
      const out = await api.saveSchedule({
        cron: draft.cron,
        enabled: draft.enabled,
        refresh_downloads: true,
        window: draft.window ?? "",
        only_changed: draft.only_changed ?? true,
        regions: [],
      });
      s.setData(() => out);
      regions.reload();
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function check() {
    setChecking(true);
    setError(undefined);
    try {
      const out = await api.checkRegions();
      regions.setData(() => out);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }

  const list = regions.data?.regions ?? [];
  const history = s.data?.history ?? [];

  return (
    <Page>
      <PageHeader
        title="Schedule"
        description="When each region's data is refreshed from Geofabrik and the map rebuilt. Every region can have its own setting; the default covers the rest. Times are UTC."
        actions={
          <Button
            variant="primary"
            icon={<Save className="size-4" />}
            disabled={!dirty || !valid}
            loading={saving}
            onClick={save}
          >
            {saved ? "Saved" : "Save default"}
          </Button>
        }
      />
      {error && (
        <div className="mb-4">
          <ErrorNote error={error} />
        </div>
      )}
      {!draft ? (
        s.error ? (
          <ErrorNote error={s.error} onRetry={s.reload} />
        ) : (
          <Spinner />
        )
      ) : (
        <div className="space-y-5">
          <Card
            title={
              <span className="flex items-center gap-2">
                <CalendarClock className="size-4 text-faint" /> Default schedule
              </span>
            }
            action={
              <span className="text-xs text-faint">for regions without a setting of their own</span>
            }
          >
            <div className="grid gap-x-8 gap-y-5 lg:grid-cols-2">
              <div className="space-y-5">
                <div className="flex items-center justify-between gap-4">
                  <div>
                    <div className="text-sm font-medium">Scheduled refreshes</div>
                    <div className="text-xs text-muted">
                      When off, regions on the default are refreshed only when you build.
                    </div>
                  </div>
                  <Switch
                    checked={draft.enabled}
                    onChange={(enabled) => setDraft({ ...draft, enabled })}
                    label="Scheduled refreshes"
                  />
                </div>
                <div className={draft.enabled ? undefined : "pointer-events-none opacity-50"}>
                  <CronEditor
                    value={draft.cron}
                    onChange={(cron) => setDraft({ ...draft, cron })}
                  />
                </div>
              </div>
              <div className="space-y-5">
                <WindowEditor
                  value={draft.window ?? ""}
                  onChange={(window) => setDraft({ ...draft, window })}
                  anyLabel="Builds may start at any time."
                />
                <div className="flex items-center justify-between gap-4 border-t border-line pt-4">
                  <div>
                    <div className="text-sm font-medium">
                      Only when the source has something new
                    </div>
                    <div className="text-xs text-muted">
                      At each schedule time the regions that are due are checked against Geofabrik.
                      If none has a newer extract, "nothing new" is recorded and no build runs.
                    </div>
                  </div>
                  <Switch
                    checked={draft.only_changed ?? true}
                    onChange={(only_changed) => setDraft({ ...draft, only_changed })}
                    label="Only when the source has something new"
                  />
                </div>
              </div>
            </div>
          </Card>

          <Card
            title="Regions"
            pad={false}
            action={
              <Button
                size="sm"
                icon={<RefreshCw className="size-3.5" />}
                loading={checking}
                disabled={list.length === 0}
                onClick={check}
                title="Ask Geofabrik now whether each region has newer data. Builds nothing."
              >
                Check sources now
              </Button>
            }
          >
            {regions.error && !regions.data ? (
              <div className="p-4">
                <ErrorNote error={regions.error} onRetry={regions.reload} />
              </div>
            ) : !regions.data ? (
              <Spinner />
            ) : list.length === 0 ? (
              <Empty
                title="No regions yet"
                action={
                  <Link
                    to="/regions?add=1"
                    className="text-sm font-medium text-accent-strong hover:underline"
                  >
                    Add a region
                  </Link>
                }
              >
                Each region you add gets its own refresh setting here.
              </Empty>
            ) : (
              <ul className="divide-y divide-line">
                {list.map((r) => (
                  <RegionRow key={r.id} r={r} onEdit={() => setEditing(r)} />
                ))}
              </ul>
            )}
            <p className="border-t border-line px-4 py-3 text-xs text-faint">
              Whichever regions are due, a build makes the map and the routing graph over all
              regions together, so tiles and routes cross region borders. What a region's setting
              decides is when its data is downloaded again. Regions that are not due keep their
              download, and the elevation tiles are reused, so they cost little.
            </p>
          </Card>

          <Card title="Recent decisions">
            {history.length === 0 ? (
              <p className="text-sm text-muted">
                Nothing yet. Each time a region comes due, what was found and done is recorded here.
              </p>
            ) : (
              <ul className="space-y-2">
                {history.slice(0, 8).map((h) => (
                  <li key={h.id} className="flex flex-wrap items-baseline gap-x-3 text-[13px]">
                    <span className="tnum shrink-0 text-xs text-faint">{fmt.dateTime(h.at)}</span>
                    <span className={h.ok ? undefined : "text-bad"}>{h.message}</span>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      )}
      {editing && draft && (
        <RefreshDialog
          region={editing}
          defaults={s.data ?? draft}
          onClose={() => setEditing(undefined)}
          onSaved={(out) => {
            regions.setData((d) =>
              d ? { regions: d.regions.map((x) => (x.id === out.id ? out : x)) } : d,
            );
            setEditing(undefined);
          }}
        />
      )}
    </Page>
  );
}

function RegionRow({ r, onEdit }: { r: Region; onEdit: () => void }) {
  const p = r.refresh_policy;
  const age = fmt.ageDays(r.data_date);
  return (
    <li
      aria-label={r.name}
      className="grid gap-x-4 gap-y-1 px-4 py-3 sm:grid-cols-[minmax(0,14rem)_minmax(0,1fr)_auto] sm:items-center"
    >
      <div className="min-w-0">
        <div className="truncate text-sm font-medium">{r.name}</div>
        <div className="text-xs text-faint">
          {r.data_date ? (
            <>
              data of {fmt.date(r.data_date)}{" "}
              <span className={cx(age != null && age > 14 && "text-warn")}>
                ({fmt.relTime(r.data_date)})
              </span>
            </>
          ) : (
            "not downloaded yet"
          )}
        </div>
      </div>
      <div className="min-w-0 text-[13px]">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span>{policySummary(p)}</span>
          {p?.mode !== "never" && p?.effective.cron && !p.effective.only_changed && (
            <Badge>even if unchanged</Badge>
          )}
        </div>
        <dl className="mt-0.5 flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-muted">
          <div>
            <dt className="inline text-faint">Next refresh: </dt>
            <dd className="inline" title={p?.next_build ? fmt.dateTime(p.next_build) : undefined}>
              {nextRefresh(p)}
            </dd>
          </div>
          <div>
            <dt className="inline text-faint">Last check: </dt>
            <dd
              className={cx("inline", p?.last_check === "error" && "text-bad")}
              title={p?.last_check_detail}
            >
              {lastCheck(p)}
            </dd>
          </div>
          {p?.mode !== "never" && (
            <div>
              <dt className="inline text-faint">A refresh costs: </dt>
              <dd className="inline">{refreshCost(r.refresh_cost)}</dd>
            </div>
          )}
        </dl>
      </div>
      <Button
        size="sm"
        icon={<Settings2 className="size-3.5" />}
        onClick={onEdit}
        aria-label={`Refresh settings for ${r.name}`}
      >
        Change
      </Button>
    </li>
  );
}
