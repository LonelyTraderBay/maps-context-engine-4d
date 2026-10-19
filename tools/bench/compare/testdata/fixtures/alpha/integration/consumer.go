package integration

import (
	"context"
	"example.com/synthetic/alpha/service"
)

func Run(p service.Processor, item service.Item) (service.Item, error) { return p.Process(context.Background(), item) }
