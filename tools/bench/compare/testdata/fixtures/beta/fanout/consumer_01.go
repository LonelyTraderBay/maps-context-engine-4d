package fanout

import "example.com/synthetic/beta/store"

var _ store.Store = (*store.MemoryStore)(nil)

func lenStore(value string, salt int) int { return len(value) + salt }
func Use01(value string) int { return lenStore(value, 1) }
