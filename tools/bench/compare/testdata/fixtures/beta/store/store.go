package store

type Store interface { Get(string) (string, bool); Put(string, string) }
type MemoryStore struct { values map[string]string }
func NewMemoryStore() *MemoryStore { return &MemoryStore{values: map[string]string{}} }
func (s *MemoryStore) Get(key string) (string, bool) { value, ok := s.values[key]; return value, ok }
func (s *MemoryStore) Put(key, value string) { s.values[key] = value }
