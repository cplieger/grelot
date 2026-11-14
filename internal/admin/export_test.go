//go:build !windows

package admin

// AnswerOnly makes s answer only uid.
func (s *Server) AnswerOnly(uid int) { s.uid = uid }
