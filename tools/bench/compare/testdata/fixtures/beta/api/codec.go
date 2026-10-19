package api

type Envelope struct { Version int; Payload []byte }
type Decoder interface { Decode([]byte) (Envelope, error) }
