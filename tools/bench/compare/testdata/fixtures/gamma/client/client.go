package client

type Request struct { Query string; Limit int }
type Response struct { IDs []string; Pending bool }
