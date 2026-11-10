package build

// Which generation directories retention may touch.
//
// <data>/generations/ is shared with whoever copies generations in from another machine, and
// a copy can be arriving at any moment, under its final name, while this process builds or
// adopts something else. So retention only ever considers directories this data directory has
// taken ownership of: generations a build here finished, and generations an adoption made
// live. Those are recorded in generations/.retention.json. A directory that is not recorded
// is somebody's copy, finished or not: it is never deleted and never counts as the generation
// kept for rollback. Activating it is the handoff that records it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

type retentionRecord struct {
	// Generations are owned: built here to the end, or adopted (made live) here.
	Generations []string `json:"generations"`
	// Building are builds in progress here. One still listed when another build runs is the
	// leftover of a build that was killed, and is deleted.
	Building []string `json:"building,omitempty"`
}

func (b *Builder) retentionPath() string {
	return filepath.Join(b.generationsDir(), ".retention.json")
}

func (b *Builder) need() Parts {
	if b.Need == (Parts{}) {
		return AllParts()
	}
	return b.Need
}

// loadRetention reads the record, creating it when the data directory has none.
func (b *Builder) loadRetention() *retentionRecord { return b.loadRetentionSeed(false) }

// EnsureRetentionRecord creates the record for a data directory from before it existed (a
// server calls it at start). Every complete generation there was built by that server, since
// nothing could be copied in then, so they are taken as owned, once. Without this an upgraded
// server would never prune the generations it already had.
func (b *Builder) EnsureRetentionRecord() { b.loadRetentionSeed(true) }

// loadRetentionSeed reads the record. When there is none it starts one: with every complete
// generation (all, for a server starting or a build, which is when an existing data directory
// is first met), or with only the live generation (an adoption, where other directories may be
// copies waiting their turn and must not become owned by accident).
func (b *Builder) loadRetentionSeed(all bool) *retentionRecord {
	var r retentionRecord
	data, err := os.ReadFile(b.retentionPath())
	if err == nil && json.Unmarshal(data, &r) == nil {
		return &r
	}
	if err == nil || !os.IsNotExist(err) {
		// Unreadable or corrupt: own nothing rather than guess. Nothing is deleted until
		// generations are built or adopted again.
		return &r
	}
	cur := b.CurrentGeneration()
	for _, g := range b.Generations() {
		if !all && g.ID != cur {
			continue
		}
		if _, err := Validate(filepath.Join(b.generationsDir(), g.ID), g.ID, b.need()); err == nil {
			r.Generations = append(r.Generations, g.ID)
		}
	}
	_ = b.saveRetention(&r)
	return &r
}

func (b *Builder) saveRetention(r *retentionRecord) error {
	if err := os.MkdirAll(b.generationsDir(), 0o755); err != nil {
		return err
	}
	sort.Strings(r.Generations)
	r.Generations = slices.Compact(r.Generations)
	sort.Strings(r.Building)
	r.Building = slices.Compact(r.Building)
	if r.Generations == nil {
		r.Generations = []string{}
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	tmp := b.retentionPath() + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.retentionPath())
}

// updateRetention applies f to the record and saves it.
func (b *Builder) updateRetention(f func(*retentionRecord)) error {
	r := b.loadRetention()
	f(r)
	return b.saveRetention(r)
}

func without(list []string, id string) []string {
	return slices.DeleteFunc(list, func(s string) bool { return s == id })
}

// own records id as a generation retention is responsible for.
func (b *Builder) own(id string) error {
	return b.updateRetention(func(r *retentionRecord) {
		r.Generations = append(r.Generations, id)
		r.Building = without(r.Building, id)
	})
}

// pruneGenerations applies the retention rule to the generations this data directory owns:
// the live one and the newest other complete one stay, older ones are deleted. It returns the
// generations it deleted.
//
//   - The live generation is never deleted, by name or by the directory current resolves to.
//   - A directory that is not owned (a copy from another machine, arriving or waiting to be
//     activated) is not deleted and does not count as the one kept.
//   - An owned generation that no longer validates does not count as the one kept either, and
//     is left for a person to look at.
//   - The directory of a build of this data directory that never finished (building is the
//     one running now, if any) is deleted.
func (b *Builder) pruneGenerations(building string) ([]string, error) {
	rec := b.loadRetention()
	cur := b.CurrentGeneration()
	liveDir, _ := filepath.EvalSymlinks(b.currentLink())
	kept := 0
	var removed []string
	var firstErr error
	remove := func(id string) {
		if err := os.RemoveAll(filepath.Join(b.generationsDir(), id)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		removed = append(removed, id)
	}
	for _, g := range b.Generations() { // newest first; real directories only
		dir := filepath.Join(b.generationsDir(), g.ID)
		if g.ID == cur || g.ID == building {
			continue
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil && liveDir != "" && real == liveDir {
			continue
		}
		owned := slices.Contains(rec.Generations, g.ID)
		if !owned {
			if slices.Contains(rec.Building, g.ID) {
				remove(g.ID) // a build here that was killed
			}
			continue
		}
		if _, err := Validate(dir, g.ID, b.need()); err != nil {
			continue
		}
		if kept < KeepGenerations-1 && cur != "" {
			kept++
			continue
		}
		remove(g.ID)
	}
	// Forget what is gone.
	exists := func(id string) bool {
		fi, err := os.Lstat(filepath.Join(b.generationsDir(), id))
		return err == nil && fi.IsDir()
	}
	rec.Generations = slices.DeleteFunc(rec.Generations, func(id string) bool { return !exists(id) })
	rec.Building = slices.DeleteFunc(rec.Building, func(id string) bool { return id != building || !exists(id) })
	if err := b.saveRetention(rec); err != nil && firstErr == nil {
		firstErr = err
	}
	return removed, firstErr
}
