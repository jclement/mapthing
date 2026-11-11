package build

// What is in <data>/generations/, for the admin UI, and deleting one by hand.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// Generation states, as the admin UI names them.
const (
	GenLive     = "live"     // current points at it
	GenPrevious = "previous" // the one retention keeps for rollback
	GenKept     = "kept"     // complete and owned, but not the rollback slot
	GenBuilding = "building" // a build is writing it now
	GenIncoming = "incoming" // not owned: copied in (or still arriving), never activated here
	GenInvalid  = "invalid"  // owned (or left by a build) but incomplete or damaged
)

// GenerationInfo describes one directory under generations/.
type GenerationInfo struct {
	ID     string
	Status string
	// Owned: built here to the end, or made live here. Retention only touches owned generations.
	Owned bool
	// Valid: it passed Validate for the parts this server serves, so it can be activated.
	Valid bool
	// Problems says why it is not valid.
	Problems []string
	// Bytes is the size of its files. Files hard-linked from another generation (reused
	// terrain) count here in full; DiskBytes in Inventory counts them once.
	Bytes int64
}

func (b *Builder) setBusy(building, activating string) {
	b.busyMu.Lock()
	b.building, b.activating = building, activating
	b.busyMu.Unlock()
}

// Building returns the id of the generation a build is writing now, or "".
func (b *Builder) Building() string {
	b.busyMu.Lock()
	defer b.busyMu.Unlock()
	return b.building
}

func (b *Builder) activatingNow() string {
	b.busyMu.Lock()
	defer b.busyMu.Unlock()
	return b.activating
}

// Inventory lists every generation directory (newest first) with its state, and the disk
// they use together, counting a file shared by hard link once.
func (b *Builder) Inventory() (gens []GenerationInfo, diskBytes int64) {
	rec := b.loadRetention()
	cur := b.CurrentGeneration()
	building := b.Building()
	previous := false
	seen := map[[2]uint64]bool{}
	for _, g := range b.Generations() {
		dir := filepath.Join(b.generationsDir(), g.ID)
		info := GenerationInfo{ID: g.ID, Owned: slices.Contains(rec.Generations, g.ID)}
		info.Bytes, diskBytes = sizeOnce(dir, seen, diskBytes)
		if g.ID == building {
			info.Status = GenBuilding
			gens = append(gens, info)
			continue
		}
		if _, err := Validate(dir, g.ID, b.need()); err == nil {
			info.Valid = true
		} else {
			var inv *InvalidError
			if errors.As(err, &inv) {
				info.Problems = inv.Problems
			} else {
				info.Problems = []string{err.Error()}
			}
		}
		switch {
		case g.ID == cur:
			info.Status = GenLive
		case !info.Owned && slices.Contains(rec.Building, g.ID):
			info.Status = GenInvalid
			info.Problems = append([]string{"left by a build that did not finish; the next build removes it"}, info.Problems...)
		case !info.Owned:
			info.Status = GenIncoming
		case !info.Valid:
			info.Status = GenInvalid
		case !previous && cur != "":
			info.Status, previous = GenPrevious, true
		default:
			info.Status = GenKept
		}
		gens = append(gens, info)
	}
	return gens, diskBytes
}

// sizeOnce returns the size of the files under dir, and total plus those of them not seen
// before (by device and inode).
func sizeOnce(dir string, seen map[[2]uint64]bool, total int64) (apparent, newTotal int64) {
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip what cannot be read
		}
		fi, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // gone while walking
		}
		apparent += fi.Size()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			k := [2]uint64{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev's type differs by platform
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		total += fi.Size()
		return nil
	})
	return apparent, total
}

// Refusals from DeleteGeneration.
var (
	ErrGenerationLive = errors.New("that generation is live")
	ErrGenerationBusy = errors.New("that generation is being built or activated")
)

// DeleteGeneration removes generations/<id> for good. It refuses the live generation
// (ErrGenerationLive), the one a build is writing or an activation is switching to
// (ErrGenerationBusy), and anything that is not a generation directory (ErrNoGeneration).
// Unlike retention it deletes a generation whether or not this data directory owns it: this
// is a person's decision. A symbolic link under generations/ is removed, not followed.
func (b *Builder) DeleteGeneration(id string) error {
	if !ValidGenerationID(id) {
		return fmt.Errorf("%w: %q is not a generation id", ErrNoGeneration, id)
	}
	dir := filepath.Join(b.generationsDir(), id)
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: there is no generation %s", ErrNoGeneration, id)
	}
	if id == b.Building() || id == b.activatingNow() {
		return ErrGenerationBusy
	}
	if id == b.CurrentGeneration() {
		return ErrGenerationLive
	}
	// By directory too: current may name the same directory another way.
	if live, err := filepath.EvalSymlinks(b.currentLink()); err == nil {
		if real, err := filepath.EvalSymlinks(dir); err == nil && real == live {
			return ErrGenerationLive
		}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		err = os.Remove(dir)
	} else if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrNoGeneration, id)
	} else {
		err = os.RemoveAll(dir)
	}
	if err != nil {
		return fmt.Errorf("could not delete generation %s: %w", id, err)
	}
	return b.updateRetention(func(r *retentionRecord) {
		r.Generations = without(r.Generations, id)
		r.Building = without(r.Building, id)
	})
}
