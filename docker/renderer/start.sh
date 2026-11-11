#!/bin/sh
# Start the renderer on a private Xvfb display. MapLibre Native's Linux build renders through
# GLX; Mesa's llvmpipe does the work in software, so no GPU is needed.
#   RENDERER_RUNTIME   node (default). Bun cannot load the addon: see README.md.
#   RENDERER_DISPLAY   X display number to use (default 99)
# xvfb-run is not used: it hangs when it runs as PID 1 in a container.
set -eu
cd "$(dirname "$0")"
RUNTIME=${RENDERER_RUNTIME:-node}
DISP=${RENDERER_DISPLAY:-99}
export LIBGL_ALWAYS_SOFTWARE=1 DISPLAY=":$DISP"

rm -f "/tmp/.X$DISP-lock" "/tmp/.X11-unix/X$DISP"
# What the X server prints goes to stderr with an "[xvfb] " prefix; mapthing files those lines
# under their own log source. A fifo keeps $! the X server's pid (a pipeline's would be sed's).
# The fifo is removed only once the X server is up: each side opens it in its own background
# process, and removing it before both had (which happened on a busy machine) left the X
# server unable to start.
XLOG="${TMPDIR:-/tmp}/xvfb-$DISP.$$.fifo"
SPID=
rm -f "$XLOG"
if mkfifo "$XLOG" 2>/dev/null; then
  sed 's/^/[xvfb] /' <"$XLOG" >&2 &
  SPID=$!
  Xvfb ":$DISP" -screen 0 1024x768x24 -nolisten tcp -noreset >"$XLOG" 2>&1 &
  XPID=$!
else
  Xvfb ":$DISP" -screen 0 1024x768x24 -nolisten tcp -noreset >/dev/null 2>&1 &
  XPID=$!
fi
i=0
while [ ! -S "/tmp/.X11-unix/X$DISP" ]; do
  i=$((i + 1))
  if [ $i -gt 100 ] || ! kill -0 $XPID 2>/dev/null; then
    echo "Xvfb did not start" >&2
    # Leave nothing behind: a process still holding this script's output would keep the
    # supervisor from seeing that it failed.
    kill $XPID $SPID 2>/dev/null || true
    rm -f "$XLOG"
    exit 1
  fi
  sleep 0.1
done
rm -f "$XLOG"

"$RUNTIME" server.js &
RPID=$!
trap 'kill -TERM $RPID 2>/dev/null; wait $RPID; kill $XPID 2>/dev/null' TERM INT
# exit as soon as either process dies, so the supervisor restarts the pair
while kill -0 $RPID 2>/dev/null && kill -0 $XPID 2>/dev/null; do sleep 1; done
kill $RPID $XPID 2>/dev/null || true
wait $RPID 2>/dev/null
