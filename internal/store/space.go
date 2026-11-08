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

// nearlyFull reports the bytes in use past 90 % of the ceiling.
func (s *Store) nearlyFull() (bool, error) {
	sp, err := s.measure()
	return sp.InUseBytes*10 > s.maxBytes*9, err
}
