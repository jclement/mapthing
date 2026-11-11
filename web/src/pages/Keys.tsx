import { KeyRound, Plus, ShieldAlert, Trash2 } from "lucide-react";
import { type FormEvent, useEffect, useState } from "react";
import {
  type ApiKey,
  api,
  type CreatedKey,
  DEFAULT_STYLE,
  keySnippets,
  type LimitScope,
  type LimitsConfig,
  type StyleInfo,
} from "../api";
import { Sparkline, StackedBars } from "../components/charts";
import {
  Badge,
  Button,
  Card,
  Confirm,
  CopyButton,
  Empty,
  ErrorNote,
  Field,
  Input,
  Modal,
  Page,
  PageHeader,
  Progress,
  Spinner,
} from "../components/ui";
import * as fmt from "../lib/format";
import {
  EMPTY_FORM,
  exact,
  formFromLimits,
  formToInput,
  KIND_NAME,
  type LimitsForm,
  limitsSummary,
  SCOPE_NAME,
  share,
  shareTone,
  unitsExample,
} from "../lib/limits";
import { useApi } from "../lib/useApi";

function Limits({ k }: { k: ApiKey }) {
  const parts = limitsSummary(k.limits);
  return (
    <div className="space-y-0.5">
      <div className={parts.length ? undefined : "text-faint"}>
        {parts.length ? parts.join(" · ") : "Unlimited"}
      </div>
      {k.limits.origins.length > 0 && (
        <div className="truncate text-xs text-faint" title={k.limits.origins.join(", ")}>
          {k.limits.origins.join(", ")}
        </div>
      )}
    </div>
  );
}

/** The limit fields, shared by the create form and the edit form. */
function LimitFields({
  form,
  onChange,
  weights,
}: {
  form: LimitsForm;
  onChange: (f: LimitsForm) => void;
  weights?: Record<string, number>;
}) {
  const set = (k: keyof LimitsForm) => (e: { target: { value: string } }) =>
    onChange({ ...form, [k]: e.target.value });
  const num = (k: keyof LimitsForm, id: string, step?: string) => (
    <Input
      id={id}
      type="number"
      min={0}
      step={step}
      inputMode="decimal"
      value={form[k]}
      onChange={set(k)}
    />
  );
  const daily = Number(form.daily);
  return (
    <div className="space-y-4">
      <p className="text-xs text-muted">
        Limits count <strong>units</strong>, not requests: a vector tile costs {weights?.tile ?? 1},
        a raster tile {weights?.raster ?? 10}, a route {weights?.route ?? 10}. Leave a field empty
        for no limit.
      </p>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Rate (units per second)" hint="Sustained">
          {(id) => num("rps", id, "any")}
        </Field>
        <Field label="Burst (units)" hint="Empty = two seconds' worth">
          {(id) => num("burst", id)}
        </Field>
        <Field
          label="Daily quota (units)"
          hint={daily > 0 ? unitsExample(daily, weights) : "Resets at midnight UTC"}
        >
          {(id) => num("daily", id)}
        </Field>
        <Field label="Monthly quota (units)" hint="Resets on the 1st, UTC">
          {(id) => num("monthly", id)}
        </Field>
        <Field label="Per-client rate (units per second)" hint="Each address using the key">
          {(id) => num("ipRps", id, "any")}
        </Field>
        <Field label="Per-client burst (units)" hint="Empty = two seconds' worth">
          {(id) => num("ipBurst", id)}
        </Field>
      </div>
      <Field
        label="Allowed origins"
        hint="Browser origins allowed to use the key, comma separated. Empty = any."
      >
        {(id) => (
          <Input
            id={id}
            value={form.origins}
            placeholder="https://app.example.com"
            onChange={set("origins")}
          />
        )}
      </Field>
    </div>
  );
}

function Meter({
  label,
  used,
  of,
  note,
}: {
  label: string;
  used: number;
  of: number;
  note?: string;
}) {
  const x = share(used, of);
  return (
    <div>
      <div className="mb-1 flex items-baseline justify-between gap-3 text-xs">
        <span className="text-muted">{label}</span>
        <span className="tnum">
          {exact(used)} <span className="text-faint">of {exact(of)}</span>
        </span>
      </div>
      <Progress value={x} tone={shareTone(x)} />
      {note && <div className="mt-1 text-xs text-faint">{note}</div>}
    </div>
  );
}

const CHART_SERIES = [
  { key: "served", label: "Served", color: "var(--color-k1)" },
  { key: "refused", label: "Refused (429)", color: "var(--color-bad)" },
];

