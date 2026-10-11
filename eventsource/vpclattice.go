package eventsource

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"
)

// VPCLatticeEvent is the request a VPC Lattice target group sends to a
// function with event structure version 1. Its keys are snake_case.
type VPCLatticeEvent struct {
	RawPath               string            `json:"raw_path"`
	Method                string            `json:"method"`
	Headers               map[string]string `json:"headers"`
	QueryStringParameters map[string]string `json:"query_string_parameters"`
	Body                  string            `json:"body"`
	IsBase64Encoded       bool              `json:"is_base64_encoded"`
}

// VPCLatticeEventV2 is the request a VPC Lattice target group sends to a
// function with event structure version 2.
type VPCLatticeEventV2 struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	Method  string `json:"method"`
	// Headers holds every value of each header. VPC Lattice sends them as
	// arrays; a plain string value decodes as a single value.
	Headers map[string]StringValues `json:"headers"`
	// QueryStringParameters holds every value of each parameter, as Headers.
	QueryStringParameters map[string]StringValues  `json:"queryStringParameters,omitempty"`
	Body                  string                   `json:"body,omitempty"`
	IsBase64Encoded       bool                     `json:"isBase64Encoded"`
	RequestContext        VPCLatticeRequestContext `json:"requestContext"`
}

// VPCLatticeRequestContext describes where a version 2 request comes from.
type VPCLatticeRequestContext struct {
	ServiceNetworkARN string                    `json:"serviceNetworkArn"`
	ServiceARN        string                    `json:"serviceArn"`
	TargetGroupARN    string                    `json:"targetGroupArn"`
	Identity          VPCLatticeRequestIdentity `json:"identity"`
	Region            string                    `json:"region"`
	// TimeEpoch is the request time in microseconds since the Unix epoch.
	// VPC Lattice sends it as a string; a number decodes too.
	TimeEpoch EpochMicros `json:"timeEpoch"`
}

// VPCLatticeRequestIdentity is the caller of a version 2 request, as far as
// VPC Lattice authenticated it.
type VPCLatticeRequestIdentity struct {
	SourceVPCARN   string `json:"sourceVpcArn,omitempty"`
	Type           string `json:"type,omitempty"`
	Principal      string `json:"principal,omitempty"`
	PrincipalOrgID string `json:"principalOrgID,omitempty"`
	SessionName    string `json:"sessionName,omitempty"`
	X509SubjectCN  string `json:"X509SubjectCn,omitempty"`
	X509IssuerOU   string `json:"X509IssuerOu,omitempty"`
	X509SANDNS     string `json:"x509SanDns,omitempty"`
	X509SANURI     string `json:"X509SanUri,omitempty"`
	X509SANNameCN  string `json:"X509SanNameCn,omitempty"`
}

// StringValues holds the values of a header or a query string parameter.
// It decodes from a JSON array of strings or from a single string, because
// VPC Lattice documents arrays while some payloads carry plain strings.
type StringValues []string

// UnmarshalJSON decodes an array of strings, a string or null.
func (s *StringValues) UnmarshalJSON(data []byte) error {
	value := jsontext.Value(data)
	switch value.Kind() {
	case '"':
		var one string
		if err := json.Unmarshal(data, &one); err != nil {
			return fmt.Errorf("decode a string value: %w", err)
		}
		*s = StringValues{one}
		return nil
	case 'n':
		*s = nil
		return nil
	default:
		var many []string
		if err := json.Unmarshal(data, &many); err != nil {
			return fmt.Errorf("decode string values: %w", err)
		}
		*s = many
		return nil
	}
}

// EpochMicros is a time in microseconds since the Unix epoch that decodes
// from a JSON number or from a string holding one.
type EpochMicros int64

// UnmarshalJSON decodes a number, a string holding a number, or null.
func (e *EpochMicros) UnmarshalJSON(data []byte) error {
	value := jsontext.Value(data)
	raw := string(data)
	switch value.Kind() {
	case '"':
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("decode the epoch: %w", err)
		}
	case 'n':
		*e = 0
		return nil
	default:
	}
	micros, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("decode the epoch %q: %w", raw, err)
	}
	*e = EpochMicros(micros)
	return nil
}

// Time returns the instant as a time.Time in UTC.
func (e EpochMicros) Time() time.Time {
	return time.UnixMicro(int64(e)).UTC()
}
