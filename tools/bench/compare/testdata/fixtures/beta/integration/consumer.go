package integration

import "example.com/synthetic/beta/store"

func Save(s store.Store, key, value string) { s.Put(key, value) }
