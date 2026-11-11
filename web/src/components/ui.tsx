import { Check, Copy, Loader2, X } from "lucide-react";
import {
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";

export function cx(...c: (string | false | null | undefined)[]) {
  return c.filter(Boolean).join(" ");
}

type Variant = "primary" | "secondary" | "ghost" | "danger";

export function Button({
  variant = "secondary",
  size = "md",
  loading,
  icon,
  className,
  children,
  disabled,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant;
  size?: "sm" | "md";
  loading?: boolean;
  icon?: ReactNode;
}) {
  const v: Record<Variant, string> = {
    primary:
      "bg-accent text-accent-fg hover:bg-accent-strong shadow-sm disabled:opacity-50 border border-transparent",
    secondary:
      "bg-panel text-fg border border-line-strong hover:bg-sunken shadow-xs disabled:opacity-50",
    ghost: "text-muted hover:text-fg hover:bg-sunken border border-transparent disabled:opacity-40",
    danger: "bg-bad text-white hover:brightness-95 border border-transparent disabled:opacity-50",
  };
  return (
    <button
      type="button"
      disabled={disabled || loading}
      className={cx(
        "inline-flex items-center justify-center gap-1.5 rounded-lg font-medium whitespace-nowrap transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent disabled:cursor-not-allowed",
        size === "sm" ? "h-7 px-2.5 text-xs" : "h-9 px-3.5 text-sm",
        v[variant],
        className,
      )}
      {...rest}
    >
      {loading ? <Loader2 className="size-4 animate-spin" /> : icon}
      {children}
    </button>
  );
}

export function Card({
  title,
  action,
  children,
  className,
  pad = true,
}: {
  title?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
  pad?: boolean;
}) {
  return (
    <section
      className={cx("rounded-(--radius-card) border border-line bg-panel shadow-xs", className)}
    >
      {(title || action) && (
        <header className="flex items-center justify-between gap-3 border-b border-line px-4 py-2.5">
          <h2 className="text-[13px] font-semibold tracking-tight">{title}</h2>
          {action}
        </header>
      )}
      <div className={pad ? "p-4" : undefined}>{children}</div>
    </section>
  );
}

type Tone = "neutral" | "ok" | "warn" | "bad" | "info" | "accent";
export function Badge({
  tone = "neutral",
  children,
  dot,
}: {
  tone?: Tone;
  children: ReactNode;
  dot?: boolean;
}) {
  const t: Record<Tone, string> = {
    neutral: "bg-sunken text-muted ring-line",
    ok: "bg-ok/10 text-ok ring-ok/25",
    warn: "bg-warn/12 text-[oklch(52%_0.12_70)] dark:text-warn ring-warn/30",
    bad: "bg-bad/10 text-bad ring-bad/25",
    info: "bg-info/10 text-info ring-info/25",
    accent: "bg-accent-soft text-accent-strong ring-accent/25",
  };
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ring-inset whitespace-nowrap",
        t[tone],
      )}
    >
      {dot && <span className="size-1.5 rounded-full bg-current" />}
      {children}
    </span>
  );
}

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  children: (id: string) => ReactNode;
}) {
  const id = useId();
  return (
    <div className="space-y-1.5">
      <label htmlFor={id} className="block text-xs font-medium text-muted">
        {label}
      </label>
      {children(id)}
      {error ? (
        <p className="text-xs text-bad">{error}</p>
      ) : hint ? (
        <p className="text-xs text-faint">{hint}</p>
      ) : null}
    </div>
  );
}

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cx(
        "h-9 w-full rounded-lg border border-line-strong bg-panel px-3 text-sm text-fg placeholder:text-faint shadow-xs outline-none transition focus:border-accent focus:ring-3 focus:ring-accent/20",
        className,
      )}
      {...rest}
    />
  );
}

export function Switch({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx(
        "relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent disabled:opacity-50",
        checked ? "bg-accent" : "bg-line-strong",
      )}
    >
      <span
        className={cx(
          "inline-block size-4 rounded-full bg-white shadow transition-transform",
          checked ? "translate-x-4.5" : "translate-x-0.5",
        )}
      />
    </button>
  );
}

