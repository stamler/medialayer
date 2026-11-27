package core

import "syscall"

func statCtime(s *syscall.Stat_t) (int64, int64) { return int64(s.Ctim.Sec), int64(s.Ctim.Nsec) }
