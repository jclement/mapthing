import { KeyRound, Plus, ShieldAlert, Trash2 } from "lucide-react";
import { type FormEvent, useState } from "react";
import {
  type ApiKey,
  api,
  type CreatedKey,
  DEFAULT_STYLE,
  keySnippets,
  type StyleInfo,
} from "../api";
import { Sparkline } from "../components/charts";
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
  Spinner,
} from "../components/ui";
import * as fmt from "../lib/format";
import { useApi } from "../lib/useApi";

function Limits({ k }: { k: ApiKey }) {
  const parts: string[] = [];
  if (k.limits.rpm) parts.push(`${k.limits.rpm.toLocaleString()}/min`);
  if (k.limits.rpd) parts.push(`${k.limits.rpd.toLocaleString()}/day`);
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

function CreateKey({
  open,
  onClose,
  onCreated,
  styles,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
  styles: StyleInfo[];
}) {
  const [style, setStyle] = useState<string>(DEFAULT_STYLE);
  const [label, setLabel] = useState("");
  const [rpm, setRpm] = useState("");
  const [rpd, setRpd] = useState("");
  const [origins, setOrigins] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [created, setCreated] = useState<CreatedKey | null>(null);

  function reset() {
    setLabel("");
    setRpm("");
    setRpd("");
    setOrigins("");
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
    setBusy(true);
    setError(undefined);
    try {
      const out = await api.createKey({
        label: label.trim(),
        rpm: rpm ? Number(rpm) : null,
        rpd: rpd ? Number(rpd) : null,
        origins: origins
          .split(/[\s,]+/)
          .map((s) => s.trim())
          .filter(Boolean),
      });
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
        <div className="grid grid-cols-2 gap-3">
          <Field label="Requests per minute" hint="Empty = no limit">
            {(id) => (
              <Input
                id={id}
                type="number"
                min={1}
                value={rpm}
                onChange={(e) => setRpm(e.target.value)}
              />
            )}
          </Field>
          <Field label="Requests per day" hint="Empty = no limit">
            {(id) => (
              <Input
                id={id}
                type="number"
                min={1}
                value={rpd}
                onChange={(e) => setRpd(e.target.value)}
              />
            )}
          </Field>
        </div>
        <Field
          label="Allowed origins"
          hint="Browser origins allowed to use the key, comma separated. Empty = any."
        >
          {(id) => (
            <Input
              id={id}
              value={origins}
              placeholder="https://app.example.com"
              onChange={(e) => setOrigins(e.target.value)}
            />
          )}
        </Field>
        {error && <ErrorNote error={error} />}
      </form>
    </Modal>
  );
}

export function KeysPage() {
  const keys = useApi(() => api.keys(), []);
  const styles = useApi(() => api.styles(), []);
  const [creating, setCreating] = useState(false);
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
                      <div className="font-medium">{k.label}</div>
                      <code className="font-mono text-xs text-faint">{k.prefix}…</code>
                    </td>
                    <td className="px-3 py-2.5 text-muted">{fmt.date(k.created_at)}</td>
                    <td className="px-3 py-2.5">
                      {k.last_used_at ? fmt.relTime(k.last_used_at) : <Badge>never</Badge>}
                    </td>
                    <td className="max-w-48 px-3 py-2.5">
                      <Limits k={k} />
                    </td>
                    <td className="px-3 py-2.5">
                      <Sparkline values={k.usage.daily} />
                    </td>
                    <td className="tnum px-3 py-2.5 text-right">
                      <div>{k.usage.today.toLocaleString()}</div>
                      <div className="text-xs text-faint">{fmt.number(k.usage.total)} total</div>
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

      <CreateKey
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
