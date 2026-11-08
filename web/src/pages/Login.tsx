import { type FormEvent, useState } from "react";
import { ApiError, api } from "../api";
import { Button, Field, Input } from "../components/ui";

export function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.login(username, password);
      onLogin();
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 401
          ? "Wrong username or password."
          : err instanceof Error
            ? err.message
            : "Sign-in failed",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="relative grid min-h-full place-items-center overflow-hidden bg-bg px-4 py-10">
      <svg
        className="pointer-events-none absolute inset-0 h-full w-full text-line"
        aria-hidden="true"
        preserveAspectRatio="none"
        viewBox="0 0 800 600"
      >
        {Array.from({ length: 14 }, (_, i) => (
          <path
            key={i}
            d={`M-20 ${90 + i * 38} C 180 ${40 + i * 40}, 320 ${170 + i * 34}, 520 ${100 + i * 37} S 760 ${60 + i * 41}, 840 ${120 + i * 36}`}
            fill="none"
            stroke="currentColor"
            strokeWidth={1}
          />
        ))}
      </svg>
      <form
        onSubmit={submit}
        className="relative w-full max-w-sm rounded-2xl border border-line bg-panel p-7 shadow-float"
      >
        <div className="mb-6 flex items-center gap-3">
          <svg viewBox="0 0 32 32" className="size-9" aria-hidden="true">
            <rect width="32" height="32" rx="8" className="fill-accent" />
            <path d="M6 22l6-10 5 7 3-4 6 7z" className="fill-accent-fg" />
            <circle cx="22" cy="10" r="2.5" className="fill-accent-fg" />
          </svg>
          <div>
            <h1 className="text-lg font-semibold tracking-tight">mapthing</h1>
            <p className="text-xs text-muted">Sign in to manage your map server</p>
          </div>
        </div>
        <div className="space-y-4">
          <Field label="Username">
            {(id) => (
              <Input
                id={id}
                autoComplete="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
                autoFocus
              />
            )}
          </Field>
          <Field label="Password">
            {(id) => (
              <Input
                id={id}
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
            )}
          </Field>
          {error && (
            <p role="alert" className="rounded-lg bg-bad/10 px-3 py-2 text-sm text-bad">
              {error}
            </p>
          )}
          <Button type="submit" variant="primary" loading={busy} className="w-full">
            Sign in
          </Button>
          <p className="text-center text-xs text-faint">
            The admin credential is set with <code className="font-mono">ADMIN_USER</code> and{" "}
            <code className="font-mono">ADMIN_PASSWORD</code>.
          </p>
        </div>
      </form>
    </div>
  );
}
