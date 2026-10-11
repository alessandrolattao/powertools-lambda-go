package eventsource

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// ErrInvalidNumber reports a DynamoDB number that is not a decimal number.
var ErrInvalidNumber = errors.New("invalid DynamoDB number")

// Number is a DynamoDB number kept as its decimal text, so that the 38
// digits DynamoDB stores survive: Powertools Python returns a Decimal,
// float64 would round. It marshals to JSON as a number.
type Number string

// String returns the decimal text.
func (n Number) String() string { return string(n) }

// Int64 returns the number as an int64; it fails on a fraction or an
// overflow.
func (n Number) Int64() (int64, error) {
	v, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q as int64: %w", ErrInvalidNumber, string(n), err)
	}
	return v, nil
}

// Float64 returns the number as a float64, rounded to the nearest one.
func (n Number) Float64() (float64, error) {
	v, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q as float64: %w", ErrInvalidNumber, string(n), err)
	}
	return v, nil
}

// MarshalJSON writes the number as a JSON number, exactly.
func (n Number) MarshalJSON() ([]byte, error) {
	normalized, err := normalizeNumber(string(n))
	if err != nil {
		return nil, err
	}
	return []byte(normalized), nil
}

// UnmarshalJSON reads a JSON number, or a string holding one.
func (n *Number) UnmarshalJSON(data []byte) error {
	text := string(data)
	if jsontext.Value(data).Kind() == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("decode a number: %w", err)
		}
	}
	normalized, err := normalizeNumber(text)
	if err != nil {
		return err
	}
	*n = Number(normalized)
	return nil
}

// normalizeNumber turns DynamoDB's decimal text into a JSON number. Like
// Powertools Python, "" and "." are zero; a leading "+" and a dot with no
// digit on one side are accepted.
func normalizeNumber(text string) (string, error) {
	s := strings.TrimSpace(text)
	if s == "" || s == "." {
		return "0", nil
	}
	sign := ""
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		sign, s = "-", s[1:]
	default:
	}
	if strings.HasPrefix(s, ".") {
		s = "0" + s
	}
	if mantissa, exponent, ok := strings.Cut(s, "e"); ok {
		s = strings.TrimSuffix(mantissa, ".") + "e" + exponent
	} else if mantissa, exponent, ok := strings.Cut(s, "E"); ok {
		s = strings.TrimSuffix(mantissa, ".") + "E" + exponent
	} else {
		s = strings.TrimSuffix(s, ".")
	}
	s = sign + s
	if v := jsontext.Value(s); v.Kind() != '0' || !v.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidNumber, text)
	}
	return s, nil
}

// DynamoDBValue converts an attribute value of a stream record into a Go
// value, the way Powertools Python's TypeDeserializer does: NULL is nil,
// BOOL a bool, N a Number, S a string, B a []byte, SS a []string, NS a
// []Number, BS a [][]byte, L a []any and M a map[string]any.
//
// Two differences from Python: binary values are bytes (Python keeps the
// base64 text) and sets are slices (Python builds a set).
func DynamoDBValue(av events.DynamoDBAttributeValue) (any, error) {
	switch av.DataType() {
	case events.DataTypeNull:
		return nil, nil //nolint:nilnil // a DynamoDB NULL is a valid nil value, not a missing one
	case events.DataTypeBoolean:
		return av.Boolean(), nil
	case events.DataTypeNumber:
		return dynamoDBNumber(av.Number())
	case events.DataTypeString:
		return av.String(), nil
	case events.DataTypeBinary:
		return av.Binary(), nil
	case events.DataTypeStringSet:
		return av.StringSet(), nil
	case events.DataTypeNumberSet:
		set := av.NumberSet()
		numbers := make([]Number, 0, len(set))
		for _, text := range set {
			n, err := dynamoDBNumber(text)
			if err != nil {
				return nil, err
			}
			numbers = append(numbers, n)
		}
		return numbers, nil
	case events.DataTypeBinarySet:
		return av.BinarySet(), nil
	case events.DataTypeList:
		list := av.List()
		values := make([]any, 0, len(list))
		for i, item := range list {
			v, err := DynamoDBValue(item)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			values = append(values, v)
		}
		return values, nil
	case events.DataTypeMap:
		return DynamoDBImage(av.Map())
	default:
		return nil, nil //nolint:nilnil // an attribute value with no type decodes as null, as JSON null does
	}
}

func dynamoDBNumber(text string) (Number, error) {
	normalized, err := normalizeNumber(text)
	if err != nil {
		return "", err
	}
	return Number(normalized), nil
}

// DynamoDBImage converts a stream record image (Keys, NewImage, OldImage)
// into a map of Go values; see DynamoDBValue. A nil image gives an empty
// map, as Powertools Python gives an empty dict.
func DynamoDBImage(image map[string]events.DynamoDBAttributeValue) (map[string]any, error) {
	out := make(map[string]any, len(image))
	for name, av := range image {
		v, err := DynamoDBValue(av)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// DynamoDBImageJSON renders a stream record image as a JSON object: numbers
// as JSON numbers with every digit, binary values as base64 strings, sets
// as arrays. It is the bridge to typed structs.
func DynamoDBImageJSON(image map[string]events.DynamoDBAttributeValue) (jsontext.Value, error) {
	values, err := DynamoDBImage(image)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(values, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode the image: %w", err)
	}
	return raw, nil
}

// DecodeDynamoDBImage decodes a stream record image into a T through its
// JSON rendering (DynamoDBImageJSON): fields match the attribute names with
// their json tags.
func DecodeDynamoDBImage[T any](image map[string]events.DynamoDBAttributeValue) (T, error) {
	var out T
	raw, err := DynamoDBImageJSON(image)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode the image into %T: %w", out, err)
	}
	return out, nil
}
