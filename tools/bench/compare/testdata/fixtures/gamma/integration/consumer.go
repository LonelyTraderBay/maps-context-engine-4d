package integration

import (
	"example.com/synthetic/gamma/client"
	"example.com/synthetic/gamma/search"
)

func Query(i search.Index, request client.Request) client.Response { return client.Response{IDs: i.Lookup(request.Query)} }
