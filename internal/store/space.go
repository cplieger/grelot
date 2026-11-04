package store

import "context"

// Space is the store file's size and how much of it holds data. bbolt
// reuses freed pages and never shrinks the file, so FileBytes stays near
// its high-water mark and only InUseBytes falls when rows expire.
type Space struct {
	FileBytes  int64
	FreeBytes  int64
	InUseBytes int64
}

// Measure reads the file's space and keeps the bytes in use for the post
// check. It walks every table's pages, so the server runs it once a
// minute, not per post.
func (s *Store) Measure(ctx context.Context) (Space, error) {
	st, err := s.db.Stats(ctx)
	if err != nil {
		return Space{}, err
	}
	sp := Space{FileBytes: st.FileBytes, FreeBytes: st.FreeBytes, InUseBytes: st.FileBytes - st.FreeBytes}
	s.inUse.Store(sp.InUseBytes)
	return sp, nil
}

// nearlyFull reports the last measure past 90 % of the ceiling.
func (s *Store) nearlyFull() bool { return s.inUse.Load()*10 > s.maxBytes*9 }
