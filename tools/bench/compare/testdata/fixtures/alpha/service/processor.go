package service

import "context"

type Item struct { ID string; Value int }
type Processor interface { Process(context.Context, Item) (Item, error) }
type Worker struct{}
func (Worker) Process(_ context.Context, item Item) (Item, error) { return item, nil }
