import { Component, type ErrorInfo, type ReactNode } from "react";

type Props = { children: ReactNode };
type State = { error: Error | null };

/**
 * Catches render errors so a bug in one view shows a message with a way back,
 * instead of unmounting the whole app and leaving a blank page.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("mapthing UI error", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <div
        role="alert"
        className="mx-auto mt-16 max-w-xl rounded-lg border border-red-300 bg-red-50 p-6 text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100"
      >
        <h1 className="text-lg font-semibold">This page hit an error</h1>
        <p className="mt-2 text-sm">
          The rest of mapthing is fine. Reload to try again; if it keeps happening, the message
          below helps when reporting it.
        </p>
        <pre className="mt-4 overflow-x-auto whitespace-pre-wrap text-xs opacity-80">
          {error.message}
        </pre>
        <div className="mt-4 flex gap-3">
          <button
            type="button"
            className="rounded-md bg-red-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-red-800"
            onClick={() => location.reload()}
          >
            Reload
          </button>
          <button
            type="button"
            className="rounded-md border border-red-300 px-3 py-1.5 text-sm dark:border-red-700"
            onClick={() => this.setState({ error: null })}
          >
            Try again without reloading
          </button>
        </div>
      </div>
    );
  }
}
