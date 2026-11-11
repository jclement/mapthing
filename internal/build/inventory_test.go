package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func statuses(b *Builder) map[string]string {
	out := map[string]string{}
	gens, _ := b.Inventory()
	for _, g := range gens {
		out[g.ID] = g.Status
	}
	return out
}

func TestInventoryStates(t *testing.T) {
	b := newAdoptBuilder(t)
	const g1, g2, g3, copyIn, broken, leftover, running = "20261101T000000Z", "20261102T000000Z", "20261103T000000Z",
		"20261104T000000Z", "20261105T000000Z", "20261106T000000Z", "20261107T000000Z"
	for _, id := range []string{g1, g2, g3, copyIn, broken, leftover, running} {
		b.addGen(t, id)
	}
	// g1, g2, g3 and broken are this server's; g3 is live.
	for _, id := range []string{g1, g2, g3, broken} {
		if err := b.own(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.swapCurrent(g3); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.generationsDir(), broken, "basemap.pmtiles")); err != nil {
		t.Fatal(err)
	}
	// leftover: a build that was killed. running: the build going on now.
	if err := b.updateRetention(func(r *retentionRecord) { r.Building = append(r.Building, leftover, running) }); err != nil {
		t.Fatal(err)
	}
	b.setBusy(running, "")
	// A copy in flight under a dot name is not a generation at all.
	if err := os.MkdirAll(filepath.Join(b.generationsDir(), ".incoming-x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := statuses(b)
	want := map[string]string{g3: GenLive, g2: GenPrevious, g1: GenKept, copyIn: GenIncoming,
		broken: GenInvalid, leftover: GenInvalid, running: GenBuilding}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%s: status %q, want %q", id, got[id], st)
		}
	}
	if len(got) != len(want) {
		t.Errorf("inventory lists %d generations, want %d: %v", len(got), len(want), got)
	}
	gens, _ := b.Inventory()
	for _, g := range gens {
		switch g.ID {
		case broken:
			if g.Valid || len(g.Problems) == 0 {
				t.Errorf("broken: valid=%v problems=%v", g.Valid, g.Problems)
			}
		case copyIn:
			if !g.Valid || g.Owned {
				t.Errorf("incoming copy: valid=%v owned=%v (a complete copy can be activated, but is not ours)", g.Valid, g.Owned)
			}
		case g1:
			if !g.Valid || !g.Owned || g.Bytes == 0 {
				t.Errorf("kept: %+v", g)
			}
		}
	}
}

func TestInventoryCountsSharedFilesOnce(t *testing.T) {
	b := newAdoptBuilder(t)
	const g1, g2 = "20261101T000000Z", "20261102T000000Z"
	d1, d2 := b.addGen(t, g1), b.addGen(t, g2)
	// g2 reuses g1's terrain by hard link, as a build with terrain reuse does.
	big := make([]byte, 100_000)
	if err := os.WriteFile(filepath.Join(d1, "extra.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(d1, "extra.bin"), filepath.Join(d2, "extra.bin")); err != nil {
		t.Skip("no hard links here:", err)
	}
	gens, disk := b.Inventory()
	var sum int64
	for _, g := range gens {
		sum += g.Bytes
	}
	if sum-disk != int64(len(big)) {
		t.Errorf("sum of generations %d, on disk %d: the shared %d bytes should count once", sum, disk, len(big))
	}
}

func TestDeleteGenerationRules(t *testing.T) {
	b := newAdoptBuilder(t)
	const live, old, copyIn, running, adopting = "20261103T000000Z", "20261101T000000Z", "20261104T000000Z",
		"20261107T000000Z", "20261108T000000Z"
	for _, id := range []string{live, old, copyIn, running, adopting} {
		b.addGen(t, id)
	}
	for _, id := range []string{live, old} {
		_ = b.own(id)
	}
	if _, err := b.Adopt(context.Background(), live, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	exists := func(id string) bool {
		_, err := os.Lstat(filepath.Join(b.generationsDir(), id))
		return err == nil
	}

	// The live generation is refused, by name and when reached through another name.
	if err := b.DeleteGeneration(live); !errors.Is(err, ErrGenerationLive) || !exists(live) {
		t.Fatalf("deleting the live generation: err=%v exists=%v", err, exists(live))
	}
	alias := filepath.Join(b.generationsDir(), "alias")
	if err := os.Symlink(live, alias); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteGeneration("alias"); !errors.Is(err, ErrGenerationLive) || !exists(live) || !exists("alias") {
		t.Fatalf("deleting a link to the live generation: err=%v", err)
	}

	// The one a build is writing, and the one an activation is switching to.
	b.setBusy(running, adopting)
	for _, id := range []string{running, adopting} {
		if err := b.DeleteGeneration(id); !errors.Is(err, ErrGenerationBusy) || !exists(id) {
			t.Fatalf("deleting busy generation %s: err=%v exists=%v", id, err, exists(id))
		}
	}
	// While that build runs, another generation can still be deleted.
	if err := b.DeleteGeneration(old); err != nil || exists(old) {
		t.Fatalf("deleting an old generation during a build: err=%v exists=%v", err, exists(old))
	}
	b.setBusy("", "")
	if rec := b.loadRetention(); len(rec.Generations) != 1 || rec.Generations[0] != live {
		t.Errorf("retention record after delete: %v (the deleted generation should be forgotten)", rec.Generations)
	}

	// A copy that was never activated can be deleted by hand (retention would never touch it).
	if err := b.DeleteGeneration(copyIn); err != nil || exists(copyIn) {
		t.Fatalf("deleting an incoming copy: err=%v", err)
	}

	// Not generations: unknown ids, path tricks, dot directories, plain files.
	if err := os.WriteFile(filepath.Join(b.generationsDir(), "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(b.DataDir, "precious")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"nope", "", "../precious", "..", ".retention.json", "a/b", "afile"} {
		if err := b.DeleteGeneration(id); !errors.Is(err, ErrNoGeneration) {
			t.Errorf("DeleteGeneration(%q) = %v, want ErrNoGeneration", id, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a path outside generations/ was deleted")
	}
	if _, err := os.Stat(b.retentionPath()); err != nil {
		t.Error("the retention record was deleted")
	}

	// A symbolic link to somewhere else is removed without following it.
	link := filepath.Join(b.generationsDir(), "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteGeneration("linked"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("deleting a link deleted what it pointed at")
	}
	// After all that the live generation still validates.
	if _, err := Validate(filepath.Join(b.generationsDir(), live), live, AllParts()); err != nil {
		t.Errorf("live generation after the deletes: %v", err)
	}
}
