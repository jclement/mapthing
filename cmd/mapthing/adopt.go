package main

// Adopting a generation built elsewhere (docs/operations.md): the admin API form, which
// starts the routing graph before it swaps, and the command-line form for a shell on the box.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jclement/mapthing/internal/build"
	"github.com/jclement/mapthing/internal/config"
	"github.com/jclement/mapthing/internal/server"
	"github.com/jclement/mapthing/internal/supervisor"
)

func needParts(s config.Serve) build.Parts {
	return build.Parts{Tiles: s.Tiles, Routing: s.Routing, Elevation: s.Elevation}
}

// activateGeneration is server.Deps.ActivateGeneration. The order is what keeps a bad
// generation from ever being served, in whole or in part: validate the files; bring the
// routing graph up as a standby without switching to it (a graph that will not load stops
// here); move the current symlink; and only when that has succeeded switch routing to the
// standby and retire the old instance. If the symlink cannot be moved the standby is
// discarded, and tiles, elevation and routing are all still the old generation.
func (f *features) activateGeneration(ctx context.Context, id string) (server.Activation, error) {
	res, err := f.builder.Adopt(ctx, id, build.AdoptOptions{
		Prepare: func(ctx context.Context, genDir string) (build.Prepared, error) {
			if f.valhalla == nil {
				return nil, nil
			}
			dir, err := filepath.EvalSymlinks(genDir) // as tiles.CurrentGeneration will name it
			if err != nil {
				return nil, err
			}
			standby, err := f.valhalla.Prepare(ctx, dir)
			if errors.Is(err, supervisor.ErrUnavailable) {
				return nil, nil // no routing binary on this machine (development)
			}
			if err != nil {
				return nil, err
			}
			return standby, nil
		},
		Restart: func(ctx context.Context) error {
			f.watcher.Check(ctx)
			return nil
		},
	})
	var inv *build.InvalidError
	var prep *build.PrepareError
	switch {
	case err == nil:
	case errors.Is(err, build.ErrNoGeneration):
		return server.Activation{}, &server.ActivateError{Status: http.StatusNotFound, Code: "no_generation", Message: err.Error()}
	case errors.Is(err, build.ErrBusy):
		return server.Activation{}, &server.ActivateError{Status: http.StatusConflict, Code: "busy",
			Message: "a build or another activation is running; try again when it finishes"}
	case errors.As(err, &inv):
		return server.Activation{}, &server.ActivateError{Status: http.StatusUnprocessableEntity, Code: "invalid_generation",
			Message: inv.Error() + ". The live generation is unchanged.", Problems: inv.Problems}
	case errors.As(err, &prep):
		return server.Activation{}, &server.ActivateError{Status: http.StatusUnprocessableEntity, Code: "routing_failed",
			Message: "the routing graph of generation " + id + " did not start: " + prep.Error() + ". The live generation is unchanged."}
	default:
		return server.Activation{}, err
	}
	out := server.Activation{ID: res.ID, AlreadyLive: res.AlreadyLive, Removed: res.Removed}
	if res.Previous != "" {
		out.Previous = &res.Previous
	}
	return out, nil
}

// generationInventory is server.Deps.GenerationInventory.
func (f *features) generationInventory() ([]server.GenerationState, int64) {
	gens, disk := f.builder.Inventory()
	out := make([]server.GenerationState, 0, len(gens))
	for _, g := range gens {
		out = append(out, server.GenerationState{ID: g.ID, Status: g.Status, Owned: g.Owned, Valid: g.Valid, Problems: g.Problems, Bytes: g.Bytes})
	}
	return out, disk
}

// deleteGeneration is server.Deps.DeleteGeneration.
func (f *features) deleteGeneration(_ context.Context, id string) error {
	err := f.builder.DeleteGeneration(id)
	switch {
	case errors.Is(err, build.ErrNoGeneration):
		return &server.ActivateError{Status: http.StatusNotFound, Code: "no_generation", Message: err.Error()}
	case errors.Is(err, build.ErrGenerationLive):
		return &server.ActivateError{Status: http.StatusConflict, Code: "live",
			Message: "generation " + id + " is the one being served; activate another generation first"}
	case errors.Is(err, build.ErrGenerationBusy):
		return &server.ActivateError{Status: http.StatusConflict, Code: "busy",
			Message: "generation " + id + " is being built or activated; wait for that to finish"}
	}
	return err
}

// adopt is `mapthing adopt [-data DIR] <generation directory | generation id>`.
//
// Given a directory outside <data>/generations it validates it, moves it in (a rename on the
// same filesystem, otherwise a copy) under the generation id in its manifest, and makes it
// live. Given an id it activates <data>/generations/<id>. A running server notices the swap
// within seconds and follows (tiles at once, routing blue/green). Unlike the admin API form it
// cannot try the routing graph first; do not run it while a build is running.
func adopt(args []string, out io.Writer) error {
	fl := flag.NewFlagSet("adopt", flag.ContinueOnError)
	data := fl.String("data", getenv("MAPTHING_DATA", "/data"), "data directory")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 1 {
		return errors.New("usage: mapthing adopt [-data DIR] <generation directory | generation id>")
	}
	serve, err := config.ParseServe(os.Getenv("MAPTHING_SERVE"))
	if err != nil {
		return err
	}
	need := needParts(serve)
	gens := filepath.Join(*data, "generations")
	arg := fl.Arg(0)
	id := arg
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		// The directory itself is moved, not a link to it.
		if arg, err = filepath.EvalSymlinks(arg); err != nil {
			return err
		}
		m, err := build.Validate(arg, "", need)
		if err != nil {
			return err
		}
		id = m.Generation
		if !build.ValidGenerationID(id) {
			return fmt.Errorf("the manifest's generation %q cannot name a directory", id)
		}
		dest := filepath.Join(gens, id)
		if !sameDir(arg, dest) {
			if _, err := os.Stat(dest); err == nil {
				return fmt.Errorf("%s already exists; activate it with: mapthing adopt %s", dest, id)
			}
			if err := os.MkdirAll(gens, 0o755); err != nil {
				return err
			}
			if err := os.Rename(arg, dest); err != nil {
				// Another filesystem: copy under a dot name (not a generation yet), then rename.
				tmp := filepath.Join(gens, "."+id+".adopting")
				_ = os.RemoveAll(tmp)
				if err := copyTree(arg, tmp); err != nil {
					_ = os.RemoveAll(tmp)
					return fmt.Errorf("copy into %s: %w", gens, err)
				}
				if err := os.Rename(tmp, dest); err != nil {
					_ = os.RemoveAll(tmp)
					return err
				}
				_, _ = fmt.Fprintf(out, "copied %s to %s (the original is still there)\n", arg, dest)
			}
		}
	}
	b := build.New(*data)
	b.Need = need
	res, err := b.Adopt(context.Background(), id, build.AdoptOptions{})
	if err != nil {
		return err
	}
	if res.AlreadyLive {
		_, _ = fmt.Fprintf(out, "generation %s is already live\n", res.ID)
		return nil
	}
	_, _ = fmt.Fprintf(out, "generation %s is live", res.ID)
	if res.Previous != "" {
		_, _ = fmt.Fprintf(out, " (was %s)", res.Previous)
	}
	_, _ = fmt.Fprintln(out)
	for _, r := range res.Removed {
		_, _ = fmt.Fprintf(out, "removed generation %s\n", r)
	}
	return nil
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return false
	}
	aa, _ := filepath.Abs(ra)
	ab, _ := filepath.Abs(rb)
	return aa == ab
}

// copyTree copies the regular files and directories of src to dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, in); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}
