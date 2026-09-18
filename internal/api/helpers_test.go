package api

import (
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
)

type structpbStruct = structpb.Struct

func newStruct(m map[string]any) (*structpb.Struct, error) { return structpb.NewStruct(m) }

func errorsAs(err error, target **connect.Error) bool { return errors.As(err, target) }
