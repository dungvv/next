// Package jsonav converts arbitrary JSON values to/from DynamoDB
// AttributeValues, matching serde_dynamo's representation: objects -> M,
// arrays -> L, strings -> S, numbers -> N, bools -> BOOL, null/absent -> NULL.
package jsonav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// FromJSON converts a raw JSON value into an AttributeValue. Nil or empty
// input maps to NULL (serde_dynamo Option::None).
func FromJSON(raw json.RawMessage) (types.AttributeValue, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &types.AttributeValueMemberNULL{Value: true}, nil
	}
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid extension_data JSON: %w", err)
	}
	return fromValue(v)
}

// ToJSON converts an AttributeValue back to raw JSON. Nil or NULL maps to nil
// (JSON null).
func ToJSON(av types.AttributeValue) (json.RawMessage, error) {
	v, err := toValue(av)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func fromValue(v interface{}) (types.AttributeValue, error) {
	switch t := v.(type) {
	case nil:
		return &types.AttributeValueMemberNULL{Value: true}, nil
	case bool:
		return &types.AttributeValueMemberBOOL{Value: t}, nil
	case json.Number:
		return &types.AttributeValueMemberN{Value: t.String()}, nil
	case float64:
		return &types.AttributeValueMemberN{Value: formatFloat(t)}, nil
	case string:
		return &types.AttributeValueMemberS{Value: t}, nil
	case []interface{}:
		items := make([]types.AttributeValue, 0, len(t))
		for _, e := range t {
			av, err := fromValue(e)
			if err != nil {
				return nil, err
			}
			items = append(items, av)
		}
		return &types.AttributeValueMemberL{Value: items}, nil
	case map[string]interface{}:
		m := make(map[string]types.AttributeValue, len(t))
		for k, e := range t {
			av, err := fromValue(e)
			if err != nil {
				return nil, err
			}
			m[k] = av
		}
		return &types.AttributeValueMemberM{Value: m}, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", v)
	}
}

func toValue(av types.AttributeValue) (interface{}, error) {
	switch t := av.(type) {
	case nil:
		return nil, nil
	case *types.AttributeValueMemberNULL:
		return nil, nil
	case *types.AttributeValueMemberBOOL:
		return t.Value, nil
	case *types.AttributeValueMemberN:
		return json.Number(t.Value), nil
	case *types.AttributeValueMemberS:
		return t.Value, nil
	case *types.AttributeValueMemberL:
		out := make([]interface{}, 0, len(t.Value))
		for _, e := range t.Value {
			v, err := toValue(e)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case *types.AttributeValueMemberM:
		keys := make([]string, 0, len(t.Value))
		for k := range t.Value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]interface{}, len(t.Value))
		for _, k := range keys {
			v, err := toValue(t.Value[k])
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case *types.AttributeValueMemberNS:
		out := make([]interface{}, 0, len(t.Value))
		for _, n := range t.Value {
			out = append(out, json.Number(n))
		}
		return out, nil
	case *types.AttributeValueMemberSS:
		out := make([]interface{}, 0, len(t.Value))
		for _, s := range t.Value {
			out = append(out, s)
		}
		return out, nil
	case *types.AttributeValueMemberBS:
		out := make([]interface{}, 0, len(t.Value))
		for _, b := range t.Value {
			out = append(out, b)
		}
		return out, nil
	case *types.AttributeValueMemberB:
		return t.Value, nil
	default:
		return nil, fmt.Errorf("unsupported attribute value %T", av)
	}
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
