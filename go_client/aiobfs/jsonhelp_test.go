package aiobfs

import "encoding/json"

func jsonMarshalForTest(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshalForTest(b []byte, v any) error { return json.Unmarshal(b, v) }
