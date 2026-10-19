package search

type Index interface { Lookup(string) []string }
type MemoryIndex struct { terms map[string][]string }
func (i *MemoryIndex) Lookup(term string) []string { return append([]string(nil), i.terms[term]...) }
