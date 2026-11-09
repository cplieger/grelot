//go:build !windows

package store

// Space is the store file's size and how much of it holds data. bbolt
// grows the file ahead of use, reuses freed pages and never shrinks it, so
// FileBytes stays at or above its high-water mark until the next startup
// compaction, and only InUseBytes, the pages written less the free ones,
// falls when rows expire.
type Space struct {
	FileBytes  int64
	FreeBytes  int64
	InUseBytes int64
}

// measure reads the file's space without a transaction or a page walk.
// After Close it fails with ErrClosed.
func (s *Store) measure() (Space, error) {
	sp, err := s.space()
	if err != nil {
		return Space{}, closed(err)
	}
	return Space{FileBytes: sp.FileBytes, FreeBytes: sp.FreeBytes, InUseBytes: sp.InUseBytes}, nil
}

// grow refuses with KindFull, past 90 % of the ceiling, a write that adds
// rows by the caller's choice. A write that only removes, renews or
// recovers never calls it, so it goes through on a full store. It
// measures under the write lock: read before it, concurrent writes all
// see the space no other one has filled yet and pass the ceiling together.
func (w *wtx) grow() error {
	sp, err := w.s.measure()
	if err == nil && sp.InUseBytes*10 > w.s.maxBytes*9 {
		err = refuse(KindFull, "store nearly full: new posts, identities, follows, work paths, reservations and delegate links resume as older mail expires")
	}
	return err
}