/** One key: usage against each limit, what was refused, and its limits to edit. */
function KeyDetails({
  k,
  weights,
  onClose,
  onSaved,
}: {
  k: ApiKey | null;
  weights?: Record<string, number>;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [form, setForm] = useState<LimitsForm>(EMPTY_FORM);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [saved, setSaved] = useState(false);
  const id = k?.id;
  // biome-ignore lint/correctness/useExhaustiveDependencies: the form restarts when another key is opened, not on every reload of the list
  useEffect(() => {
    if (k) setForm(formFromLimits(k.limits));
    setError(undefined);
    setSaved(false);
  }, [id]);
  if (!k) return null;
  const l = k.limits;
  const u = k.usage;

  async function save(e: FormEvent) {
    e.preventDefault();
    if (!k) return;
    const out = formToInput(form);
    if ("error" in out) {
      setError(out.error);
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.updateKey(k.id, out.input);
      setSaved(true);
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const days = u.daily.map((n, i) => {
    const d = new Date(Date.now() - (u.daily.length - 1 - i) * 86400000);
    const refused = u.limited_daily?.[i] ?? 0;
    return {
      day: String(d.getUTCDate()), // day of the month: 14 of them fit a phone
      served: Math.max(0, n - refused),
      refused,
    };
  });
  const scopes = Object.entries(u.limited_scopes ?? {}).filter(([, n]) => (n ?? 0) > 0) as [
    LimitScope,
    number,
  ][];
  const recent = [...(u.recent_limited ?? [])].reverse().slice(0, 8);
  const limited = !!(l.rps || l.daily_quota || l.monthly_quota || l.ip_rps);

  return (
    <Modal
      open
      onClose={onClose}
      title={k.label}
      size="lg"
      footer={
        <>
          <Button onClick={onClose}>Close</Button>
          <Button variant="primary" type="submit" form="edit-limits" loading={busy}>
            Save limits
          </Button>
        </>
      }
    >
      <section aria-label="Usage against limits" className="space-y-3">
        <h3 className="text-xs font-semibold tracking-wide text-faint uppercase">Usage</h3>
        {!limited && (
          <p className="text-sm text-muted">
            This key has no limits. {exact(u.today)} requests today, {exact(u.units_today ?? 0)}{" "}
            units.
          </p>
        )}
        {l.daily_quota ? (
          <Meter
            label="Today (units, UTC day)"
            used={u.units_today ?? 0}
            of={l.daily_quota}
            note={`${exact(u.today)} requests`}
          />
        ) : null}
        {l.monthly_quota ? (
          <Meter label="This month (units)" used={u.units_month ?? 0} of={l.monthly_quota} />
        ) : null}
        {l.rps && l.burst ? (
          <Meter
            label="Burst in use right now (units)"
            used={Math.max(0, l.burst - (u.rate_remaining ?? l.burst))}
            of={l.burst}
            note={`Refills at ${l.rps} a second`}
          />
        ) : null}
        <div className="pt-1">
          <div className="mb-1 text-xs text-muted">Requests, last 14 days</div>
          <StackedBars
            rows={days}
            series={CHART_SERIES}
            height={150}
            xLabel={(r) => String(r.day)}
          />
        </div>
      </section>

      <section aria-label="Refused requests" className="mt-5 space-y-2">
        <h3 className="text-xs font-semibold tracking-wide text-faint uppercase">
          Refused with 429
        </h3>
        {(u.limited_today ?? 0) === 0 ? (
          <p className="text-sm text-muted">None today.</p>
        ) : (
          <>
            <p className="text-sm">
              <span className="tnum font-medium">{exact(u.limited_today ?? 0)}</span> today:{" "}
              {scopes.map(([sc, n]) => `${exact(n)} ${SCOPE_NAME[sc] ?? sc}`).join("; ")}.
            </p>
            {recent.length > 0 && (
              <ul className="divide-y divide-line rounded-lg border border-line text-xs">
                {recent.map((r, i) => (
                  <li key={`${r.t}-${i}`} className="flex flex-wrap gap-x-3 gap-y-0.5 px-3 py-1.5">
                    <span className="text-muted">{fmt.relTime(r.t)}</span>
                    <span>{KIND_NAME[r.kind] ?? r.kind}</span>
                    <span className="text-muted">{SCOPE_NAME[r.scope] ?? r.scope}</span>
                    {r.ip && <code className="font-mono text-faint">{r.ip}</code>}
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </section>

      <form id="edit-limits" onSubmit={save} noValidate className="mt-5 space-y-3">
        <h3 className="text-xs font-semibold tracking-wide text-faint uppercase">Limits</h3>
        <LimitFields form={form} onChange={setForm} weights={weights} />
        {error && <ErrorNote error={error} />}
        {saved && !error && (
          <p role="status" className="text-xs text-ok">
            Saved. The new limits apply to the next request.
          </p>
        )}
      </form>
    </Modal>
  );
}

/** What each kind of request costs, and the server-wide limits. */
function LimitsCard({ cfg, onSaved }: { cfg: LimitsConfig; onSaved: () => void }) {
  type Row = { weight: string; rps: string; burst: string };
  const toRows = (w: Record<string, number>, g: LimitsConfig["global"]) =>
    Object.fromEntries(
      cfg.kinds.map((k) => [
        k,
        {
          weight: String(w[k] ?? 1),
          rps: g[k]?.rps ? String(g[k].rps) : "",
          burst: g[k]?.burst ? String(g[k].burst) : "",
        },
      ]),
    ) as Record<string, Row>;
  const [rows, setRows] = useState(() => toRows(cfg.weights, cfg.global));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [saved, setSaved] = useState(false);
  const set = (k: string, f: keyof Row, v: string) => {
    setSaved(false);
    setRows((r) => ({ ...r, [k]: { ...r[k], [f]: v } }));
  };

  async function save(e: FormEvent) {
    e.preventDefault();
    const weights: Record<string, number> = {};
    const global: LimitsConfig["global"] = {};
    for (const k of cfg.kinds) {
      const w = Number(rows[k].weight);
      if (!Number.isInteger(w) || w < 1) {
        setError(`${KIND_NAME[k] ?? k}: the cost must be a whole number, 1 or more.`);
        return;
      }
      weights[k] = w;
      const rps = Number(rows[k].rps || 0);
      const burst = Number(rows[k].burst || 0);
      if (!Number.isFinite(rps) || rps < 0 || !Number.isInteger(burst) || burst < 0) {
        setError(`${KIND_NAME[k] ?? k}: the server limit must be a number, or empty.`);
        return;
      }
      if (rps > 0) global[k] = { rps, burst };
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.saveLimits({ weights, global });
      setSaved(true);
      onSaved();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const cell = "h-8 w-full min-w-16";
  return (
    <Card title="Request costs and server limits">
      <form onSubmit={save} noValidate className="space-y-3">
        <p className="text-xs text-muted">
          What one request of each kind takes from a key's limits, and how many of each this server
          answers per second across all keys. A request over a server limit gets a 429 and costs the
          key nothing.
        </p>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[520px] text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                <th className="py-2 pr-3 font-medium">Kind of request</th>
                <th className="w-24 px-2 py-2 font-medium">Cost (units)</th>
                <th className="w-28 px-2 py-2 font-medium">Server limit (per second)</th>
                <th className="w-24 py-2 pl-2 font-medium">Burst</th>
              </tr>
            </thead>
            <tbody>
              {cfg.kinds.map((k) => {
                const name = KIND_NAME[k] ?? k;
                return (
                  <tr key={k} className="border-b border-line last:border-0">
                    <td className="py-1.5 pr-3">{name}</td>
                    <td className="px-2 py-1.5">
                      <Input
                        aria-label={`Cost of ${name}`}
                        type="number"
                        min={1}
                        className={cell}
                        value={rows[k].weight}
                        onChange={(e) => set(k, "weight", e.target.value)}
                      />
                    </td>
                    <td className="px-2 py-1.5">
                      <Input
                        aria-label={`Server limit for ${name}`}
                        type="number"
                        min={0}
                        step="any"
                        placeholder="none"
                        className={cell}
                        value={rows[k].rps}
                        onChange={(e) => set(k, "rps", e.target.value)}
                      />
                    </td>
                    <td className="py-1.5 pl-2">
                      <Input
                        aria-label={`Server burst for ${name}`}
                        type="number"
                        min={0}
                        placeholder="auto"
                        className={cell}
                        value={rows[k].burst}
                        onChange={(e) => set(k, "burst", e.target.value)}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        {error && <ErrorNote error={error} />}
        <div className="flex flex-wrap items-center justify-end gap-2">
          {saved && (
            <span role="status" className="mr-auto text-xs text-ok">
              Saved. It applies to the next request.
            </span>
          )}
          <Button
            type="button"
            onClick={() => {
              setSaved(false);
              setRows(toRows(cfg.defaults.weights, cfg.defaults.global));
            }}
          >
            Reset to defaults
          </Button>
          <Button variant="primary" type="submit" loading={busy}>
            Save
          </Button>
        </div>
      </form>
    </Card>
  );
}

function CreateKey({
  open,
  onClose,
  onCreated,
  styles,
  weights,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
  styles: StyleInfo[];
  weights?: Record<string, number>;
}) {
  const [style, setStyle] = useState<string>(DEFAULT_STYLE);
  const [label, setLabel] = useState("");
  const [form, setForm] = useState<LimitsForm>(EMPTY_FORM);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [created, setCreated] = useState<CreatedKey | null>(null);

  function reset() {
    setLabel("");
    setForm(EMPTY_FORM);
    setError(undefined);
    setCreated(null);
  }

  function close() {
    // Dropping `created` here is what makes the secret show only once.
    reset();
    onClose();
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    const limits = formToInput(form);
    if ("error" in limits) {
      setError(limits.error);
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      const out = await api.createKey({ label: label.trim(), ...limits.input });
      setCreated(out);
      onCreated();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  // The address clients use: MAPTHING_PUBLIC_URL when set, else wherever this page was opened.
  const sys = useApi(() => api.system(), []);
  const origin =
    sys.data?.public_url ||
    (typeof location !== "undefined" ? location.origin : "https://maps.example.com");

  if (created) {
    return (
      <Modal
        open={open}
        onClose={close}
        title="Key created"
        size="lg"
        dismissable={false}
        footer={
          <Button variant="primary" onClick={close}>
            I have saved it
          </Button>
        }
      >
        <div className="mb-4 flex gap-2.5 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2.5 text-sm">
          <ShieldAlert className="mt-0.5 size-4 shrink-0 text-warn" />
          <span>
            Copy this key now. It is shown <strong>only once</strong>; mapthing keeps only a hash.
          </span>
        </div>
        <div className="mb-1 text-xs font-medium text-muted">{created.key.label}</div>
        <div className="flex items-center gap-2 rounded-lg border border-line-strong bg-sunken px-3 py-2">
          <code className="flex-1 font-mono text-[13px] break-all select-all" data-testid="secret">
            {created.secret}
          </code>
          <CopyButton value={created.secret} label="Copy key" />
        </div>
        <div className="mt-5 mb-2 flex flex-wrap items-center justify-between gap-2">
          <h3 className="text-xs font-semibold tracking-wide text-faint uppercase">Use it</h3>
          {styles.length > 0 && (
            <label className="flex items-center gap-2 text-xs text-muted">
              Style
              <select
                aria-label="Style for the snippets"
                value={style}
                onChange={(e) => setStyle(e.target.value)}
                className="h-7 rounded-md border border-line-strong bg-panel px-2 text-xs text-fg"
              >
                {styles.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
        <ul className="space-y-2">
          {keySnippets(origin, created.secret, style).map((s) => (
            <li key={s.label}>
              <div className="mb-0.5 text-xs text-muted">{s.label}</div>
              <div className="flex items-center gap-1 rounded-md bg-sunken px-2.5 py-1.5">
                <code className="flex-1 font-mono text-[11.5px] break-all">{s.value}</code>
                <CopyButton value={s.value} label={`Copy ${s.label}`} />
              </div>
            </li>
          ))}
        </ul>
      </Modal>
    );
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title="Create API key"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            type="submit"
            form="create-key"
            loading={busy}
            disabled={!label.trim()}
          >
            Create key
          </Button>
        </>
      }
    >
      <form id="create-key" onSubmit={submit} className="space-y-4">
        <Field label="Label" hint="Who or what uses this key, e.g. “Field app (mobile)”">
          {(id) => (
            <Input
              id={id}
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              required
              autoFocus
            />
          )}
        </Field>
        <LimitFields form={form} onChange={setForm} weights={weights} />
        {error && <ErrorNote error={error} />}
      </form>
    </Modal>
  );
}

export function KeysPage() {
  const keys = useApi(() => api.keys(), []);
  const styles = useApi(() => api.styles(), []);
  const [creating, setCreating] = useState(false);
  const limits = useApi(() => api.limits(), []);
  const [openId, setOpenId] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function revoke() {
    if (!revoking) return;
    setBusy(true);
    try {
      await api.revokeKey(revoking.id);
      setRevoking(null);
      keys.reload();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const list = keys.data?.keys ?? [];
  const opened = list.find((k) => k.id === openId) ?? null;
  const weights = limits.data?.weights;

  return (
    <Page>
      <PageHeader
        title="API keys"
        description="Keys authorise tiles, styles, routing and elevation for your apps."
        actions={
          <Button
            variant="primary"
            icon={<Plus className="size-4" />}
            onClick={() => setCreating(true)}
          >
            Create key
          </Button>
        }
      />
      {error && (
        <div className="mb-4">
          <ErrorNote error={error} />
        </div>
      )}
      <Card pad={false}>
        {keys.loading && !keys.data ? (
          <Spinner />
        ) : keys.error ? (
          <div className="p-4">
            <ErrorNote error={keys.error} onRetry={keys.reload} />
          </div>
        ) : list.length === 0 ? (
          <Empty
            icon={<KeyRound className="size-5" />}
            title="No API keys"
            action={
              <Button
                variant="primary"
                icon={<Plus className="size-4" />}
                onClick={() => setCreating(true)}
              >
                Create key
              </Button>
            }
          >
            Create a key for each app so you can see its usage and revoke it on its own.
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] text-sm">
              <thead>
                <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                  <th className="px-4 py-2 font-medium">Label</th>
                  <th className="px-3 py-2 font-medium">Created</th>
                  <th className="px-3 py-2 font-medium">Last used</th>
                  <th className="px-3 py-2 font-medium">Limits</th>
                  <th className="px-3 py-2 font-medium">Last 14 days</th>
                  <th className="px-3 py-2 text-right font-medium">Today</th>
                  <th className="px-3 py-2" />
                </tr>
              </thead>
              <tbody>
                {list.map((k) => (
                  <tr key={k.id} className="border-b border-line last:border-0 hover:bg-sunken/60">
                    <td className="px-4 py-2.5">
                      <button
                        type="button"
                        className="text-left font-medium hover:text-accent hover:underline"
                        aria-label={`Usage and limits of ${k.label}`}
                        onClick={() => setOpenId(k.id)}
                      >
                        {k.label}
                      </button>
                      <br />
                      <code className="font-mono text-xs text-faint">{k.prefix}…</code>
                    </td>
                    <td className="px-3 py-2.5 text-muted">{fmt.date(k.created_at)}</td>
                    <td className="px-3 py-2.5">
                      {k.last_used_at ? fmt.relTime(k.last_used_at) : <Badge>never</Badge>}
                    </td>
                    <td className="max-w-56 px-3 py-2.5">
                      <Limits k={k} />
                      {k.limits.daily_quota ? (
                        <Progress
                          className="mt-1.5 h-1.5"
                          value={share(k.usage.units_today, k.limits.daily_quota)}
                          tone={shareTone(share(k.usage.units_today, k.limits.daily_quota))}
                        />
                      ) : null}
                    </td>
                    <td className="px-3 py-2.5">
                      <Sparkline values={k.usage.daily} />
                    </td>
                    <td className="tnum px-3 py-2.5 text-right">
                      <div>{k.usage.today.toLocaleString()}</div>
                      <div className="text-xs text-faint">{fmt.number(k.usage.total)} total</div>
                      {(k.usage.limited_today ?? 0) > 0 && (
                        <div className="mt-0.5">
                          <Badge tone="warn">{exact(k.usage.limited_today ?? 0)} refused</Badge>
                        </div>
                      )}
                    </td>
                    <td className="px-3 py-2.5 text-right">
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`Revoke ${k.label}`}
                        onClick={() => setRevoking(k)}
                      >
                        <Trash2 className="size-3.5" />
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <div className="mt-4">
        {limits.loading && !limits.data ? (
          <Card>
            <Spinner />
          </Card>
        ) : limits.error ? (
          <Card title="Request costs and server limits">
            <ErrorNote error={limits.error} onRetry={limits.reload} />
          </Card>
        ) : limits.data ? (
          <LimitsCard cfg={limits.data} onSaved={limits.reload} />
        ) : null}
      </div>

      <KeyDetails
        k={opened}
        weights={weights}
        onClose={() => setOpenId(null)}
        onSaved={keys.reload}
      />
      <CreateKey
        weights={weights}
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={keys.reload}
        styles={styles.data ?? []}
      />
      <Confirm
        open={!!revoking}
        title={`Revoke “${revoking?.label}”?`}
        confirmLabel="Revoke key"
        busy={busy}
        onClose={() => setRevoking(null)}
        onConfirm={revoke}
      >
        Apps using <code className="font-mono">{revoking?.prefix}…</code> stop working immediately.
        This cannot be undone.
      </Confirm>
    </Page>
  );
}
