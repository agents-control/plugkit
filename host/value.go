package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// ErrPrecision refuses a JSON number that a double cannot hold exactly; such numbers must travel as strings.
var ErrPrecision = errors.New("a number has more precision than the protocol's doubles keep; send it as a string")

// ToValue converts JSON into a protobuf Value, nil for empty input, refusing numbers a double would round.
func ToValue(raw json.RawMessage) (*structpb.Value, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if !fitsDoubles(doc) {
		return nil, ErrPrecision
	}
	v := &structpb.Value{}
	if err := v.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return v, nil
}

// FromValue renders a protobuf Value as JSON, nil for a nil Value.
func FromValue(v *structpb.Value) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return v.MarshalJSON()
}

func fitsDoubles(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			if !fitsDoubles(e) {
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !fitsDoubles(e) {
				return false
			}
		}
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return false
		}
		exact, ok1 := new(big.Rat).SetString(string(x))
		double, ok2 := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
		return ok1 && ok2 && exact.Cmp(double) == 0
	}
	return true
}

// Unavailable reports whether err means the plugin could not serve the call, so it may be retried.
// Domain failures are data in each port's reply, never gRPC errors.
func Unavailable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted, codes.Canceled:
		return true
	}
	return false
}
