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

// reserveBuild records the generation a build is writing ("" when it ends).
func (b *Builder) reserveBuild(id string) {
	b.busyMu.Lock()
	b.building = id
	b.busyMu.Unlock()
}

// reserveActivation claims id for an adoption. false when a delete of it is under way.
func (b *Builder) reserveActivation(id string) bool {
	b.busyMu.Lock()
	defer b.busyMu.Unlock()
	if b.deleting[id] {
		return false
	}
	b.activating = id
	return true
}

func (b *Builder) releaseActivation() {
	b.busyMu.Lock()
	b.activating = ""
	b.busyMu.Unlock()
}

// Building returns the id of the generation a build is writing now, or "".
func (b *Builder) Building() string {
	b.busyMu.Lock()
	defer b.busyMu.Unlock()
	return b.building
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
// (ErrGenerationLive), the one a build is writing, an activation is switching to or another
// delete is removing (ErrGenerationBusy), and anything that is not a generation directory
// (ErrNoGeneration). Other generations can be deleted while a build runs.
// Unlike retention it deletes a generation whether or not this data directory owns it: this
// is a person's decision. A symbolic link under generations/ is removed, not followed.
func (b *Builder) DeleteGeneration(id string) error {
	if !ValidGenerationID(id) {
		return fmt.Errorf("%w: %q is not a generation id", ErrNoGeneration, id)
	}
	dir := filepath.Join(b.generationsDir(), id)

	// The checks and the claim are one step under the coordination lock. A build or an
	// activation claims its generation under the same lock before touching it, and current
	// only ever moves to a generation so claimed; so once this delete holds its claim, the
	// directory cannot become live, be validated or be written until the claim is released.
	b.busyMu.Lock()
	fi, err := os.Lstat(dir)
	switch {
	case err != nil:
		b.busyMu.Unlock()
		return fmt.Errorf("%w: there is no generation %s", ErrNoGeneration, id)
	case id == b.building || id == b.activating || b.deleting[id]:
		b.busyMu.Unlock()
		return ErrGenerationBusy
	case b.isLive(id, dir):
		b.busyMu.Unlock()
		return ErrGenerationLive
	case fi.Mode()&os.ModeSymlink == 0 && !fi.IsDir():
		b.busyMu.Unlock()
		return fmt.Errorf("%w: %s is not a directory", ErrNoGeneration, id)
	}
	if b.deleting == nil {
		b.deleting = map[string]bool{}
	}
	b.deleting[id] = true
	b.busyMu.Unlock()
	defer func() {
		b.busyMu.Lock()
		delete(b.deleting, id)
		b.busyMu.Unlock()
	}()

	if fi.Mode()&os.ModeSymlink != 0 {
		err = os.Remove(dir)
	} else if b.removeAll != nil {
		err = b.removeAll(dir)
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

// isLive reports whether id, or the directory it names, is what current points at (current
// may name the same directory another way).
func (b *Builder) isLive(id, dir string) bool {
	if id == b.CurrentGeneration() {
		return true
	}
	if live, err := filepath.EvalSymlinks(b.currentLink()); err == nil {
		if real, err := filepath.EvalSymlinks(dir); err == nil && real == live {
			return true
		}
	}
	return false
}
