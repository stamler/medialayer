package core

import "syscall"

func statCtime(s *syscall.Stat_t) (int64, int64) { return s.Ctimespec.Sec, s.Ctimespec.Nsec }
