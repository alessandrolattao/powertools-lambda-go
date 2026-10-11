package eventsource

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"
	"uuid"

	"github.com/aws/aws-lambda-go/events"
)

// AppSyncResolverEvent is what AppSync sends to a direct Lambda resolver
// (and to the Amplify GraphQL Transformer @function directive).
type AppSyncResolverEvent struct {
	Arguments map[string]any   `json:"arguments"`
	Identity  *AppSyncIdentity `json:"identity"`
	Source    map[string]any   `json:"source"`
	Request   AppSyncRequest   `json:"request"`
	Prev      *AppSyncPrev     `json:"prev"`
	Info      AppSyncInfo      `json:"info"`
	Stash     map[string]any   `json:"stash"`

	// FieldName, ParentTypeName and TypeName are where Amplify GraphQL
	// Transformer payloads, which have no info, name the field; read them
	// through Field and ParentType.
	FieldName      string `json:"fieldName,omitempty"`
	ParentTypeName string `json:"parentTypeName,omitempty"`
	TypeName       string `json:"typeName,omitempty"`
}

// Field returns the name of the field being resolved, from info or, for
// Amplify payloads, from the top-level fieldName.
func (e AppSyncResolverEvent) Field() string {
	if e.Info.FieldName != "" {
		return e.Info.FieldName
	}
	return e.FieldName
}

// ParentType returns the name of the type the field belongs to, from info
// or, for Amplify payloads, from parentTypeName or typeName.
func (e AppSyncResolverEvent) ParentType() string {
	switch {
	case e.Info.ParentTypeName != "":
		return e.Info.ParentTypeName
	case e.ParentTypeName != "":
		return e.ParentTypeName
	default:
		return e.TypeName
	}
}

// AppSyncInfo describes the GraphQL request being resolved.
type AppSyncInfo struct {
	FieldName           string         `json:"fieldName"`
	ParentTypeName      string         `json:"parentTypeName"`
	Variables           map[string]any `json:"variables"`
	SelectionSetList    []string       `json:"selectionSetList"`
	SelectionSetGraphQL string         `json:"selectionSetGraphQL,omitempty"`
}

// AppSyncRequest is the HTTP request AppSync received.
type AppSyncRequest struct {
	// Headers are lower-cased by AppSync; LookupHeader matches them
	// case-insensitively anyway.
	Headers    map[string]string `json:"headers"`
	DomainName string            `json:"domainName,omitempty"`
}

// AppSyncPrev holds the result of the previous function of a pipeline
// resolver.
type AppSyncPrev struct {
	Result any `json:"result"`
}

// UnmarshalJSON decodes the prev object. A prev that is not an object (some
// AppSync Events payloads carry a placeholder string) is left empty rather
// than failing the whole event.
func (p *AppSyncPrev) UnmarshalJSON(data []byte) error {
	if jsontext.Value(data).Kind() != '{' {
		*p = AppSyncPrev{}
		return nil
	}
	var raw struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode prev: %w", err)
	}
	p.Result = raw.Result
	return nil
}

// AppSyncAuthorization is how the caller of an AppSync request was
// authorized.
type AppSyncAuthorization string

// The authorization modes, as AppSyncIdentity.Authorization tells them
// apart. An API key request has no identity at all.
const (
	AppSyncAuthorizationIAM     AppSyncAuthorization = "AWS_IAM"
	AppSyncAuthorizationCognito AppSyncAuthorization = "AMAZON_COGNITO_USER_POOLS"
	AppSyncAuthorizationOIDC    AppSyncAuthorization = "OPENID_CONNECT"
	AppSyncAuthorizationLambda  AppSyncAuthorization = "AWS_LAMBDA"
)

// AppSyncIdentity is the caller of an AppSync request. AppSync sends a
// different shape per authorization mode; this struct holds them all and
// Authorization tells which one arrived. The IAM members come from the
// embedded events.AppSyncIAMIdentity.
type AppSyncIdentity struct {
	events.AppSyncIAMIdentity

	// Sub, Issuer, Claims: Cognito user pools and OpenID Connect.
	Sub    string         `json:"sub,omitempty"`
	Issuer string         `json:"issuer,omitempty"`
	Claims map[string]any `json:"claims,omitempty"`
	// DefaultAuthStrategy and Groups: Cognito user pools only.
	DefaultAuthStrategy string   `json:"defaultAuthStrategy,omitempty"`
	Groups              []string `json:"groups,omitempty"`
	// ResolverContext: Lambda authorization, what the authorizer returned.
	ResolverContext map[string]any `json:"resolverContext,omitempty"`
}

