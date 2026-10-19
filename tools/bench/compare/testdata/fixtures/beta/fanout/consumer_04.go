package fanout

import "example.com/synthetic/beta/store"

var _ store.Store = (*store.MemoryStore)(nil)

func Use04(value string) int { return lenStore(value, 4) }
