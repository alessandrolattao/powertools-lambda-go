package eventsource

import (
	"encoding/json/v2"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

// ConfigEvent is events.ConfigEvent (an AWS Config custom rule evaluation)
// with the evaluation mode, and helpers that decode its JSON-encoded
// members.
type ConfigEvent struct {
	events.ConfigEvent

	// EvaluationMode is DETECTIVE or PROACTIVE.
	EvaluationMode string `json:"evaluationMode,omitempty"`
}

// The message types of an AWS Config invoking event.
const (
	ConfigConfigurationItemChangeNotification          = "ConfigurationItemChangeNotification"
	ConfigOversizedConfigurationItemChangeNotification = "OversizedConfigurationItemChangeNotification"
	ConfigScheduledNotification                        = "ScheduledNotification"
)

// ConfigInvokingEvent is what triggered an AWS Config rule evaluation: a
// *ConfigConfigurationChanged, a *ConfigOversizedConfiguration or a
// *ConfigScheduled. Tell them apart with a type switch.
//
//sumtype:decl
type ConfigInvokingEvent interface {
	// MessageType returns the message type of the invoking event.
	MessageType() string
	sealed()
}

// ConfigConfigurationChanged is the invoking event of a configuration
// change: the resource's configuration item and what changed.
type ConfigConfigurationChanged struct {
	ConfigurationItemDiff    map[string]any          `json:"configurationItemDiff"`
	ConfigurationItem        ConfigConfigurationItem `json:"configurationItem"`
	NotificationCreationTime string                  `json:"notificationCreationTime"`
	Type                     string                  `json:"messageType"`
	RecordVersion            string                  `json:"recordVersion"`
}

// MessageType returns ConfigConfigurationItemChangeNotification.
func (e *ConfigConfigurationChanged) MessageType() string { return e.Type }

func (*ConfigConfigurationChanged) sealed() {}

// ConfigConfigurationItem is the configuration of a resource as AWS Config
// recorded it.
type ConfigConfigurationItem struct {
	RelatedEvents                []any          `json:"relatedEvents"`
	Relationships                []any          `json:"relationships"`
	Configuration                map[string]any `json:"configuration"`
	SupplementaryConfiguration   map[string]any `json:"supplementaryConfiguration"`
	Tags                         map[string]any `json:"tags"`
	ConfigurationItemVersion     string         `json:"configurationItemVersion"`
	ConfigurationItemCaptureTime string         `json:"configurationItemCaptureTime"`
	ConfigurationStateID         int64          `json:"configurationStateId"`
	AWSAccountID                 string         `json:"awsAccountId"`
	ConfigurationItemStatus      string         `json:"configurationItemStatus"`
	ResourceType                 string         `json:"resourceType"`
	ResourceID                   string         `json:"resourceId"`
	ResourceName                 string         `json:"resourceName"`
	ARN                          string         `json:"ARN"`
	AWSRegion                    string         `json:"awsRegion"`
	AvailabilityZone             string         `json:"availabilityZone"`
	ConfigurationStateMD5Hash    string         `json:"configurationStateMd5Hash"`
	ResourceCreationTime         string         `json:"resourceCreationTime"`
}

// ConfigOversizedConfiguration is the invoking event of a configuration
// change too large for the event: a summary of the configuration item.
type ConfigOversizedConfiguration struct {
	ConfigurationItemSummary ConfigConfigurationItemSummary `json:"configurationItemSummary"`
	NotificationCreationTime string                         `json:"notificationCreationTime"`
	Type                     string                         `json:"messageType"`
	RecordVersion            string                         `json:"recordVersion"`
}

// MessageType returns ConfigOversizedConfigurationItemChangeNotification.
func (e *ConfigOversizedConfiguration) MessageType() string { return e.Type }

func (*ConfigOversizedConfiguration) sealed() {}

// ConfigConfigurationItemSummary summarizes an oversized configuration item.
type ConfigConfigurationItemSummary struct {
	ChangeType                   string `json:"changeType"`
	ConfigurationItemVersion     string `json:"configurationItemVersion"`
	ConfigurationItemCaptureTime string `json:"configurationItemCaptureTime"`
	ConfigurationStateID         int64  `json:"configurationStateId"`
	AWSAccountID                 string `json:"awsAccountId"`
	ConfigurationItemStatus      string `json:"configurationItemStatus"`
	ResourceType                 string `json:"resourceType"`
	ResourceID                   string `json:"resourceId"`
	ResourceName                 string `json:"resourceName"`
	ARN                          string `json:"ARN"`
	AWSRegion                    string `json:"awsRegion"`
	AvailabilityZone             string `json:"availabilityZone"`
	ConfigurationStateMD5Hash    string `json:"configurationStateMd5Hash"`
	ResourceCreationTime         string `json:"resourceCreationTime"`
}

// ConfigScheduled is the invoking event of a periodic evaluation.
type ConfigScheduled struct {
	AWSAccountID             string `json:"awsAccountId"`
	NotificationCreationTime string `json:"notificationCreationTime"`
	Type                     string `json:"messageType"`
	RecordVersion            string `json:"recordVersion"`
}

// MessageType returns ConfigScheduledNotification.
func (e *ConfigScheduled) MessageType() string { return e.Type }

func (*ConfigScheduled) sealed() {}

// DecodeInvokingEvent decodes the invoking event by its message type. Any
// type other than the scheduled and oversized notifications decodes as a
// configuration change, as Powertools Python does.
func (e ConfigEvent) DecodeInvokingEvent() (ConfigInvokingEvent, error) {
	var header struct {
		MessageType string `json:"messageType"`
	}
	raw := []byte(e.InvokingEvent)
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("decode the invoking event: %w", err)
	}
	var out ConfigInvokingEvent
	switch header.MessageType {
	case ConfigScheduledNotification:
		out = new(ConfigScheduled)
	case ConfigOversizedConfigurationItemChangeNotification:
		out = new(ConfigOversizedConfiguration)
	default:
		out = new(ConfigConfigurationChanged)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("decode the %s invoking event: %w", header.MessageType, err)
	}
	return out, nil
}

// DecodeRuleParameters decodes the rule parameters into a T. Missing
// parameters give the zero T.
func (e ConfigEvent) DecodeRuleParameters[T any]() (T, error) {
	var out T
	if e.RuleParameters == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(e.RuleParameters), &out); err != nil {
		return out, fmt.Errorf("decode the rule parameters into %T: %w", out, err)
	}
	return out, nil
}