// Authorization tells how the caller was authorized. Powertools Python only
// tells Cognito (any identity with a sub) from IAM; this also recognizes
// OpenID Connect and Lambda identities.
func (i AppSyncIdentity) Authorization() AppSyncAuthorization {
	switch {
	case i.ResolverContext != nil:
		return AppSyncAuthorizationLambda
	case i.Sub != "" && (i.DefaultAuthStrategy != "" || i.Username != ""):
		return AppSyncAuthorizationCognito
	case i.Sub != "":
		return AppSyncAuthorizationOIDC
	default:
		return AppSyncAuthorizationIAM
	}
}

// AppSyncEventsEvent is what AppSync Events sends to a Lambda handler of a
// channel namespace, for a publish or a subscribe.
type AppSyncEventsEvent struct {
	Identity  *AppSyncIdentity  `json:"identity"`
	Request   AppSyncRequest    `json:"request"`
	Info      AppSyncEventsInfo `json:"info"`
	Prev      *AppSyncPrev      `json:"prev"`
	Stash     map[string]any    `json:"stash"`
	Events    []AppSyncEvent    `json:"events"`
	OutErrors []any             `json:"outErrors"`
}

// AppSyncEvent is one published event.
type AppSyncEvent struct {
	ID      string `json:"id"`
	Payload any    `json:"payload"`
}

// AppSyncEventsInfo describes the operation and its channel.
type AppSyncEventsInfo struct {
	Channel          AppSyncChannel          `json:"channel"`
	ChannelNamespace AppSyncChannelNamespace `json:"channelNamespace"`
	// Operation is PUBLISH or SUBSCRIBE.
	Operation string `json:"operation"`
}

// AppSyncChannel is the channel of an operation.
type AppSyncChannel struct {
	Path     string   `json:"path"`
	Segments []string `json:"segments"`
}

// AppSyncChannelNamespace is the namespace of the channel.
type AppSyncChannelNamespace struct {
	Name string `json:"name"`
}

// The AppSync Events operations.
const (
	AppSyncEventsPublish   = "PUBLISH"
	AppSyncEventsSubscribe = "SUBSCRIBE"
)

// MakeID returns a new random identifier for an AppSync ID scalar.
func MakeID() string {
	return uuid.NewV4().String()
}

// AWSDate returns today's date as an AppSync AWSDate scalar, in UTC shifted
// by offsetHours, with the zone suffix Powertools Python writes ("Z" or
// "+05:00:00").
func AWSDate(offsetHours int) string {
	return formatAWSTime(time.Now(), "2006-01-02", offsetHours)
}

// AWSTime returns the current time as an AppSync AWSTime scalar, with
// millisecond precision, shifted by offsetHours.
func AWSTime(offsetHours int) string {
	return formatAWSTime(time.Now(), "15:04:05.000", offsetHours)
}

// AWSDateTime returns the current instant as an AppSync AWSDateTime scalar,
// with millisecond precision, shifted by offsetHours.
func AWSDateTime(offsetHours int) string {
	return formatAWSTime(time.Now(), "2006-01-02T15:04:05.000", offsetHours)
}

// AWSTimestamp returns the current time as an AppSync AWSTimestamp scalar:
// whole seconds since the Unix epoch.
func AWSTimestamp() int64 {
	return time.Now().Unix()
}

func formatAWSTime(now time.Time, layout string, offsetHours int) string {
	shifted := now.UTC().Add(time.Duration(offsetHours) * time.Hour).Format(layout)
	if offsetHours == 0 {
		return shifted + "Z"
	}
	sign := "+"
	if offsetHours < 0 {
		sign = "-"
		offsetHours = -offsetHours
	}
	hours := strconv.Itoa(offsetHours)
	if len(hours) < 2 {
		hours = "0" + hours
	}
	return shifted + sign + hours + ":00:00"
}
