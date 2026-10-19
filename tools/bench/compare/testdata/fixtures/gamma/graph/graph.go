package graph

type Node struct { ID string }
type Graph struct { edges map[string][]string }
func New() *Graph { return &Graph{edges: map[string][]string{}} }
func (g *Graph) AddEdge(from, to string) { g.edges[from] = append(g.edges[from], to) }
func (g *Graph) Neighbors(id string) []string { return append([]string(nil), g.edges[id]...) }
