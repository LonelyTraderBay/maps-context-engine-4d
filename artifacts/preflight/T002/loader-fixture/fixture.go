package fixture

type Marker struct {
	Value int
}

func NewMarker(value int) Marker {
	return Marker{Value: value}
}
