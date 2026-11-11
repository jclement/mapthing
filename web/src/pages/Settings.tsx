import { AlertTriangle, ExternalLink, Lock } from "lucide-react";
import { useState } from "react";
import { api, type Setting } from "../api";
import { OnDemandSettings } from "../components/OnDemand";
import { Badge, Card, cx, ErrorNote, Page, PageHeader, Segmented, Spinner } from "../components/ui";
import { countSet, groupSettings, shownValue, UNRECOGNISED } from "../lib/settings";
import { useApi } from "../lib/useApi";

export function SettingsPage() {
  const s = useApi(() => api.settings(), []);
  const sys = useApi(() => api.system(), []);
  // On-demand terrain is the one thing set here rather than in the environment. A server that
  // serves neither tiles nor elevation has none (503): the section is then left out.
  const od = useApi(() => api.ondemand(), [], 10000);
  const regions = useApi(() => api.regions(), []);
  const [show, setShow] = useState<"all" | "set">("all");

  return (
    <Page>
      <PageHeader
        title="Settings"
        description={
          <>
            How this server is configured, and what it is running. Settings come from the
            container's environment: change them in your <code className="font-mono">.env</code> or
            compose file and restart the container. Only on-demand terrain is changed here.
          </>
        }
        actions={
          s.data && (
            <Segmented
              label="Which settings to show"
              value={show}
              onChange={setShow}
              options={[
                { value: "all", label: "All" },
                { value: "set", label: `Set here (${countSet(s.data.settings)})` },
              ]}
            />
          )
        }
      />
      {s.error && !s.data ? (
        <ErrorNote error={s.error} onRetry={s.reload} />
      ) : !s.data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <Card title="Software">
            <table className="w-full text-[13px]">
              <tbody>
                {s.data.versions.map((v) => (
                  <tr key={v.name} className="border-b border-line last:border-0">
                    <td className="py-1.5 pr-3 font-medium">{v.name}</td>
                    <td className="py-1.5 pr-3 font-mono text-xs break-all sm:whitespace-nowrap">
                      {v.version || <span className="text-faint">unknown</span>}
                    </td>
                    <td className="hidden py-1.5 text-xs text-muted sm:table-cell">{v.detail}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>

          {od.data && (
            <OnDemandSettings
              status={od.data}
              regions={regions.data?.regions ?? []}
              freeBytes={sys.data?.disk.free_bytes}
              onChange={(st) => od.setData(() => st)}
            />
          )}

          {groupSettings(s.data.settings, show === "set").map((g) => (
            <Card
              key={g.name}
              title={
                <span className="flex items-center gap-2">
                  {g.name === UNRECOGNISED && <AlertTriangle className="size-4 text-warn" />}
                  {g.name}
                </span>
              }
              action={
                g.name !== UNRECOGNISED && (
                  <span className="text-xs text-faint">
                    {g.changed === 0 ? "all defaults" : `${g.changed} set`}
                  </span>
                )
              }
              pad={false}
              className={g.name === UNRECOGNISED ? "border-warn/50" : undefined}
            >
              {g.name === UNRECOGNISED && (
                <p className="border-b border-line bg-warn/10 px-4 py-2 text-[13px]">
                  These variables are set but mapthing does not read them. A misspelt name means the
                  setting you meant is still at its default.
                </p>
              )}
              <ul className="divide-y divide-line">
                {g.settings.map((x) => (
                  <SettingRow key={x.name} s={x} />
                ))}
              </ul>
            </Card>
          ))}
          {show === "set" && countSet(s.data.settings) === 0 && (
            <Card>
              <p className="text-sm text-muted">Nothing is set: every setting is at its default.</p>
            </Card>
          )}

          <Card title="Attribution and licences">
            <p className="mb-3 text-sm text-muted">
              mapthing is MIT licensed. Map data and elevation come from these sources; every style
              carries the attribution. Keep it visible in your apps.
            </p>
            {sys.error ? (
              <ErrorNote error={sys.error} onRetry={sys.reload} />
            ) : !sys.data ? (
              <Spinner />
            ) : (
              <ul className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
                {sys.data.attribution.map((a) => (
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
                {sys.data.imagery_source && (
                  <li className="text-[13px]">
                    <div className="font-medium">Imagery: {sys.data.imagery_source.name}</div>
                    <div className="text-xs text-muted">{sys.data.imagery_source.attribution}</div>
                  </li>
                )}
              </ul>
            )}
          </Card>
        </div>
      )}
    </Page>
  );
}

function SettingRow({ s }: { s: Setting }) {
  const v = shownValue(s);
  return (
    <li className="grid gap-x-4 gap-y-1 px-4 py-2.5 sm:grid-cols-[minmax(0,17rem)_minmax(0,1fr)]">
      <div className="min-w-0">
        <div className="font-mono text-xs font-medium break-all">{s.name}</div>
        <div className="mt-0.5 text-xs text-muted">{s.help}</div>
      </div>
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <span
            data-testid={`value-${s.name}`}
            className={cx("font-mono text-[13px] break-all", v.muted ? "text-muted" : "text-fg")}
          >
            {s.secret && s.set && <Lock className="mr-1 inline size-3.5 text-faint" />}
            {v.text}
          </span>
          {s.source === "env" ? (
            <Badge tone="accent">set in the environment</Badge>
          ) : s.source === "image" ? (
            <Badge>set by the image</Badge>
          ) : (
            <Badge>default</Badge>
          )}
          {s.secret && s.set && <Badge>hidden</Badge>}
        </div>
        {s.source === "env" && s.default && !s.secret && !s.default.startsWith("(") && (
          <div className="mt-0.5 text-xs text-faint">default: {s.default}</div>
        )}
      </div>
    </li>
  );
}
