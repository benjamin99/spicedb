#!/bin/bash

./buf.gen.yaml

# use sed to modify the DecodedCaveat_Cel to ensure the deterministic output of the serialization
sed -i '' 's/proto.Marshal(m.Cel)/proto.MarshalOptions{Deterministic: true}.Marshal(m.Cel)/g' pkg/proto/impl/v1/impl_vtproto.pb.go
