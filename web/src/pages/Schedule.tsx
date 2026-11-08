import { CalendarClock, Save } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { api, type Schedule } from "../api";
import {
  Button,
  Card,
  ErrorNote,
  Field,
  Input,
  Page,
  PageHeader,
  Segmented,
  Spinner,
  Switch,
} from "../components/ui";
import { CRON_PRESETS, presetFor, previewCron } from "../lib/cron";
import * as fmt from "../lib/format";
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

export function SchedulePage() {
  const s = useApi(() => api.schedule(), []);
  const regions = useApi(() => api.regions(), []);
  const [draft, setDraft] = useState<Schedule>();
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    if (s.data) setDraft(s.data);
  }, [s.data]);

  const dirty = draft && s.data && JSON.stringify(draft) !== JSON.stringify(s.data);
  const valid = draft ? previewCron(draft.cron).ok : false;

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(undefined);
    try {
      const out = await api.saveSchedule(draft);
      s.setData(() => out);
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Page>
      <PageHeader
        title="Schedule"
        description="Refresh downloads from Geofabrik and rebuild on a timer. Times are UTC."
        actions={
          <Button
            variant="primary"
            icon={<Save className="size-4" />}
            disabled={!dirty || !valid}
            loading={saving}
            onClick={save}
          >
            {saved ? "Saved" : "Save"}
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
        <div className="grid gap-5 lg:grid-cols-[1fr_340px]">
          <Card
            title={
              <span className="flex items-center gap-2">
                <CalendarClock className="size-4 text-faint" /> When
              </span>
            }
          >
            <div className="space-y-5">
              <div className="flex items-center justify-between gap-4">
                <div>
                  <div className="text-sm font-medium">Scheduled builds</div>
                  <div className="text-xs text-muted">
                    When off, builds only run when you start them.
                  </div>
                </div>
                <Switch
                  checked={draft.enabled}
                  onChange={(enabled) => setDraft({ ...draft, enabled })}
                  label="Scheduled builds"
                />
              </div>
              <div className={draft.enabled ? undefined : "pointer-events-none opacity-50"}>
                <CronEditor value={draft.cron} onChange={(cron) => setDraft({ ...draft, cron })} />
              </div>
              <div className="flex items-center justify-between gap-4 border-t border-line pt-4">
                <div>
                  <div className="text-sm font-medium">Refresh downloads</div>
                  <div className="text-xs text-muted">
                    Fetch fresh extracts first. Off rebuilds from what is already downloaded.
                  </div>
                </div>
                <Switch
                  checked={draft.refresh_downloads}
                  onChange={(refresh_downloads) => setDraft({ ...draft, refresh_downloads })}
                  label="Refresh downloads"
                />
              </div>
            </div>
          </Card>
          <Card title="Regions to refresh">
            {regions.data?.regions.length ? (
              <ul className="space-y-2.5">
                {regions.data.regions.map((r) => {
                  const on = draft.regions.find((x) => x.id === r.id)?.enabled ?? true;
                  return (
                    <li key={r.id} className="flex items-center justify-between gap-3 text-sm">
                      <div className="min-w-0">
                        <div className="truncate">{r.name}</div>
                        <div className="text-xs text-faint">data {fmt.relTime(r.data_date)}</div>
                      </div>
                      <Switch
                        checked={on}
                        label={`Refresh ${r.name}`}
                        onChange={(enabled) =>
                          setDraft({
                            ...draft,
                            regions: draft.regions.some((x) => x.id === r.id)
                              ? draft.regions.map((x) => (x.id === r.id ? { ...x, enabled } : x))
                              : [...draft.regions, { id: r.id, enabled }],
                          })
                        }
                      />
                    </li>
                  );
                })}
              </ul>
            ) : (
              <p className="text-sm text-muted">No regions installed.</p>
            )}
            <p className="mt-4 text-xs text-faint">
              Regions switched off keep their current download and are still built into every
              generation.
            </p>
          </Card>
        </div>
      )}
    </Page>
  );
}
