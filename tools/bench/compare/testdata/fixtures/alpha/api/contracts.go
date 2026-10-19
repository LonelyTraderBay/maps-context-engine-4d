package api

type Result struct { Value int; Err error }
type Handler func(string) (Result, error)
