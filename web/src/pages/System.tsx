import { CheckCircle2, ExternalLink, XCircle } from "lucide-react";
import { api } from "../api";
import { Badge, Card, ErrorNote, Page, PageHeader, Spinner, Stat } from "../components/ui";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import { useApi } from "../lib/useApi";

export function SystemPage() {
  const { generationTick } = useEvents();
  const s = useApi(() => api.system(), [generationTick], 15000);
  const d = s.data;
  if (!d) {
    return (
      <Page>
        <PageHeader title="System" />
        {s.error ? <ErrorNote error={s.error} onRetry={s.reload} /> : <Spinner />}
      </Page>
    );
  }
  return (
    <Page>
      <PageHeader
        title="System"
        description="Versions, readiness and data licences."
        actions={
          d.ready ? (
            <Badge tone="ok" dot>
              Ready
            </Badge>
          ) : (
            <Badge tone="warn" dot>
              Not ready
            </Badge>
          )
        }
      />
      <div className="grid gap-5 lg:grid-cols-2">
        <Card title="mapthing">
          <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
            <Stat label="Version" value={d.version} />
            <Stat label="Commit" value={<span className="font-mono text-base">{d.commit}</span>} />
            <Stat
              label="Built"
              value={<span className="text-base">{fmt.date(d.build_date)}</span>}
            />
            <Stat
              label="Disk free"
              value={fmt.bytes(d.disk.free_bytes)}
              sub={`of ${fmt.bytes(d.disk.total_bytes)}`}
            />
            <Stat
              label="Live generation"
              value={<span className="font-mono text-base">{d.current_generation ?? "none"}</span>}
            />
          </div>
          <table className="mt-5 w-full text-[13px]">
            <tbody>
              {d.components.map((c) => (
                <tr key={c.name} className="border-t border-line">
                  <td className="py-1.5">{c.name}</td>
                  <td className="py-1.5 text-right font-mono text-xs text-muted">{c.version}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
        <Card title="Readiness">
          <ul className="space-y-2.5">
            {d.readiness.map((r) => (
              <li key={r.name} className="flex items-start gap-2.5">
                {r.ok ? (
                  <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-ok" />
                ) : (
                  <XCircle className="mt-0.5 size-4 shrink-0 text-bad" />
                )}
                <div>
                  <div className="text-[13px] font-medium">{r.name}</div>
                  {r.detail && <div className="text-xs text-muted">{r.detail}</div>}
                </div>
              </li>
            ))}
          </ul>
          <p className="mt-4 text-xs text-faint">
            <code className="font-mono">/healthz</code> answers while the process is alive;{" "}
            <code className="font-mono">/readyz</code> once a generation is live and the
            subprocesses are up.
          </p>
        </Card>
        <Card title="Attribution and licences" className="lg:col-span-2">
          <p className="mb-3 text-sm text-muted">
            mapthing is MIT licensed. Map data and elevation come from these sources; every style
            carries the attribution. Keep it visible in your apps.
          </p>
          <ul className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
            {d.attribution.map((a) => (
              <li key={a.name} className="text-[13px]">
                <div className="flex items-center gap-1.5 font-medium">
                  {a.name}
                  {a.url && (
                    <a
                      href={a.url}
                      target="_blank"
                      rel="noreferrer"
                      className="text-faint hover:text-accent"
                      aria-label={`${a.name} licence`}
                    >
                      <ExternalLink className="size-3.5" />
                    </a>
                  )}
                </div>
                <div className="text-xs text-muted">{a.license}</div>
                {a.note && <div className="text-xs text-faint">{a.note}</div>}
              </li>
            ))}
            {d.imagery_source && (
              <li className="text-[13px]">
                <div className="font-medium">Imagery: {d.imagery_source.name}</div>
                <div className="text-xs text-muted">{d.imagery_source.attribution}</div>
              </li>
            )}
          </ul>
        </Card>
      </div>
    </Page>
  );
}