export function Segmented<T extends string>({
  value,
  onChange,
  options,
  size = "md",
  label,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { value: T; label: ReactNode; title?: string }[];
  size?: "sm" | "md";
  label: string;
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="inline-flex rounded-lg border border-line bg-sunken p-0.5"
    >
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={value === o.value}
          title={o.title}
          onClick={() => onChange(o.value)}
          className={cx(
            "inline-flex items-center gap-1 rounded-md font-medium transition",
            size === "sm" ? "h-6 px-2 text-xs" : "h-7 px-2.5 text-[13px]",
            value === o.value ? "bg-panel text-fg shadow-sm" : "text-muted hover:text-fg",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Progress({
  value,
  live,
  tone = "accent",
  className,
}: {
  value: number;
  live?: boolean;
  tone?: "accent" | "bad" | "ok";
  className?: string;
}) {
  const pct = Math.max(0, Math.min(1, value)) * 100;
  const c = { accent: "bg-accent", bad: "bg-bad", ok: "bg-ok" }[tone];
  return (
    <div
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(pct)}
      className={cx(
        "h-2 w-full overflow-hidden rounded-full bg-sunken ring-1 ring-line ring-inset",
        className,
      )}
    >
      <div
        className={cx(
          "h-full rounded-full transition-[width] duration-500",
          c,
          live && "progress-live",
        )}
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

export function Stat({
  label,
  value,
  sub,
}: {
  label: ReactNode;
  value: ReactNode;
  sub?: ReactNode;
}) {
  return (
    <div className="min-w-0">
      <div className="text-[11px] font-medium uppercase tracking-wide text-faint">{label}</div>
      <div className="tnum mt-0.5 truncate text-lg font-semibold tracking-tight">{value}</div>
      {sub && <div className="truncate text-xs text-muted">{sub}</div>}
    </div>
  );
}

export function Empty({
  icon,
  title,
  children,
  action,
}: {
  icon?: ReactNode;
  title: ReactNode;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-10 text-center">
      {icon && (
        <div className="mb-3 grid size-11 place-items-center rounded-xl bg-accent-soft text-accent-strong">
          {icon}
        </div>
      )}
      <h3 className="text-sm font-semibold">{title}</h3>
      {children && <p className="mt-1 max-w-sm text-sm text-muted">{children}</p>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

export function Spinner({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 py-10 text-sm text-muted">
      <Loader2 className="size-4 animate-spin" /> {label}…
    </div>
  );
}

export function ErrorNote({ error, onRetry }: { error: Error | string; onRetry?: () => void }) {
  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border border-bad/30 bg-bad/8 px-3 py-2 text-sm text-bad">
      <span>{typeof error === "string" ? error : error.message}</span>
      {onRetry && (
        <Button size="sm" variant="ghost" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-0.5 text-sm text-muted">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

export function Page({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return (
    <div className="h-full overflow-y-auto">
      <div className={cx("mx-auto px-4 py-6 sm:px-6 lg:px-8", wide ? "max-w-7xl" : "max-w-6xl")}>
        {children}
      </div>
    </div>
  );
}

export function CopyButton({ value, label = "Copy" }: { value: string; label?: string }) {
  const [done, setDone] = useState(false);
  useEffect(() => {
    if (!done) return;
    const t = setTimeout(() => setDone(false), 1500);
    return () => clearTimeout(t);
  }, [done]);
  return (
    <Button
      size="sm"
      variant="ghost"
      aria-label={label}
      icon={done ? <Check className="size-3.5 text-ok" /> : <Copy className="size-3.5" />}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value);
        } catch {
          /* clipboard unavailable (http); the value is selectable anyway */
        }
        setDone(true);
      }}
    >
      {done ? "Copied" : null}
    </Button>
  );
}

export function Modal({
  open,
  onClose,
  title,
  children,
  footer,
  size = "md",
  dismissable = true,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  size?: "sm" | "md" | "lg";
  dismissable?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  const dismissRef = useRef(dismissable);
  dismissRef.current = dismissable;
  // Escape closes; focus the first control once when the dialog opens (not on every render).
  useEffect(() => {
    if (!open) return;
    const fn = (e: KeyboardEvent) => e.key === "Escape" && dismissRef.current && closeRef.current();
    window.addEventListener("keydown", fn);
    ref.current?.querySelector<HTMLElement>("input,button,textarea,select")?.focus();
    return () => window.removeEventListener("keydown", fn);
  }, [open]);
  if (!open) return null;
  const w = { sm: "max-w-sm", md: "max-w-lg", lg: "max-w-2xl" }[size];
  return (
    <div className="fixed inset-0 z-50 flex items-end justify-center p-0 sm:items-center sm:p-4">
      <div
        className="absolute inset-0 bg-black/40 backdrop-blur-[2px]"
        onClick={() => dismissable && onClose()}
      />
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-label={typeof title === "string" ? title : undefined}
        className={cx(
          "relative flex max-h-[92vh] w-full flex-col rounded-t-2xl border border-line bg-panel shadow-float sm:rounded-2xl",
          w,
        )}
      >
        <header className="flex items-center justify-between border-b border-line px-5 py-3">
          <h2 className="text-[15px] font-semibold">{title}</h2>
          {dismissable && (
            <button
              type="button"
              aria-label="Close"
              onClick={onClose}
              className="rounded-md p-1 text-muted hover:bg-sunken hover:text-fg"
            >
              <X className="size-4" />
            </button>
          )}
        </header>
        <div className="overflow-y-auto px-5 py-4">{children}</div>
        {footer && (
          <footer className="flex justify-end gap-2 border-t border-line px-5 py-3">
            {footer}
          </footer>
        )}
      </div>
    </div>
  );
}

export function Confirm({
  open,
  title,
  children,
  confirmLabel,
  onConfirm,
  onClose,
  busy,
  variant = "danger",
}: {
  open: boolean;
  title: string;
  children: ReactNode;
  confirmLabel: string;
  onConfirm: () => void;
  onClose: () => void;
  busy?: boolean;
  /** "danger" for something that cannot be undone; "primary" for a change that can. */
  variant?: "danger" | "primary";
}) {
  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      size="sm"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant={variant} loading={busy} onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      <div className="text-sm text-muted">{children}</div>
    </Modal>
  );
}
