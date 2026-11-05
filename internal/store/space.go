package store

// Space is the store file's size and how much of it holds data. bbolt
// reuses freed pages and never shrinks the file, so FileBytes stays near
// its high-water mark until the next startup compaction, and only
// InUseBytes falls when rows expire.
type Space struct {
	FileBytes  int64
	FreeBytes  int64
	InUseBytes int64
}

// Measure reads the file's space without a transaction or a page walk.
// After Close it fails with ErrClosed.
func (s *Store) Measure() (Space, error) {
	sp, err := s.space()
	if err != nil {
		return Space{}, closed(err)
	}
	return Space{FileBytes: sp.FileBytes, FreeBytes: sp.FreeBytes, InUseBytes: sp.FileBytes - sp.FreeBytes}, nil
}

// nearlyFull reports the bytes in use past 90 % of the ceiling.
func (s *Store) nearlyFull() (bool, error) {
	sp, err := s.Measure()
	return sp.InUseBytes*10 > s.maxBytes*9, err
}
